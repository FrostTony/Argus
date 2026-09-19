package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func init() {
	probe.Register("noop", func(config.Probe) (probe.Prober, error) { return noop{}, nil })
	probe.Register("broken", func(config.Probe) (probe.Prober, error) { return broken{}, nil })
}

type noop struct{}

func (noop) Probe(context.Context, probe.Request, *metrics.Recorder) probe.Result {
	return probe.Result{}
}

type broken struct{}

func (broken) Probe(context.Context, probe.Request, *metrics.Recorder) probe.Result {
	return probe.Result{Err: probe.Fail(probe.ReasonContent, "the body said %q", "maintenance")}
}

const baseProbes = `
probes:
  - {name: one, type: noop, targets: ["1.1.1.1"], noop: {}}
`

func newApp(t *testing.T) *app.App {
	t.Helper()
	return newAppWith(t, func(*config.Server) {})
}

func newAppWith(t *testing.T, tune func(*config.Server)) *app.App {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.HTTP.API.Enabled = true
	cfg.HTTP.API.Token = "tok"
	cfg.Logging.Level = "error"

	var probes config.Probes
	if err := yaml.Unmarshal([]byte(baseProbes), &probes); err != nil {
		t.Fatal(err)
	}
	tune(&cfg)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(cfg, probes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// serve wires the API to a single check file it may write back to.
func serve(t *testing.T, a *app.App, reload Reloader) *httptest.Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checks.yaml")
	if err := os.WriteFile(path, []byte(baseProbes), 0o600); err != nil {
		t.Fatal(err)
	}
	return serveWith(t, a, Files{
		Reload:  reload,
		Persist: func(raw []byte) error { return config.SaveProbes(path, raw) },
	})
}

func serveWith(t *testing.T, a *app.App, files Files) *httptest.Server {
	t.Helper()
	srv, err := New(a, files)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func TestAPIRequiresToken(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	if code, _ := do(t, ts, http.MethodGet, "/api/config", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", code)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/config", "wrong", ""); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", code)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/config", "tok", ""); code != http.StatusOK {
		t.Fatalf("valid token: %d, want 200", code)
	}
}

func TestMetricsAndStatusNeedNoToken(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	for _, path := range []string{"/metrics", "/status", "/healthz"} {
		if code, _ := do(t, ts, http.MethodGet, path, "", ""); code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
	}
}

func TestConfigRoundTrips(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)

	code, body := do(t, ts, http.MethodGet, "/api/config", "tok", "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d", code)
	}
	if !strings.Contains(body, "one") {
		t.Fatalf("running probe missing from the config:\n%s", body)
	}
	if code, out := do(t, ts, http.MethodPut, "/api/config", "tok", body); code != http.StatusOK {
		t.Fatalf("PUT of its own config: %d %s", code, out)
	}
	if got := len(a.Runners()); got != 1 {
		t.Fatalf("probes after round-trip: %d", got)
	}
}

// A rejected configuration must not disturb the running probes.
func TestBrokenConfigIsRejected(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)

	code, _ := do(t, ts, http.MethodPut, "/api/config", "tok",
		"probes: [{name: x, type: nosuchprober, targets: [\"1.1.1.1\"]}]")
	if code != http.StatusBadRequest {
		t.Fatalf("unknown prober: %d, want 400", code)
	}
	if names := runnerNames(a); len(names) != 1 || names[0] != "one" {
		t.Fatalf("running probes changed: %v", names)
	}
}

func TestReloadEndpointUsesTheReloader(t *testing.T) {
	called := 0
	ts := serve(t, newApp(t), func() error { called++; return nil })

	if code, _ := do(t, ts, http.MethodPost, "/api/reload", "tok", ""); code != http.StatusOK {
		t.Fatalf("reload: %d", code)
	}
	if called != 1 {
		t.Fatalf("reloader called %d times", called)
	}
	// With no files behind it, reload is an error rather than a silent success.
	ts2 := serve(t, newApp(t), nil)
	if code, _ := do(t, ts2, http.MethodPost, "/api/reload", "tok", ""); code != http.StatusBadRequest {
		t.Fatalf("reload without files: %d, want 400", code)
	}
}

