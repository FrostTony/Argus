package scenario

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// serveAt stands a server on one loopback address: 127.0.0.1 and 127.0.0.2
// are two origins without DNS.
func serveAt(t *testing.T, ip string, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// seen records what one origin received.
type seen struct {
	mu    sync.Mutex
	lines []string
}

func (s *seen) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf("%s %s key=%s auth=%s body=%s", r.Method, r.URL.Path,
		r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), b))
}

func (s *seen) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

func probeOnce(t *testing.T, p *Prober, srv *httptest.Server) probe.Result {
	t.Helper()
	return p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
}

// A redirect may lead elsewhere; what the step wrote for its target stays on it.
func TestRedirectKeepsTheStepOnItsOrigin(t *testing.T) {
	t.Setenv("ARGUS_TEST_KEY", "key-0123456789")
	t.Setenv("ARGUS_TEST_PW", "pw-0123456789")
	var other seen
	cdn := serveAt(t, "127.0.0.2", http.HandlerFunc(other.handler))
	var home seen
	target := serveAt(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			http.Redirect(w, r, cdn.URL+"/collect", http.StatusTemporaryRedirect)
		case "/download":
			http.Redirect(w, r, cdn.URL+"/bucket/1", http.StatusFound)
		case "/moved":
			http.Redirect(w, r, "/landing", http.StatusFound)
		default:
			home.handler(w, r)
		}
	}))
	secrets := "secrets: {key: {env: ARGUS_TEST_KEY}, pw: {env: ARGUS_TEST_PW}}\n"

	login := newProber(t, secrets+`steps: [{name: login, post: /login, json: {password: "${secret.pw}"}}]`)
	if res := probeOnce(t, login, target); res.OK() || !strings.Contains(res.Err.Error(), "another origin") {
		t.Errorf("a 307 off the origin: %v", res.Err)
	}
	download := newProber(t, secrets+`steps: [{name: get, get: /download, headers: {X-Api-Key: "${secret.key}"}}]`)
	if res := probeOnce(t, download, target); !res.OK() {
		t.Errorf("a plain redirect to a CDN: %v", res.Err)
	}
	if got := other.all(); strings.Contains(got, "0123456789") || !strings.Contains(got, "GET /bucket/1") {
		t.Errorf("the other origin received:\n%s", got)
	}
	// Within the origin, the step is the step.
	moved := newProber(t, secrets+`steps: [{name: get, get: /moved, headers: {X-Api-Key: "${secret.key}"}}]`)
	if res := probeOnce(t, moved, target); !res.OK() || !strings.Contains(home.all(), "GET /landing key=key-0123456789") {
		t.Errorf("a same-origin redirect lost the header: %v\n%s", res.Err, home.all())
	}
}

