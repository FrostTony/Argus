package external

import (
	"context"
	"os"
	"path/filepath"
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
	pc := config.Probe{Name: "test", Type: "external"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "check.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func request() probe.Request {
	return probe.Request{Target: probe.Target{Name: "site", Host: "example.com"}}
}

func TestParsesOutputIntoMetrics(t *testing.T) {
	path := script(t, `
echo "# a comment"
echo "queue_depth 42"
echo 'replica_lag{replica="r1"} 1.5'
`)
	p := newProber(t, "command: "+path)
	rec := metrics.NewRecorder(nil)
	res := p.Probe(context.Background(), request(), rec)
	if !res.OK() {
		t.Fatalf("check failed: %v", res.Err)
	}

	want := map[string]float64{"external_queue_depth": 42, "external_replica_lag": 1.5}
	for _, s := range rec.Samples() {
		if v, ok := want[s.Name]; ok {
			if g := float64(s.Value.(metrics.Gauge)); g != v {
				t.Fatalf("%s = %v, want %v", s.Name, g, v)
			}
			delete(want, s.Name)
			if s.Name == "external_replica_lag" && s.Labels.Get("replica") != "r1" {
				t.Fatalf("labels lost: %v", s.Labels)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("metrics missing: %v", want)
	}
}

func TestNonZeroExitFails(t *testing.T) {
	p := newProber(t, "command: "+script(t, "echo boom >&2\nexit 3\n"))
	res := p.Probe(context.Background(), request(), metrics.NewRecorder(nil))
	if res.OK() {
		t.Fatal("a non-zero exit must fail the check")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonProtocol {
		t.Fatalf("reason: %q", got)
	}
}

func TestTargetReachesTheCommand(t *testing.T) {
	p := newProber(t, "command: "+script(t, `echo "seen 1" ; test "$ARGUS_HOST" = "example.com"`))
	res := p.Probe(context.Background(), request(), metrics.NewRecorder(nil))
	if !res.OK() {
		t.Fatalf("the command did not see ARGUS_HOST: %v", res.Err)
	}
}

func TestMalformedOutputIsContentFailure(t *testing.T) {
	p := newProber(t, "command: "+script(t, `echo "not-a-metric"`))
	res := p.Probe(context.Background(), request(), metrics.NewRecorder(nil))
	if got := probe.ReasonOf(res.Err); got != probe.ReasonContent {
		t.Fatalf("reason: %q, want content", got)
	}
}

func TestExitCodeModeIgnoresOutput(t *testing.T) {
	p := newProber(t, "command: "+script(t, `echo "garbage"`)+"\nmode: exit_code")
	res := p.Probe(context.Background(), request(), metrics.NewRecorder(nil))
	if !res.OK() {
		t.Fatalf("exit_code mode must ignore stdout: %v", res.Err)
	}
}

func TestExternalDoesNotOutliveItsDeadline(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no shell")
	}
	p := newProber(t, `
command: /bin/sh
args: ["-c", "sleep 30 & exit 0"]
mode: exit_code
self_addressed: true
`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := p.Probe(ctx, probe.Request{Target: probe.Target{Name: "x", Host: "x"}},
		metrics.NewRecorder(metrics.Labels{}))
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("the probe took %s against a 300ms deadline: the slot is held and the schedule stalls", elapsed)
	}
	if res.Err == nil {
		t.Fatal("a command killed at its deadline was reported as a success")
	}
}
