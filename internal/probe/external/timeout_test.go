package external

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func TestDeadlineIsATimeout(t *testing.T) {
	p := newProber(t, `
command: /bin/sh
args: ["-c", "exec sleep 30"]
mode: exit_code
self_addressed: true
`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := p.Probe(ctx, request(), metrics.NewRecorder(metrics.Labels{}))
	if got := probe.ReasonOf(res.Err); got != probe.ReasonTimeout {
		t.Fatalf("reason = %q, want %q (err: %v)", got, probe.ReasonTimeout, res.Err)
	}
}

func TestDeadlineKillsTheWholeProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("no pgrep")
	}
	marker := "31415926" // an unusual sleep duration to find the process by
	p := newProber(t, `
command: /bin/sh
args: ["-c", "sleep `+marker+`; echo done"]
mode: exit_code
self_addressed: true
`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = p.Probe(ctx, request(), metrics.NewRecorder(metrics.Labels{}))

	out, _ := exec.Command("pgrep", "-f", "sleep "+marker).Output()
	pids := strings.Fields(string(out))
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = exec.Command("kill", "-9", pid).Run()
		}
	})
	if len(pids) > 0 {
		t.Fatalf("the probe returned, but its grandchild is still running: pid %v", pids)
	}
}
