package http

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func newProber(t *testing.T, options string) probe.Prober {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "http"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func request(t *testing.T, srv *httptest.Server) probe.Request {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	return probe.Request{
		Target:  probe.Target{Name: "test", Host: u.Hostname(), Port: port, URL: u},
		Backend: probe.Backend{Addr: netip.MustParseAddr(u.Hostname()), Port: port},
		Buckets: metrics.Buckets{0.1, 1},
	}
}

func TestProbeSuccessRecordsPhasesAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<title>ok</title>"))
	}))
	defer srv.Close()

	p := newProber(t, `
validators:
  - name: status
    status_code: ["200-299"]
  - name: title
    body_regex: "<title>"
`)
	rec := metrics.NewRecorder(metrics.L("probe", "test"))
	res := p.Probe(context.Background(), request(t, srv), rec)
	if !res.OK() {
		t.Fatalf("check failed: %v", res.Err)
	}

	phases := map[string]bool{}
	for _, ph := range res.Phases {
		phases[ph.Name] = true
	}
	for _, want := range []string{"connect", "ttfb"} {
		if !phases[want] {
			t.Fatalf("missing phase %q: %+v", want, res.Phases)
		}
	}
	if find(rec, "http_status_code") != 200 {
		t.Fatalf("status code not recorded: %+v", rec.Samples())
	}
	if find(rec, "http_validator") != 1 {
		t.Fatal("validator left no metric")
	}
}

func TestProbeFailsOnStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := newProber(t, `method: GET`)
	rec := metrics.NewRecorder(nil)
	res := p.Probe(context.Background(), request(t, srv), rec)

	if res.OK() {
		t.Fatal("500 must count as a failure")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonStatus {
		t.Fatalf("failure reason: %q, want status", got)
	}
}

func TestProbePinsBackendAndKeepsHost(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	req := probe.Request{
		// A name that does not resolve, so only the pinned backend can answer.
		Target:  probe.Target{Name: "site", Host: "argus.invalid", Port: port, URL: mustURL("http://argus.invalid:" + u.Port() + "/")},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}
	p := newProber(t, `method: GET`)
	res := p.Probe(context.Background(), req, metrics.NewRecorder(nil))

	if !res.OK() {
		t.Fatalf("request never reached the backend: %v", res.Err)
	}
	if gotHost != "argus.invalid:"+u.Port() {
		t.Fatalf("Host was rewritten to %q", gotHost)
	}
}

func TestProbeConnectionRefused(t *testing.T) {
	p := newProber(t, `method: GET`)
	req := probe.Request{
		Target:  probe.Target{Name: "dead", Host: "127.0.0.1", Port: 1, URL: mustURL("http://127.0.0.1:1/")},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: 1},
	}
	res := p.Probe(context.Background(), req, metrics.NewRecorder(nil))
	if res.OK() {
		t.Fatal("connecting to a closed port must not succeed")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonConnect {
		t.Fatalf("reason: %q, want connect", got)
	}
}

func TestParseCodeRange(t *testing.T) {
	cases := map[string][2]int{"200": {200, 200}, "200-399": {200, 399}, "2xx": {200, 299}}
	for in, want := range cases {
		got, err := parseCodeRange(in)
		if err != nil || got.lo != want[0] || got.hi != want[1] {
			t.Fatalf("%s -> %+v (%v)", in, got, err)
		}
	}
	if _, err := parseCodeRange("399-200"); err == nil {
		t.Fatal("a reversed range must be an error")
	}
}

