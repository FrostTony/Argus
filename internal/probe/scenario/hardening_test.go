package scenario

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func buildProbe(pc config.Probe, options string) (*Prober, error) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		return nil, err
	}
	pc.Type, pc.Options = config.ScenarioType, *node.Content[0]
	p, err := New(pc)
	if err != nil {
		return nil, err
	}
	return p.(*Prober), nil
}

// A secret goes out escaped when it sits in a query or a path, and is quoted
// back escaped; it must be masked in that form too.
func TestSecretIsMaskedInEveryFormItTakes(t *testing.T) {
	t.Setenv("ARGUS_TEST_PW", "p@ss w/rd!")
	t.Setenv("ARGUS_TEST_USER", "admin")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echoes the request line back, escaped as it arrived.
		http.Error(w, "refused "+r.URL.RawQuery+" "+r.URL.EscapedPath(), http.StatusForbidden)
	}))
	defer srv.Close()

	p := newProber(t, `
secrets:
  user: {env: ARGUS_TEST_USER}
  pw:   {env: ARGUS_TEST_PW}
steps:
  - name: login
    http:
      path: "/login/${secret.pw}"
      query: {u: "${secret.user}", p: "${secret.pw}", both: "${secret.user}-${secret.pw}"}
    expect: [{status_code: [403]}]
    # Fails quoting the body, which quotes the request.
    extract: {token: {regex: "^welcome"}}
  - {name: never, http: {path: /x}}
`)
	res := p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
	if res.OK() {
		t.Fatal("want a failure")
	}
	rep := p.Reports()[0]
	if !strings.Contains(res.Err.Error(), "${secret.pw}") || !strings.Contains(res.Err.Error(), "${secret.user}-") {
		t.Errorf("err = %q: want the echoed secrets masked, not absent", res.Err)
	}
	for _, text := range []string{res.Err.Error(), rep.Error, rep.Steps[0].Error} {
		for _, leak := range []string{"p@ss", "p%40ss", "w%2Frd", "w/rd"} {
			if strings.Contains(text, leak) {
				t.Errorf("%q leaks %q", text, leak)
			}
		}
	}
}

// The overlap: one secret inside another must not leave the rest showing.
func TestOverlappingSecretsAreMaskedWhole(t *testing.T) {
	s := newScope(nil, map[string]string{"user": "admin", "pw": "admin-Xyz"})
	if got := s.redact("sent admin-Xyz as admin"); got != "sent ${secret.pw} as ${secret.user}" {
		t.Errorf("redact = %q", got)
	}
}

// The URL net/http wraps a transport error in carries the query, and with it
// whatever secret is there.
func TestTransportErrorCarriesNoURL(t *testing.T) {
	t.Setenv("ARGUS_TEST_PW", "hunter2")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens: the connection is refused

	p := newProber(t, `
secrets: {pw: {env: ARGUS_TEST_PW}}
steps:
  - {name: login, http: {path: /login, query: {p: "${secret.pw}"}}}
`)
	u, _ := url.Parse("http://" + addr + "/")
	res := p.Probe(context.Background(), probe.Request{Target: probe.Target{Name: "t", Host: "127.0.0.1", URL: u}},
		metrics.NewRecorder(nil))
	if probe.ReasonOf(res.Err) != probe.ReasonConnect {
		t.Errorf("reason = %s, want connect", probe.ReasonOf(res.Err))
	}
	if msg := res.Err.Error(); strings.Contains(msg, "hunter2") || strings.Contains(msg, "/login") {
		t.Errorf("err = %q", msg)
	}
}

func TestValueIsOneSegmentWhateverItHolds(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		if r.URL.Path == "/ids" {
			_, _ = io.WriteString(w, `{"dots": "..", "empty": "", "id": "a/b"}`)
		}
	}))
	defer srv.Close()

	for value, want := range map[string]string{
		"dots":  `".." cannot be a path segment`,
		"empty": "an empty value cannot be a path segment",
		"id":    "",
	} {
		paths = nil
		p := newProber(t, `
steps:
  - {name: ids, http: {path: /ids}, extract: {v: {json_path: `+value+`}}}
  - {name: del, http: {method: DELETE, path: "/videos/${var.v}/files", headers: {X-V: "${var.v}"}}}
`)
		res := p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
		mu.Lock()
		got := slices.Clone(paths)
		mu.Unlock()
		if want == "" {
			if !res.OK() || len(got) != 2 || got[1] != "/videos/a%2Fb/files" {
				t.Errorf("%s: err = %v, paths = %v: a slash is escaped within the segment", value, res.Err, got)
			}
			continue
		}
		if res.OK() || !strings.Contains(res.Err.Error(), want) || len(got) != 1 {
			t.Errorf("%s: err = %v, paths = %v: want it refused before the DELETE goes out", value, res.Err, got)
		}
	}
}