func TestRunProbeOnDemand(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	if code, _ := do(t, ts, http.MethodPost, "/api/probes?name=one", "tok", ""); code != http.StatusOK {
		t.Fatalf("run one: %d", code)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/probes?name=absent", "tok", ""); code != http.StatusNotFound {
		t.Fatalf("run absent: %d, want 404", code)
	}
}

func TestSelfMetricsAreFreshOnScrape(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	time.Sleep(20 * time.Millisecond)

	_, body := do(t, ts, http.MethodGet, "/metrics", "", "")
	for _, want := range []string{"argus_uptime_seconds", "argus_probes_configured", "argus_build_info"} {
		if !strings.Contains(body, want) {
			t.Fatalf("%s missing from /metrics", want)
		}
	}
}

func TestAPIDisabledByDefault(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.Enabled = false })
	ts := serve(t, a, nil)
	if code, _ := do(t, ts, http.MethodGet, "/api/config", "tok", ""); code != http.StatusNotFound {
		t.Fatalf("api served while disabled: %d", code)
	}
}

func runnerNames(a *app.App) []string {
	out := make([]string, 0)
	for _, r := range a.Runners() {
		out = append(out, r.Name)
	}
	return out
}

func TestReadAuthCanBeDisabledSeparately(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.ReadAuth = config.AuthNone })
	ts := serve(t, a, nil)

	if code, _ := do(t, ts, http.MethodGet, "/api/config", "", ""); code != http.StatusOK {
		t.Fatalf("open read: %d, want 200", code)
	}
	if code, _ := do(t, ts, http.MethodPut, "/api/config", "", baseProbes); code != http.StatusUnauthorized {
		t.Fatalf("write with read auth disabled: %d, want 401", code)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/reload", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("reload with read auth disabled: %d, want 401", code)
	}
}

func TestReadTokenCannotWrite(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.ReadToken = "ro" })
	ts := serve(t, a, nil)

	if code, _ := do(t, ts, http.MethodGet, "/api/config", "ro", ""); code != http.StatusOK {
		t.Fatalf("read with the read token: %d, want 200", code)
	}
	if code, _ := do(t, ts, http.MethodPut, "/api/config", "ro", baseProbes); code != http.StatusUnauthorized {
		t.Fatalf("write with the read token: %d, want 401", code)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/config", "tok", ""); code != http.StatusOK {
		t.Fatalf("read with the main token: %d", code)
	}
	if code, _ := do(t, ts, http.MethodPut, "/api/config", "tok", baseProbes); code != http.StatusOK {
		t.Fatalf("write with the main token: %d", code)
	}
}

func TestWriteAuthCanBeDisabled(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) {
		c.HTTP.API.WriteAuth = config.AuthNone
		c.HTTP.API.ReadAuth = config.AuthNone
	})
	ts := serve(t, a, nil)

	if code, out := do(t, ts, http.MethodPut, "/api/config", "", baseProbes); code != http.StatusOK {
		t.Fatalf("open write: %d %s", code, out)
	}
}

// Requiring a token with none configured would lock the API to nobody.
func TestAuthConfigIsValidated(t *testing.T) {
	cases := map[string]func(*config.API){
		"write auth without a token": func(a *config.API) { a.Token = "" },
		"unknown read mode":          func(a *config.API) { a.ReadAuth = "maybe" },
	}
	for name, tune := range cases {
		cfg := config.DefaultServer()
		cfg.HTTP.API.Enabled = true
		cfg.HTTP.API.Token = "tok"
		tune(&cfg.HTTP.API)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestCheckStoresNothing(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)

	before := a.Store.Len()
	code, body := do(t, ts, http.MethodGet, "/api/check?probe=one", "tok", "")
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, body)
	}
	if got := a.Store.Len(); got != before {
		t.Fatalf("the check added %d series to the store", got-before)
	}
	if !strings.Contains(body, `"probe": "one"`) || !strings.Contains(body, `"backends"`) {
		t.Fatalf("the result does not describe the check:\n%s", body)
	}
}

func TestCheckNeedsOnlyReadPermission(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.ReadToken = "ro" })
	ts := serve(t, a, nil)

	if code, _ := do(t, ts, http.MethodGet, "/api/check?probe=one", "ro", ""); code != http.StatusOK {
		t.Fatalf("check with the read token: %d", code)
	}
}

