package server

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

type nonFinite struct{}

func (nonFinite) Probe(_ context.Context, _ probe.Request, rec *metrics.Recorder) probe.Result {
	rec.Gauge("external_value", math.NaN())
	rec.Gauge("external_limit", math.Inf(1))
	return probe.Result{}
}

func init() {
	probe.Register("nonfinite", func(config.Probe) (probe.Prober, error) { return nonFinite{}, nil })
}

// Parsing expands ${VAR} and its errors quote what they choke on, so a caller
// without a token must never get as far as the parser.
func TestCheckParsesOnlyOnceAuthorised(t *testing.T) {
	t.Setenv("ARGUS_TEST_SECRET", "s3cr3t-value")
	ts := serve(t, newApp(t), nil)
	for _, body := range []string{
		`{name: x, type: noop, interval: "${ARGUS_TEST_SECRET}"}`,
		`{name: x, type: "${ARGUS_TEST_SECRET}"}`,
		`{name: "${ARGUS_TEST_SECRET}", type: noop, targets: ["1.1.1.1"], noop: {}, bogus: 1}`,
		`{name: x, type: noop, targets: ["1.1.1.1"], noop: {}, labels: {"${ARGUS_TEST_SECRET}": a}}`,
	} {
		code, out := do(t, ts, http.MethodPost, "/api/check", "", body)
		if code != http.StatusUnauthorized || strings.Contains(out, "s3cr3t-value") {
			t.Errorf("no token: %d %s", code, out)
		}
	}
	// Whitespace is no definition, so running a configured probe stays a read.
	if code, out := do(t, ts, http.MethodPost, "/api/check?probe=one", "", "  \n"); code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated read was let through: %d %s", code, out)
	}
}

func TestConfigReadsBackWithoutExpandingSecrets(t *testing.T) {
	t.Setenv("ARGUS_TEST_SECRET", "s3cr3t-value")
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.ReadToken = "ro" })
	ts := serve(t, a, nil)

	const pushed = "probes:\n  - {name: one, type: noop, targets: [\"https://x/?key=${ARGUS_TEST_SECRET}\"], noop: {}}\n"
	if code, body := do(t, ts, http.MethodPut, "/api/config", "tok", pushed); code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}
	code, body := do(t, ts, http.MethodGet, "/api/config", "ro", "")
	if code != http.StatusOK || body != pushed {
		t.Fatalf("GET: %d, want what was pushed, reference included:\n%s", code, body)
	}
}

// Two pushes at once: the one running must be the one saved and the one /status
// reports, or a restart brings back the other.
func TestConcurrentPushesStayConsistent(t *testing.T) {
	a := newApp(t)
	path := filepath.Join(t.TempDir(), "checks.yaml")
	saving, release := make(chan struct{}), make(chan struct{})
	ts := serveWith(t, a, Files{Persist: func(raw []byte) error {
		if strings.Contains(string(raw), "first") {
			close(saving)
			<-release
		}
		return config.SaveProbes(path, raw)
	}})

	const first = "probes:\n  - {name: first, type: noop, targets: [\"1.1.1.1\"], noop: {}}\n"
	const second = "probes:\n  - {name: second, type: noop, targets: [\"1.1.1.1\"], noop: {}}\n"
	firstDone, secondDone := make(chan int, 1), make(chan int, 1)
	go func() { code, _ := do(t, ts, http.MethodPut, "/api/config", "tok", first); firstDone <- code }()
	<-saving
	go func() { code, _ := do(t, ts, http.MethodPut, "/api/config", "tok", second); secondDone <- code }()
	// Give the second push the chance to overtake the first's save.
	select {
	case code := <-secondDone:
		secondDone <- code
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if c1, c2 := <-firstDone, <-secondDone; c1 != http.StatusOK || c2 != http.StatusOK {
		t.Fatalf("pushes answered %d and %d", c1, c2)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	running := probeNames(a)
	if len(running) != 1 || !strings.Contains(string(onDisk), "name: "+running[0]) {
		t.Fatalf("running %v, saved:\n%s", running, onDisk)
	}
	if a.ConfigMeta().Hash != config.Hash(onDisk) {
		t.Fatal("the reported hash is not the hash of what was saved")
	}
}

func TestNonFiniteGaugesKeepPagesServing(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)
	reload(t, a, "probes:\n  - {name: one, type: nonfinite, targets: [\"1.1.1.1\"], nonfinite: {}}\n")
	if err := a.RunOnce(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/status/data", "/api/check?probe=one"} {
		if code, body := do(t, ts, http.MethodGet, path, "tok", ""); code != http.StatusOK || !strings.Contains(body, "one") {
			t.Errorf("%s: %d %.200s", path, code, body)
		}
	}
}

func TestUnencodableAnswerIsAnError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, math.NaN())
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}
