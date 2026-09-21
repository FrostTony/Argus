package app

import "testing"

// Phases keep only sum and count unless the node or the probe asks for buckets.
func TestPhaseHistogramsAreOptIn(t *testing.T) {
	const body = `
probes:
  - {name: plain, type: counting, targets: ["1.1.1.1"], counting: {}}
  - {name: on, type: counting, phase_histograms: true, targets: ["1.1.1.1"], counting: {}}
  - {name: off, type: counting, phase_histograms: false, targets: ["1.1.1.1"], counting: {}}
`
	for _, node := range []bool{false, true} {
		srv := testServer()
		srv.Probing.PhaseHistograms = node
		a, err := Build(srv, probesFrom(t, body), discard())
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{"plain": node, "on": true, "off": false}
		for name, r := range byName(a.Runners()) {
			if got := len(r.PhaseBuckets) > 0; got != want[name] {
				t.Errorf("node %v, probe %s: phase histograms %v, want %v", node, name, got, want[name])
			}
			if len(r.Buckets) == 0 {
				t.Errorf("probe %s lost its latency buckets", name)
			}
		}
	}
}