func TestCheckReturnsDebugTrace(t *testing.T) {
	ts := serve(t, newApp(t), nil)

	_, plain := do(t, ts, http.MethodGet, "/api/check?probe=one", "tok", "")
	if strings.Contains(plain, `"log"`) {
		t.Errorf("the trace was returned without debug=true:\n%s", plain)
	}
	_, debug := do(t, ts, http.MethodGet, "/api/check?probe=one&debug=true", "tok", "")
	if !strings.Contains(debug, `"log"`) || !strings.Contains(debug, "check succeeded") {
		t.Errorf("no trace with debug=true:\n%s", debug)
	}
}

func TestCheckAcceptsAnAdHocDefinition(t *testing.T) {
	ts := serve(t, newApp(t), nil)

	spec := "name: adhoc\ntype: noop\ntargets: [\"9.9.9.9\"]\nnoop: {}\n"
	code, body := do(t, ts, http.MethodPost, "/api/check", "tok", spec)
	if code != http.StatusOK {
		t.Fatalf("ad-hoc check: %d %s", code, body)
	}
	if !strings.Contains(body, "9.9.9.9") {
		t.Fatalf("the ad-hoc target was not checked:\n%s", body)
	}
}

func TestCheckOverridesTheTarget(t *testing.T) {
	ts := serve(t, newApp(t), nil)

	_, body := do(t, ts, http.MethodGet, "/api/check?probe=one&target=8.8.8.8", "tok", "")
	if !strings.Contains(body, "8.8.8.8") {
		t.Fatalf("the override was ignored:\n%s", body)
	}
}

// The message must be in the plain answer, not only behind debug=true.
func TestFailedCheckReportsTheMessage(t *testing.T) {
	ts := serve(t, newApp(t), nil)

	spec := "name: adhoc\ntype: broken\ntargets: [\"1.1.1.1\"]\nbroken: {}\n"
	code, body := do(t, ts, http.MethodPost, "/api/check", "tok", spec)
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, body)
	}
	if !strings.Contains(body, `"success": false`) {
		t.Fatalf("a failing probe reported success:\n%s", body)
	}
	if !strings.Contains(body, "maintenance") {
		t.Fatalf("the failure message is missing:\n%s", body)
	}
}

func TestCheckRejectsUnknownProbe(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	if code, _ := do(t, ts, http.MethodGet, "/api/check?probe=absent", "tok", ""); code != http.StatusBadRequest {
		t.Fatalf("unknown probe: %d, want 400", code)
	}
}

// The page arrives with the first snapshot already in it.
func TestStatusPageRenders(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)
	if err := a.RunOnce(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, ts, http.MethodGet, "/", "", "")
	if code != http.StatusOK {
		t.Fatalf("status page: %d", code)
	}
	for _, want := range []string{"<!doctype html>", "/status/data", `\"one\"`, "keeps no history"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	// The agent's own metrics carry a probe label but no target.
	if strings.Contains(body, "probes_configured") {
		t.Error("a self-metric leaked onto the page")
	}
}

func TestStatusDataCarriesEveryMetric(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)
	if err := a.RunOnce(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, ts, http.MethodGet, "/status/data", "", "")
	if code != http.StatusOK {
		t.Fatalf("status data: %d %s", code, body)
	}
	var got stateView
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if len(got.Probes) != 1 || len(got.Probes[0].Targets) != 1 {
		t.Fatalf("want one probe with one target, got %+v", got.Probes)
	}
	b := got.Probes[0].Targets[0].Backends
	if len(b) != 1 || !b[0].Up {
		t.Fatalf("want one backend that is up, got %+v", b)
	}
	// probe_duration_seconds is a histogram; the page shows its statistics.
	h, ok := b[0].Hist["probe"]
	if !ok || h.Count == 0 {
		t.Fatalf("no histogram statistics: %+v", b[0].Hist)
	}
	if b[0].Counters["probe_total"] == 0 {
		t.Fatalf("counters missing: %+v", b[0].Counters)
	}
	if got.Up != 1 || got.Down != 0 {
		t.Fatalf("tally = %d up / %d down, want 1/0", got.Up, got.Down)
	}
}

