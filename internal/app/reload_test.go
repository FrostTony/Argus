package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/http"
)

// Probers read the files they name once, while being built, so a rotation the
// YAML does not mention still has to rebuild them.
func TestReloadPicksUpRotatedFiles(t *testing.T) {
	var mu sync.Mutex
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = string(b)
		mu.Unlock()
	}))
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(file, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := "probes:\n  - name: h\n    targets: [\"" + srv.URL + "\"]\n    http: {method: POST, body_file: " + file + "}\n"

	a, err := Build(testServer(), probesFrom(t, doc), discard())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	before := byName(a.Runners())["h"]

	// Nothing rotated: the runner, and with it its ticker, is left alone.
	if err := a.Reload(probesFrom(t, doc)); err != nil {
		t.Fatal(err)
	}
	if byName(a.Runners())["h"] != before {
		t.Fatal("an unchanged probe was rebuilt")
	}

	if err := os.WriteFile(file, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(probesFrom(t, doc)); err != nil {
		t.Fatal(err)
	}
	if err := a.RunOnce(context.Background(), "h"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if last != "new" {
		t.Errorf("after the reload the probe still sends %q", last)
	}
}

func TestUnnamedNodeRunsProbesWithRunOn(t *testing.T) {
	const doc = `
probes:
  - {name: everywhere, type: counting, targets: ["1.1.1.1"]}
  - {name: fleet, type: counting, run_on: "^(fra|ams)[0-9]+$", targets: ["2.2.2.2"]}
`
	a, err := Build(testServer(), probesFrom(t, doc), discard())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if byName(a.Runners())["fleet"] == nil {
		t.Error("the unnamed node dropped the run_on probe")
	}
}

// A definition sent to /api/check is held to what a file is held to.
func TestAdhocDefinitionIsValidated(t *testing.T) {
	for _, field := range []string{"source_ip: 10.0.0.300", "ip_version: ipv7", "run_on: \"(\""} {
		_, err := config.ParseProbe([]byte("type: counting\ntargets: [\"1.1.1.1\"]\n" + field + "\n"))
		if err == nil {
			t.Errorf("%s was accepted", field)
		}
	}
}

// A probe that sets only one of interval and timeout inherits the other, and
// the effective pair is what gets checked.
func TestEffectiveTimingIsValidated(t *testing.T) {
	node := testServer()
	node.Probing.Interval = config.Duration(60 * time.Second)
	node.Probing.Timeout = config.Duration(10 * time.Second)
	for name, doc := range map[string]string{
		"timeout above the node's interval": `probes: [{name: a, type: counting, timeout: 90s, targets: ["1.1.1.1"]}]`,
		"interval below the node's timeout": `probes: [{name: a, type: counting, interval: 5s, targets: ["1.1.1.1"]}]`,
		"repeats that cannot fit":           `probes: [{name: a, type: counting, requests_per_probe: 10, targets: ["1.1.1.1"]}]`,
	} {
		_, err := Build(node, probesFrom(t, doc), discard())
		if err == nil || !strings.Contains(err.Error(), "exceeds interval") {
			t.Errorf("%s: want it rejected, got %v", name, err)
		}
	}
}

func TestProbeSuccessCarriesTargetLabels(t *testing.T) {
	const doc = `
probes:
  - name: a
    type: counting
    targets:
      - {host: 1.1.1.1, name: one, labels: {country: ru}}
`
	a, err := Build(testServer(), probesFrom(t, doc), discard())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	samples, err := a.Samples(context.Background(), CheckRequest{Probe: "a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if s.Name == "probe_up" || s.Name == "probe_success" {
			if got := s.Labels.Get("country"); got != "ru" {
				t.Errorf("%s has country=%q, want ru", s.Name, got)
			}
		}
	}
}
