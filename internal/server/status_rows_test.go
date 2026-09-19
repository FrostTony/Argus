package server

import (
	"context"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

type zzDown struct{}

func (zzDown) Probe(context.Context, probe.Request, *metrics.Recorder) probe.Result {
	return probe.Result{Err: probe.Fail(probe.ReasonConnect, "connection refused")}
}

type zzSelfDown struct{ zzDown }

func (zzSelfDown) SelfAddressed() bool { return true }

func init() {
	probe.Register("zzdown", func(config.Probe) (probe.Prober, error) { return zzDown{}, nil })
	probe.Register("zzselfdown", func(config.Probe) (probe.Prober, error) { return zzSelfDown{}, nil })
}

// Every kind of unit gets a row carrying the runner's reason: self-addressed,
// fan-out off, a literal address, a labelled target, and an unresolvable name.
func TestStatusRows(t *testing.T) {
	a := newApp(t)
	reload(t, a, `probes:
  - {name: selfp, type: zzselfdown, targets: ["example.org"]}
  - {name: nofan, type: zzdown, per_backend: false, targets: ["example.org"]}
  - {name: lit, type: zzdown, targets: [{name: web, host: "127.0.0.1", port: 9, labels: {env: prod}}]}
  - {name: nores, type: zzdown, targets: ["does-not-exist.invalid"]}
`)
	for _, name := range []string{"selfp", "nofan", "lit", "nores"} {
		if err := a.RunOnce(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	page, err := newStatusPage(a)
	if err != nil {
		t.Fatal(err)
	}
	view := page.state()
	seen := map[string]bool{}
	for _, p := range view.Probes {
		seen[p.Name] = true
		if len(p.Targets) != 1 {
			t.Errorf("%s: %d target rows, want 1", p.Name, len(p.Targets))
			continue
		}
		tg := p.Targets[0]
		switch p.Name {
		case "nores":
			if len(tg.Backends) != 0 || tg.Error == "" {
				t.Errorf("nores: backends=%d error=%q", len(tg.Backends), tg.Error)
			}
		default:
			if len(tg.Backends) != 1 {
				t.Errorf("%s: %d backend rows, want 1", p.Name, len(tg.Backends))
				continue
			}
			b := tg.Backends[0]
			if b.Up || b.Reason != "connect" || b.Error == "" {
				t.Errorf("%s: row = up=%v reason=%q error=%q", p.Name, b.Up, b.Reason, b.Error)
			}
		}
	}
	for _, name := range []string{"selfp", "nofan", "lit", "nores"} {
		if !seen[name] {
			t.Errorf("probe %s missing from the page", name)
		}
	}
	if view.Down != 4 {
		t.Errorf("down = %d, want 4", view.Down)
	}
}