func TestStatusDataCarriesTheFailureMessage(t *testing.T) {
	a := newAppWith(t, func(*config.Server) {})
	ts := serve(t, a, nil)
	// Replace the probe with one that always fails, so there is a message.
	if code, out := do(t, ts, http.MethodPut, "/api/config", "tok",
		"probes: [{name: one, type: broken, targets: [\"1.1.1.1\"], broken: {}}]"); code != http.StatusOK {
		t.Fatalf("config: %d %s", code, out)
	}
	if err := a.RunOnce(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	_, body := do(t, ts, http.MethodGet, "/status/data", "", "")
	var got stateView
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	b := got.Probes[0].Targets[0].Backends[0]
	if b.Up {
		t.Fatalf("the backend should be down: %+v", b)
	}
	if b.Reason != "content" {
		t.Errorf("reason = %q, want content", b.Reason)
	}
	if !strings.Contains(b.Error, "maintenance") {
		t.Errorf("error = %q, want the prober's own message", b.Error)
	}
}

func TestFailureMessageClearsOnRecovery(t *testing.T) {
	a := newAppWith(t, func(*config.Server) {})
	ts := serve(t, a, nil)

	if code, _ := do(t, ts, http.MethodPut, "/api/config", "tok",
		"probes: [{name: one, type: broken, targets: [\"1.1.1.1\"], broken: {}}]"); code != http.StatusOK {
		t.Fatal("config")
	}
	if err := a.RunOnce(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := do(t, ts, http.MethodPut, "/api/config", "tok", baseProbes); code != http.StatusOK {
		t.Fatal("config back to healthy")
	}
	if err := a.RunOnce(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	_, body := do(t, ts, http.MethodGet, "/status/data", "", "")
	if strings.Contains(body, "maintenance") {
		t.Errorf("a stale failure message survived recovery:\n%s", body)
	}
}

func TestStatusDataSkipsSelfMetrics(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)
	if err := a.RunOnce(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	_, body := do(t, ts, http.MethodGet, "/status/data", "", "")
	for _, unwanted := range []string{"probes_configured", "goroutines", "build_info"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("self-metric %q leaked into the page data", unwanted)
		}
	}
}

func TestStatusJSONStillServed(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	code, body := do(t, ts, http.MethodGet, "/status.json", "", "")
	if code != http.StatusOK || !strings.Contains(body, `"probes"`) {
		t.Fatalf("status.json: %d %s", code, body)
	}
}

// A top-level function or var replaces the window property of the same name.
func TestPageDoesNotShadowWindowGlobals(t *testing.T) {
	reserved := map[string]bool{
		"history": true, "location": true, "name": true, "status": true,
		"top": true, "parent": true, "self": true, "length": true, "origin": true,
		"event": true, "screen": true, "close": true, "open": true, "focus": true,
		"print": true, "find": true, "scroll": true, "stop": true, "frames": true,
		"navigator": true, "document": true, "external": true, "menubar": true,
	}
	// Only declarations at the start of a line are top-level.
	decl := regexp.MustCompile(`(?m)^(?:function|var|let|const)\s+([A-Za-z_$][\w$]*)`)
	for _, m := range decl.FindAllStringSubmatch(statusHTML, -1) {
		if reserved[m[1]] {
			t.Errorf("the page declares %q at the top level, which replaces window.%s", m[1], m[1])
		}
	}
}

// The page has no build step, so nothing else would catch a typo in it.
func TestPageScriptParses(t *testing.T) {
	script := statusHTML[strings.Index(statusHTML, "<script>")+len("<script>"):]
	script = script[:strings.Index(script, "</script>")]
	// Balanced braces and parentheses catch a truncated edit.
	depth := map[rune]int{}
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	for _, r := range script {
		switch r {
		case '(', '[', '{':
			depth[r]++
		case ')', ']', '}':
			depth[pairs[r]]--
		}
	}
	for open, n := range depth {
		if n != 0 {
			t.Errorf("unbalanced %q in the page script: %d left open", open, n)
		}
	}
}

// A definition names the external prober; an override redirects credentials.
func TestAdHocChecksNeedWritePermission(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.ReadToken = "ro" })
	ts := serve(t, a, nil)

	if code, _ := do(t, ts, http.MethodGet, "/api/check?probe=one", "ro", ""); code != http.StatusOK {
		t.Errorf("a configured probe was refused to the read token: %d", code)
	}
	spec := "name: adhoc\ntype: noop\ntargets: [\"9.9.9.9\"]\nnoop: {}\n"
	if code, _ := do(t, ts, http.MethodPost, "/api/check", "ro", spec); code != http.StatusUnauthorized {
		t.Errorf("an ad-hoc definition was accepted from the read token: %d", code)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/check?probe=one&target=8.8.8.8", "ro", ""); code != http.StatusUnauthorized {
		t.Errorf("a target override was accepted from the read token: %d", code)
	}
	if code, out := do(t, ts, http.MethodPost, "/api/check", "tok", spec); code != http.StatusOK {
		t.Errorf("the write token was refused: %d %s", code, out)
	}
}