// An empty value is a value: refused only where it would change a path.
func TestEmptyValueIsFineOutsideAPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"note": ""}`)
	}))
	defer srv.Close()
	p := newProber(t, `
steps:
  - {name: a, http: {path: /x}, extract: {note: {json_path: note}}}
  - {name: b, http: {path: /y, headers: {X-Note: "${var.note}"}, json: {note: "${var.note}"}}}
`)
	if res := p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil)); !res.OK() {
		t.Error(res.Err)
	}
}

func TestReferencesStayInThePath(t *testing.T) {
	cases := map[string]string{
		`{path: "/search?q=${var.q}"}`:            "goes under query:",
		`{url: "https://${var.q}/x"}`:             "not in its host",
		`{url: "https://h.example/x?a=${var.q}"}`: "goes under query:",
	}
	for http, want := range cases {
		_, err := build(`
vars: {q: x}
steps:
  - {name: a, http: ` + http + `}`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", http, err, want)
		}
	}
}

func TestVarsArePlainValues(t *testing.T) {
	for options, want := range map[string]string{
		`{u: "${ARGUS_SURELY_UNSET}"}`: "environment variable is not set",
		`{t: "t-${run.id}"}`:           "plain value",
	} {
		_, err := build("vars: " + options + "\nsteps:\n  - {name: a, http: {path: /x}}\n")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", options, err, want)
		}
	}
}

func TestPanicIsReportedAsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	p := newProber(t, `steps: [{name: a, http: {path: /x}}]`)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must reach the runner, which isolates it")
			}
		}()
		// A nil recorder panics on the first series.
		p.Probe(context.Background(), requestFor(t, srv), nil)
	}()
	if rep := p.Reports(); len(rep) != 1 || rep[0].Success || !strings.Contains(rep[0].Error, "panicked") {
		t.Errorf("reports = %+v", rep)
	}
}

func TestTimingMustFit(t *testing.T) {
	steps := `
steps:
  - {name: wait, http: {path: /x}, until: [{status_code: [200]}], within: 10m}
cleanup:
  - {name: z, http: {path: /y}}
`
	pc := config.Probe{Name: "s", Interval: config.Duration(15 * time.Minute), Timeout: config.Duration(5 * time.Minute)}
	if _, err := buildProbe(pc, steps); err == nil || !strings.Contains(err.Error(), "within (10m0s) exceeds") {
		t.Errorf("err = %v", err)
	}
	pc.Timeout = config.Duration(15 * time.Minute)
	pc.Interval = config.Duration(15 * time.Minute)
	if _, err := buildProbe(pc, steps); err == nil || !strings.Contains(err.Error(), "cleanup_timeout") {
		t.Errorf("err = %v", err)
	}
}

func TestFilesAreTakenFromTheScenariosDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "clip.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := `steps: [{name: a, http: {path: /x, body_file: fixtures/clip.bin}}]`
	if _, err := buildProbe(config.Probe{Name: "s", Dir: dir}, options); err != nil {
		t.Errorf("relative to the scenario's file: %v", err)
	}
	if _, err := buildProbe(config.Probe{Name: "s"}, options); err == nil {
		t.Error("without a directory the path is the working directory's, where the fixture is not")
	}
}

// A 307 sends the body again, which for a file means reading it again.
func TestRedirectResendsAStreamedBody(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
		chunks []bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		chunks = append(chunks, len(r.TransferEncoding) > 0)
		mu.Unlock()
		if r.URL.Path == "/upload" {
			http.Redirect(w, r, "/stored", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"id": "1"}`)
	}))
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "clip.bin")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No vars at all: the first extract writes into a map nobody declared.
	p := newProber(t, `
steps:
  - name: upload
    http: {method: PUT, path: /upload, body_file: `+file+`}
    extract: {id: {json_path: id}}
  - name: fields
    http: {path: /form, multipart: {fields: {id: "${var.id}"}}}
`)
	res := p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
	if !res.OK() {
		t.Fatal(res.Err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 || bodies[0] != "payload" || bodies[1] != "payload" {
		t.Errorf("bodies = %q: the redirect must carry the file again", bodies)
	}
	for i, c := range chunks {
		if c {
			t.Errorf("request %d went out chunked: its length was known", i)
		}
	}
}

func TestReloadKeepsTheLastRuns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	old := newProber(t, `steps: [{name: a, http: {path: /x}}]`)
	old.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))

	next := newProber(t, `steps: [{name: b, http: {path: /y}}]`)
	next.Inherit(old)
	if rep := next.Reports(); len(rep) != 1 || rep[0].Steps[0].Name != "a" {
		t.Errorf("reports = %+v: an edited scenario keeps the account of its last run", rep)
	}

	// A run of the old prober that the reload interrupted ends after the
	// heir exists; its report is the heir's to show.
	old.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
	latest := old.Reports()[0].Started
	if rep := next.Reports(); !rep[0].Started.Equal(latest) {
		t.Errorf("heir shows the run of %s, want the interrupted one of %s", rep[0].Started, latest)
	}
	// And the heir's own run is not overwritten by an older one forwarded late.
	next.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
	own := next.Reports()[0]
	next.keep(Report{Target: own.Target, Started: own.Started.Add(-time.Hour)})
	if rep := next.Reports(); rep[0].Steps[0].Name != "b" {
		t.Errorf("an older forwarded report replaced the heir's own: %+v", rep[0])
	}
}

