package app

import (
	"context"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func upTargets(samples []metrics.Sample) map[string]bool {
	out := map[string]bool{}
	for _, s := range samples {
		if s.Name == probe.SeriesUp {
			out[s.Labels.Get("target")] = true
		}
	}
	return out
}

// The rebuilt runner continues from what the old one knew, so it can retire
// the target it no longer has.
func TestTargetRemovedByReloadIsRetired(t *testing.T) {
	srv := testServer() // interval: 1h
	srv.Probing.StaleAfter = config.Duration(50 * time.Millisecond)

	const two = `
probes:
  - {name: a, type: counting, targets: ["1.1.1.1", "2.2.2.2"], counting: {}}
`
	const one = `
probes:
  - {name: a, type: counting, targets: ["1.1.1.1"], counting: {}}
`
	a, err := Build(srv, probesFrom(t, two), discard())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.RunOnce(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := upTargets(a.Store.Snapshot()); !got["1.1.1.1"] || !got["2.2.2.2"] {
		t.Fatalf("probe_up before the reload: %v", got)
	}

	if err := a.Reload(probesFrom(t, one)); err != nil {
		t.Fatal(err)
	}
	if err := a.RunOnce(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // three times stale_after
	got := upTargets(a.Store.Snapshot())
	if !got["1.1.1.1"] {
		t.Fatalf("the surviving target lost its probe_up: %v", got)
	}
	if got["2.2.2.2"] {
		t.Fatal("2.2.2.2 was removed from the probe, the probe ran again and stale_after passed three times over, " +
			"yet its probe_up is still exposed (and will be for interval + stale_after)")
	}
}