func TestAPIRejectsUnknownFields(t *testing.T) {
	ts := serve(t, newApp(t), nil)

	bad := "probes:\n  - {name: one, type: noop, targets: [\"1.1.1.1\"], intervl: 30s, noop: {}}\n"
	if code, out := do(t, ts, http.MethodPut, "/api/config", "tok", bad); code != http.StatusBadRequest {
		t.Fatalf("a misspelled field was accepted: %d %s", code, out)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/check", "tok",
		"name: x\ntype: noop\ntargets: [\"1.1.1.1\"]\ntimeoutt: 3s\nnoop: {}\n"); code != http.StatusBadRequest {
		t.Fatal("a misspelled field in an ad-hoc check was accepted")
	}
}

// Concurrent runs of one probe share its health state and its slots.
func TestOnDemandRunsDoNotOverlap(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)

	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = do(t, ts, http.MethodPost, "/api/probes?name=one", "tok", "")
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != http.StatusOK && c != http.StatusConflict {
			t.Fatalf("unexpected status %d; want 200 or 409", c)
		}
	}
}

func TestConfigChangeReachesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checks.yaml")
	if err := os.WriteFile(path, []byte(baseProbes), 0o600); err != nil {
		t.Fatal(err)
	}

	a := newApp(t)
	files := Files{
		Reload: func() error {
			probes, err := config.LoadProbes(path)
			if err != nil {
				return err
			}
			return a.Reload(probes)
		},
		Persist: func(raw []byte) error { return config.SaveProbes(path, raw) },
	}
	ts := serveWith(t, a, files)

	const added = `
probes:
  - {name: one, type: noop, targets: ["1.1.1.1"], noop: {}}
  - {name: two, type: noop, targets: ["2.2.2.2"], noop: {}}
`
	if code, body := do(t, ts, http.MethodPut, "/api/config", "tok", added); code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "two") {
		t.Fatalf("the file still holds the old configuration:\n%s", onDisk)
	}

	if code, body := do(t, ts, http.MethodPost, "/api/reload", "tok", ""); code != http.StatusOK {
		t.Fatalf("reload: %d %s", code, body)
	}
	if names := probeNames(a); len(names) != 2 {
		t.Fatalf("a reload from disk undid the change: %v", names)
	}
}

// Applying a change in memory alone would drift the node from its files.
func TestConfigChangeIsRefusedWithNowhereToWriteIt(t *testing.T) {
	a := newApp(t)
	ts := serveWith(t, a, Files{Reload: func() error { return nil }})

	code, body := do(t, ts, http.MethodPut, "/api/config", "tok", baseProbes)
	if code != http.StatusConflict {
		t.Fatalf("PUT with no writable file: %d, want 409", code)
	}
	if !strings.Contains(body, "file") {
		t.Errorf("the message does not say what is missing: %s", body)
	}
}

// A saved configuration the node refuses would stop it starting.
func TestRejectedConfigIsNotWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checks.yaml")
	if err := os.WriteFile(path, []byte(baseProbes), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newApp(t)
	ts := serveWith(t, a, Files{
		Persist: func(raw []byte) error { return config.SaveProbes(path, raw) },
	})

	if code, _ := do(t, ts, http.MethodPut, "/api/config", "tok",
		`probes: [{name: nope, type: does_not_exist, targets: ["1.1.1.1"]}]`); code != http.StatusBadRequest {
		t.Fatalf("an unbuildable probe was accepted: %d", code)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "does_not_exist") {
		t.Fatalf("a rejected configuration was written to the file:\n%s", onDisk)
	}
}

func probeNames(a *app.App) []string {
	var out []string
	for _, p := range a.Status().Probes {
		out = append(out, p.Name)
	}
	return out
}