func find(rec *metrics.Recorder, name string) float64 {
	for _, s := range rec.Samples() {
		if s.Name != name {
			continue
		}
		switch v := s.Value.(type) {
		case metrics.Gauge:
			return float64(v)
		case metrics.Counter:
			return float64(v)
		}
	}
	return -1
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestOAuth2TokenRequestIsNotPinnedToTheBackend(t *testing.T) {
	var tokenHits atomic.Int64
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	var seenAuth atomic.Value
	seenAuth.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	p := newProber(t, `
auth:
  oauth2:
    client_id: argus
    client_secret: shhh
    token_url: `+tokenSrv.URL+`/token
`)
	res := p.Probe(context.Background(), request(t, target), metrics.NewRecorder(metrics.Labels{}))
	if res.Err != nil {
		t.Fatalf("probe failed: %v", res.Err)
	}
	if tokenHits.Load() != 1 {
		t.Fatalf("the token endpoint was reached %d times: the request went to the backend instead",
			tokenHits.Load())
	}
	if got := seenAuth.Load().(string); got != "Bearer tok" {
		t.Fatalf("the target saw Authorization %q", got)
	}
}

// read_body: false still reports a size, without pulling the whole body.
func TestReadBodyFalseDoesNotDownloadEverything(t *testing.T) {
	const huge = 8 << 20
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 64<<10)
		for sent.Load() < huge {
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	p := newProber(t, "read_body: false\nmax_body_bytes: 65536\n")
	rec := metrics.NewRecorder(metrics.Labels{})
	if res := p.Probe(context.Background(), request(t, srv), rec); res.Err != nil {
		t.Fatalf("probe failed: %v", res.Err)
	}
	var size float64
	for _, s := range rec.Samples() {
		if s.Name == "http_response_size_bytes" {
			size = float64(s.Value.(metrics.Gauge))
		}
	}
	if size == 0 {
		t.Fatal("the size was reported as zero for a body that was read")
	}
	if size > 2*65536 {
		t.Fatalf("read %v bytes against a 64KiB limit", size)
	}
}

func TestRedirectHopsAreCountedInThePhases(t *testing.T) {
	var mux http.ServeMux
	srv := httptest.NewServer(&mux)
	defer srv.Close()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		http.Redirect(w, r, "/second", http.StatusFound)
	})
	mux.HandleFunc("/second", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte("done"))
	})

	p := newProber(t, "follow_redirects: true\n")
	res := p.Probe(context.Background(), request(t, srv), metrics.NewRecorder(metrics.Labels{}))
	if res.Err != nil {
		t.Fatalf("probe failed: %v", res.Err)
	}
	var sum time.Duration
	for _, ph := range res.Phases {
		sum += ph.D
	}
	if sum < 60*time.Millisecond {
		t.Fatalf("the phases add up to %s for a chain that took at least 80ms: %v", sum, res.Phases)
	}
}

func TestRedirectToAnotherHostIsNotPinned(t *testing.T) {
	elsewhere := listenOn(t, "127.0.0.2", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("arrived"))
	})
	var reached atomic.Int64
	origin := listenOn(t, "127.0.0.1", func(w http.ResponseWriter, r *http.Request) {
		if reached.Add(1) > 1 {
			// The redirect came back here, so it was pinned.
			w.WriteHeader(http.StatusTeapot)
			return
		}
		http.Redirect(w, r, "http://"+elsewhere+"/", http.StatusFound)
	})

	u := mustURL("http://" + origin + "/")
	port, _ := strconv.Atoi(u.Port())
	req := probe.Request{
		Target:  probe.Target{Name: "site", Host: u.Hostname(), Port: port, URL: u},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}

	p := newProber(t, "follow_redirects: true\n")
	res := p.Probe(context.Background(), req, metrics.NewRecorder(metrics.Labels{}))
	if res.Err != nil {
		t.Fatalf("the redirect did not reach the other host: %v", res.Err)
	}
	if n := reached.Load(); n != 1 {
		t.Fatalf("the origin was dialled %d times; the redirect was pinned back to it", n)
	}
}

func listenOn(t *testing.T, addr string, h http.HandlerFunc) string {
	t.Helper()
	ln, err := net.Listen("tcp", addr+":0")
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().String()
}

// SNI is per hop: it names the host of the request being made, not of the first one.
func TestSNIFollowsEachHop(t *testing.T) {
	var mu sync.Mutex
	var names []string
	// GetConfigForClient runs for every handshake, even with no SNI.
	capture := func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		names = append(names, h.ServerName)
		mu.Unlock()
		return nil, nil
	}

	second := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("done"))
	}))
	second.TLS = &tls.Config{GetConfigForClient: capture}
	second.StartTLS()
	defer second.Close()

	first := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/", http.StatusFound)
	}))
	first.TLS = &tls.Config{GetConfigForClient: capture}
	first.StartTLS()
	defer first.Close()

	u := mustURL(first.URL + "/")
	port, _ := strconv.Atoi(u.Port())
	req := probe.Request{
		Target:  probe.Target{Name: "site", Host: "argus.invalid", Port: port, URL: mustURL("https://argus.invalid:" + u.Port() + "/")},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}
	p := newProber(t, "follow_redirects: true\ntls: {insecure_skip_verify: true}\n")
	if res := p.Probe(context.Background(), req, metrics.NewRecorder(metrics.Labels{})); res.Err != nil {
		t.Fatalf("probe failed: %v", res.Err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(names) != 2 {
		t.Fatalf("handshakes: %v, want one per hop", names)
	}
	if names[0] != "argus.invalid" {
		t.Errorf("first hop sent SNI %q, want the domain under test", names[0])
	}
	// The second hop is addressed by IP, and an IP is never sent as SNI.
	if names[1] != "" {
		t.Errorf("second hop sent SNI %q, want none for an address", names[1])
	}
}