// A message quotes the body, escaping and cutting it as it goes: the secret
// must be masked before any of that.
func TestSecretIsMaskedHoweverTheBodyIsQuoted(t *testing.T) {
	t.Setenv("ARGUS_TEST_PW", `pa"ss-SuperSecretToken`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The secret, JSON-escaped, sits where an excerpt cuts it in two.
		_, _ = io.WriteString(w, strings.Repeat("x", 100)+`{"user":"pa\"ss-SuperSecretToken"}`)
	}))
	defer srv.Close()
	p := newProber(t, `
secrets: {pw: {env: ARGUS_TEST_PW}}
steps:
  - {name: a, http: {path: /x, headers: {X-Pw: "${secret.pw}"}}, extract: {v: {regex: "^welcome"}}}
`)
	res := p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
	for _, text := range []string{res.Err.Error(), p.Reports()[0].Error} {
		if strings.Contains(text, "SuperSecret") || strings.Contains(text, "pa\\") {
			t.Errorf("leaked: %s", text)
		}
	}
}

func TestSizeBoundsReachPastWhatIsKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 4<<20))
	}))
	defer srv.Close()
	for bound, ok := range map[string]bool{
		"max_size_bytes: 3145728": false,
		"min_size_bytes: 3145728": true,
	} {
		p := newProber(t, "steps: [{name: a, http: {path: /x}, expect: [{"+bound+"}]}]")
		if res := p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil)); res.OK() != ok {
			t.Errorf("%s on 4 MiB: ok = %v, err = %v", bound, res.OK(), res.Err)
		}
	}
}

func TestColonStaysInItsSegment(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/api/videos/")
	p := newProber(t, `
vars: {id: "urn:42"}
steps: [{name: a, http: {path: "${var.id}/files"}}]
`)
	res := p.Probe(context.Background(), probe.Request{Target: probe.Target{Name: "t", URL: u}}, metrics.NewRecorder(nil))
	if !res.OK() || got != "/api/videos/urn%3A42/files" {
		t.Errorf("err = %v, path = %q: a colon must not read as a scheme", res.Err, got)
	}
}

func TestMoreConfigurationErrors(t *testing.T) {
	for options, want := range map[string]string{
		`
vars: {u: "https://s3.example/x?sig=1"}
steps: [{name: a, http: {url: "${var.u}", query: {b: c}}}]`: "signature included",
		`
cleanup_timeout: 10s
steps: [{name: a, http: {path: /x}}]
cleanup: [{name: z, http: {path: /y}, until: [{status_code: [200]}], within: 1m}]`: "exceeds cleanup_timeout",
	} {
		if _, err := build(options); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}