func TestProbeEndpointRendersAnExposition(t *testing.T) {
	ts := serve(t, newApp(t), nil)

	code, body := do(t, ts, http.MethodGet, "/probe?probe=one&target=1.1.1.1", "tok", "")
	if code != http.StatusOK {
		t.Fatalf("/probe: %d %s", code, body)
	}
	for _, want := range []string{
		"# TYPE argus_probe_success gauge",
		`argus_probe_success{`,
		`probe="one"`,
		`target="1.1.1.1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition is missing %q:\n%s", want, body)
		}
	}
}

// A target that stopped resolving has no per-backend series; the roll-up does.
func TestProbeEndpointReportsFailureAsZero(t *testing.T) {
	a := newAppWith(t, func(*config.Server) {})
	ts := serve(t, a, nil)
	if code, out := do(t, ts, http.MethodPut, "/api/config", "tok",
		"probes: [{name: one, type: broken, targets: [\"1.1.1.1\"], broken: {}}]"); code != http.StatusOK {
		t.Fatalf("config: %d %s", code, out)
	}

	_, body := do(t, ts, http.MethodGet, "/probe?probe=one", "tok", "")
	if !strings.Contains(body, "argus_probe_success{") || !strings.Contains(body, "} 0") {
		t.Fatalf("a failed check did not report probe_success 0:\n%s", body)
	}
}

func TestProbeEndpointStoresNothing(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)
	before := a.Store.Len()

	if code, _ := do(t, ts, http.MethodGet, "/probe?probe=one&target=9.9.9.9", "tok", ""); code != http.StatusOK {
		t.Fatal("probe failed")
	}
	if got := a.Store.Len(); got != before {
		t.Fatalf("the store grew from %d to %d series", before, got)
	}
}

func TestProbeEndpointAcceptsModuleAlias(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	code, body := do(t, ts, http.MethodGet, "/probe?module=one&target=1.1.1.1", "tok", "")
	if code != http.StatusOK || !strings.Contains(body, "argus_probe_success") {
		t.Fatalf("module= was not accepted: %d %s", code, body)
	}
}

// Choosing a target sends the probe's credentials to that address.
func TestProbeEndpointNeedsWritePermissionForAChosenTarget(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) {
		c.HTTP.API.ReadToken = "readonly"
	})
	ts := serve(t, a, nil)

	if code, _ := do(t, ts, http.MethodGet, "/probe?probe=one", "readonly", ""); code != http.StatusOK {
		t.Error("a configured probe, as configured, is a read and was refused")
	}
	if code, _ := do(t, ts, http.MethodGet, "/probe?probe=one&target=evil.example", "readonly", ""); code != http.StatusUnauthorized {
		t.Errorf("a read token pointed the probe at another address: %d", code)
	}
}

// Off means not mounted, not 403.
func TestProbeEndpointCanBeTurnedOff(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.ProbeEndpoint = false })
	ts := serve(t, a, nil)

	if code, _ := do(t, ts, http.MethodGet, "/probe?probe=one", "tok", ""); code != http.StatusNotFound {
		t.Fatalf("/probe answered %d with probe_endpoint off, want 404", code)
	}
}

func TestLogoIsServedAndReferenced(t *testing.T) {
	ts := serve(t, newApp(t), nil)

	code, body := do(t, ts, http.MethodGet, "/logo.png", "", "")
	if code != http.StatusOK {
		t.Fatalf("/logo.png: %d", code)
	}
	if !strings.HasPrefix(body, "\x89PNG") {
		t.Fatalf("/logo.png did not answer with a PNG")
	}
	if len(body) > 32<<10 {
		t.Errorf("the mark is %d bytes; it rides in the binary and should stay small", len(body))
	}

	_, page := do(t, ts, http.MethodGet, "/", "", "")
	for _, want := range []string{`rel="icon" href="/logo.png"`, `class="logo" src="/logo.png"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not carry %q", want)
		}
	}
}

// Registering a handler twice panics at startup.
func TestLogoPathIsReserved(t *testing.T) {
	c := config.DefaultServer()
	c.HTTP.MetricsPath = "/logo.png"
	if err := c.Validate(); err == nil {
		t.Fatal("metrics_path /logo.png was accepted")
	}
}
