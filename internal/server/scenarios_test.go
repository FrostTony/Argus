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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

// scenarioNode is a running node with one scenario, "pipeline", against a
// backend that counts logins and answers each once its gate is open.
type scenarioNode struct {
	app    *app.App
	ts     *httptest.Server
	logins atomic.Int64
	gate   atomic.Pointer[chan struct{}]
}

func newScenarioNode(t *testing.T) *scenarioNode {
	t.Helper()
	n := &scenarioNode{}
	open := make(chan struct{})
	close(open)
	n.gate.Store(&open)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.logins.Add(1)
		<-*n.gate.Load()
	}))
	t.Cleanup(backend.Close)

	dir := t.TempDir()
	body := "interval: 1h\ntimeout: 30s\ntargets: [" + backend.URL + "]\n" +
		"steps:\n  - {name: login, http: {path: /login}}\n"
	if err := os.WriteFile(filepath.Join(dir, "pipeline.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scenarios, err := config.LoadScenarios(dir)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultServer()
	cfg.HTTP.API.Enabled = true
	cfg.HTTP.API.Token = "tok"
	cfg.HTTP.API.ReadToken = "ro"
	cfg.Probing.Jitter = 0 // the scheduled run starts with the node, and the tests count it
	var probes config.Probes
	if err := yaml.Unmarshal([]byte(baseProbes), &probes); err != nil {
		t.Fatal(err)
	}
	probes.Scenarios = scenarios
	a, err := app.Build(cfg, probes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); a.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-stopped
		_ = a.Close()
	})
	n.app = a
	n.ts = serve(t, a, nil)
	return n
}

func (n *scenarioNode) status(t *testing.T) app.ScenarioStatus {
	t.Helper()
	code, body := do(t, n.ts, http.MethodGet, "/api/scenarios/pipeline", "ro", "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d %s", code, body)
	}
	var st app.ScenarioStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// settle waits until the scenario has run this many times and is idle.
func (n *scenarioNode) settle(t *testing.T, runs int64) app.ScenarioStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := n.status(t); n.logins.Load() == runs && !st.Running && len(st.LastRuns) == 1 {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("logins = %d, want %d", n.logins.Load(), runs)
	return app.ScenarioStatus{}
}

func TestScenarioRunsInTheBackgroundOnRequest(t *testing.T) {
	n := newScenarioNode(t)
	st := n.settle(t, 1) // the scheduled run
	if st.Steps[0] != "login" || !st.LastRuns[0].Success || st.LastRuns[0].Steps[0].Code != 200 {
		t.Fatalf("status = %+v", st)
	}

	if code, _ := do(t, n.ts, http.MethodPost, "/api/scenarios/pipeline/run", "ro", ""); code != http.StatusUnauthorized {
		t.Errorf("a read token started a run: %d", code)
	}
	if code, body := do(t, n.ts, http.MethodPost, "/api/scenarios/pipeline/run", "tok", ""); code != http.StatusAccepted {
		t.Fatalf("POST run: %d %s", code, body)
	}
	n.settle(t, 2)

	if code, _ := do(t, n.ts, http.MethodPost, "/api/scenarios/nope/run", "tok", ""); code != http.StatusNotFound {
		t.Errorf("unknown scenario: %d, want 404", code)
	}
	if code, _ := do(t, n.ts, http.MethodPost, "/api/scenarios/one/run", "tok", ""); code != http.StatusNotFound {
		t.Errorf("a probe is not a scenario: %d, want 404", code)
	}
	if code, _ := do(t, n.ts, http.MethodGet, "/api/scenarios/pipeline/run", "tok", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET run: %d, want 405", code)
	}

	code, body := do(t, n.ts, http.MethodGet, "/api/scenarios", "ro", "")
	var list []app.ScenarioStatus
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil || len(list) != 1 {
		t.Errorf("list: %d %s", code, body)
	}
}

func TestScenarioIsNotRunInsideARequest(t *testing.T) {
	n := newScenarioNode(t)
	n.settle(t, 1)

	for _, path := range []string{"/api/check?probe=pipeline", "/probe?probe=pipeline"} {
		code, body := do(t, n.ts, http.MethodGet, path, "tok", "")
		if code != http.StatusBadRequest || !strings.Contains(body, "/api/scenarios/pipeline/run") {
			t.Errorf("%s: %d %s", path, code, body)
		}
	}
	code, body := do(t, n.ts, http.MethodPost, "/api/probes?name=pipeline", "tok", "")
	if code != http.StatusBadRequest || !strings.Contains(body, "/api/scenarios/pipeline/run") {
		t.Errorf("/api/probes?name=pipeline: %d %s", code, body)
	}
	// Running everything runs every probe, and no scenario.
	if code, body := do(t, n.ts, http.MethodPost, "/api/probes", "tok", ""); code != http.StatusOK {
		t.Errorf("/api/probes: %d %s", code, body)
	}
	if got := n.logins.Load(); got != 1 {
		t.Errorf("logins = %d: running all probes ran the scenario", got)
	}
}

func TestBusyScenarioIsNotStartedTwice(t *testing.T) {
	n := newScenarioNode(t)
	n.settle(t, 1)

	gate := make(chan struct{})
	n.gate.Store(&gate)
	if code, body := do(t, n.ts, http.MethodPost, "/api/scenarios/pipeline/run", "tok", ""); code != http.StatusAccepted {
		t.Fatalf("POST run: %d %s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for n.logins.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !n.status(t).Running {
		t.Error("a run in progress is reported as running")
	}
	if code, _ := do(t, n.ts, http.MethodPost, "/api/scenarios/pipeline/run", "tok", ""); code != http.StatusConflict {
		t.Errorf("second run: %d, want 409", code)
	}
	close(gate)
	n.settle(t, 2)
}

func TestConfigPushKeepsTheScenarios(t *testing.T) {
	n := newScenarioNode(t)
	n.settle(t, 1)

	if code, body := do(t, n.ts, http.MethodPut, "/api/config", "tok", baseProbes); code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if !n.app.IsScenario("pipeline") {
		t.Error("a pushed check configuration took the scenarios with it")
	}
}

// A report outlives a reload, but not the target it was about.
func TestLastRunsFollowTheTargets(t *testing.T) {
	n := newScenarioNode(t)
	before := n.settle(t, 1)

	dir := t.TempDir()
	body := "interval: 1h\ntimeout: 30s\ntargets: [" + before.Targets[0] + "/moved]\n" +
		"steps:\n  - {name: login, http: {path: /login}}\n"
	if err := os.WriteFile(filepath.Join(dir, "pipeline.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scenarios, err := config.LoadScenarios(dir)
	if err != nil {
		t.Fatal(err)
	}
	probes := n.app.Probes()
	probes.Scenarios = scenarios
	if err := n.app.Reload(probes); err != nil {
		t.Fatal(err)
	}
	for _, rep := range n.status(t).LastRuns {
		if rep.Target != before.Targets[0]+"/moved" {
			t.Errorf("the removed target %s is still reported", rep.Target)
		}
	}
}