// An address a response hands out is taken whole; a step that sends a secret
// follows it only on the target.
func TestAddressFromAResponseGetsNoSecretElsewhere(t *testing.T) {
	t.Setenv("ARGUS_TEST_TOKEN", "token-0123456789")
	var other seen
	elsewhere := serveAt(t, "127.0.0.2", http.HandlerFunc(other.handler))
	target := serveAt(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			w.Header().Set("Location", elsewhere.URL+"/job/1")
		case "/home":
			w.Header().Set("Location", "/job/1")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	scenario := func(start string) *Prober {
		return newProber(t, `
secrets: {token: {env: ARGUS_TEST_TOKEN}}
steps:
  - {name: start, post: `+start+`, status: 202, extract: {job: {header: Location}}}
  - {name: poll, get: "${var.job}", status: 202, headers: {Authorization: "Bearer ${secret.token}"}}
`)
	}
	if res := probeOnce(t, scenario("/away"), target); res.OK() || !strings.Contains(res.Err.Error(), "not the target") {
		t.Errorf("a secret followed an address elsewhere: %v", res.Err)
	}
	if got := other.all(); got != "" {
		t.Errorf("the other origin received:\n%s", got)
	}
	if res := probeOnce(t, scenario("/home"), target); !res.OK() {
		t.Errorf("an address on the target: %v", res.Err)
	}
}

// A token a step takes from an answer is a secret once it says so.
func TestExtractedSecretIsMasked(t *testing.T) {
	const token = "session-0123456789"
	target := serveAt(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"`+token+`"}`)
		default:
			_, _ = io.WriteString(w, `{"error":"expired: `+r.Header.Get("Authorization")+`"}`)
		}
	}))
	p := newProber(t, `
steps:
  - {name: token, post: /token, body: x, extract: {token: {json_path: access_token, secret: true}}}
  - {name: me, get: /me, headers: {Authorization: "Bearer ${var.token}"}, extract: {id: {regex: '"id":"([^"]+)"'}}}
`)
	res := probeOnce(t, p, target)
	rep := p.Reports()[0]
	for _, text := range []string{res.Err.Error(), rep.Error, rep.Steps[1].Error} {
		if strings.Contains(text, "0123456789") {
			t.Errorf("the token is in the report: %s", text)
		}
	}
	if _, err := build(`steps: [{name: a, get: /x, extract: {n: {json_path: n, secret: true, metric: true}}}]`); err == nil {
		t.Error("a secret was accepted as a metric")
	}
}

// Each form a secret takes on the way out or back is masked.
func TestSecretIsMaskedInEveryForm(t *testing.T) {
	t.Setenv("ARGUS_TEST_PW", "pa:ss@w/rd")
	target := serveAt(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/user/"):
			_, _ = io.WriteString(w, "no such user "+r.URL.EscapedPath())
		case r.URL.Path == "/json":
			_, _ = io.WriteString(w, `{"error":"bad password pa:ss@w\/rd"}`)
		case r.URL.Path == "/lower":
			_, _ = io.WriteString(w, "refused "+strings.ToLower(r.URL.RawQuery))
		}
	}))
	for name, step := range map[string]string{
		"path segment":  `get: "/user/${secret.pw}"`,
		"json \\/":      `post: /json, json: {pw: "${secret.pw}"}`,
		"lowercase %xx": `get: /lower, query: {p: "${secret.pw}"}`,
	} {
		p := newProber(t, "secrets: {pw: {env: ARGUS_TEST_PW}}\nsteps:\n  - {name: a, "+step+`, extract: {v: {regex: "^welcome"}}}`+"\n")
		res := probeOnce(t, p, target)
		for _, text := range []string{res.Err.Error(), p.Reports()[0].Steps[0].Error} {
			if strings.Contains(text, "ss@w") || strings.Contains(strings.ToLower(text), "ss%40w") {
				t.Errorf("%s: %s", name, text)
			}
		}
	}
}

// Masking is for what is shown; checks and extracts read the answer as it came.
func TestChecksSeeTheBodyUnmasked(t *testing.T) {
	t.Setenv("ARGUS_TEST_USER", "argus-synthetic")
	t.Setenv("ARGUS_TEST_PIN", "1234")
	target := serveAt(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dashboard":
			_, _ = io.WriteString(w, "<p>Signed in as argus-synthetic</p>")
		case "/count":
			_, _ = io.WriteString(w, `{"total": 1234}`)
		}
	}))
	p := newProber(t, `
secrets: {user: {env: ARGUS_TEST_USER}, pin: {env: ARGUS_TEST_PIN}}
steps:
  - {name: dashboard, get: /dashboard, headers: {X-U: "${secret.user}"}, expect: [{body_regex: "Signed in as argus-synthetic"}]}
  - {name: count, get: /count, headers: {X-P: "${secret.pin}"}, extract: {n: {json_path: total}}}
`)
	if res := probeOnce(t, p, target); !res.OK() {
		t.Errorf("%v", res.Err)
	}
}

// An id past 2^53 is not a float: cleanup deletes what the run created.
func TestLargeIDsStayExact(t *testing.T) {
	var mu sync.Mutex
	var deleted string
	target := serveAt(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"id": 1790000000000000123, "ratio": 1e+00}`)
		case http.MethodDelete:
			mu.Lock()
			deleted = r.URL.Path
			mu.Unlock()
		}
	}))
	p := newProber(t, `
steps:
  - {name: create, post: /items, body: x, extract: {id: {json_path: id}}, expect: [{json_path: ratio, json_equals: "1"}]}
cleanup:
  - {name: delete, delete: "/items/${var.id}"}
`)
	if res := probeOnce(t, p, target); !res.OK() {
		t.Fatal(res.Err)
	}
	if mu.Lock(); deleted != "/items/1790000000000000123" {
		t.Errorf("cleanup deleted %s", deleted)
	}
	mu.Unlock()
}

func TestFailureKeptInAReportIsBounded(t *testing.T) {
	target := serveAt(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"state":"`+strings.Repeat("x", 512<<10)+`"}`)
	}))
	p := newProber(t, `steps: [{name: a, get: /x, expect: [{json_path: state, json_equals: done}]}]`)
	res := probeOnce(t, p, target)
	rep := p.Reports()[0]
	if n := len(res.Err.Error()) + len(rep.Error) + len(rep.Steps[0].Error); n > 4<<10 {
		t.Errorf("one failed run keeps %d bytes of message", n)
	}
}

func TestJSONBodyIsSentAsWritten(t *testing.T) {
	if _, err := build(`steps: [{name: a, post: /x, json: {from: 2026-01-01}}]`); err == nil || !strings.Contains(err.Error(), "quote it") {
		t.Errorf("an unquoted date: %v", err)
	}
	if _, err := build(`steps: [{name: a, post: /x, json: {from: "2026-01-01"}}]`); err != nil {
		t.Errorf("a quoted date: %v", err)
	}
}

// A value net/http will not send is the answer's fault, not the network's.
func TestUnsendableHeaderIsContent(t *testing.T) {
	target := serveAt(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"v":"a\r\nb"}`)
	}))
	p := newProber(t, `
steps:
  - {name: a, get: /v, extract: {v: {json_path: v}}}
  - {name: b, get: /w, headers: {X-V: "${var.v}"}}
`)
	if res := probeOnce(t, p, target); probe.ReasonOf(res.Err) != probe.ReasonContent {
		t.Errorf("reason %s: %v", probe.ReasonOf(res.Err), res.Err)
	}
}
