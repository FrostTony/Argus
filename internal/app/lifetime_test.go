package app

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// gated blocks every run until released or cancelled, so a test can act while
// a run is in flight.
type gated struct {
	entered chan struct{}
	release chan struct{}
}

var gates sync.Map // probe name -> *gated

func init() {
	probe.Register("gated", func(pc config.Probe) (probe.Prober, error) {
		g, _ := gates.LoadOrStore(pc.Name, &gated{entered: make(chan struct{}, 16), release: make(chan struct{})})
		return g.(*gated), nil
	})
	probe.Register("nan", func(config.Probe) (probe.Prober, error) { return nanGauge{}, nil })
	probe.Register("stubborn", func(config.Probe) (probe.Prober, error) { return stubborn, nil })
}

// stubborn ignores cancellation, as a prober stuck in a syscall does.
var stubborn = &stubbornProber{entered: make(chan struct{}, 1), release: make(chan struct{})}

type stubbornProber struct{ entered, release chan struct{} }

func (s *stubbornProber) Probe(context.Context, probe.Request, *metrics.Recorder) probe.Result {
	s.entered <- struct{}{}
	<-s.release
	return probe.Result{}
}

func (g *gated) Probe(ctx context.Context, _ probe.Request, _ *metrics.Recorder) probe.Result {
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return probe.Result{}
	case <-ctx.Done():
		return probe.Result{Err: ctx.Err()}
	}
}

type nanGauge struct{}

func (nanGauge) Probe(_ context.Context, _ probe.Request, rec *metrics.Recorder) probe.Result {
	rec.Gauge("external_value", math.NaN())
	return probe.Result{}
}

func TestOnDemandRunEndsWithItsProbe(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, `
probes:
  - {name: leaving, type: gated, targets: ["1.1.1.1"]}
  - {name: staying, type: counting, targets: ["2.2.2.2"]}
`), discard())
	if err != nil {
		t.Fatal(err)
	}
	g, _ := gates.Load("leaving")
	done := make(chan error)
	go func() { done <- a.RunOnce(context.Background(), "leaving") }()
	<-g.(*gated).entered

	if err := a.Reload(probesFrom(t, `
probes:
  - {name: staying, type: counting, targets: ["2.2.2.2"]}
`)); err != nil {
		t.Fatal(err)
	}
	// Cancelled by the reload, the run returns without waiting for its target.
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if hasProbe(a.Store.Snapshot(), "leaving") {
		t.Error("an on-demand run brought the series of a removed probe back")
	}
}

func TestCommentInOptionsKeepsTheProbe(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, `
probes:
  - name: a
    type: counting
    targets: ["1.1.1.1"]
    counting:
      x: 1
`), discard())
	if err != nil {
		t.Fatal(err)
	}
	before := byName(a.Runners())["a"]
	edited := probesFrom(t, `
probes:
  - name: a
    type: counting
    targets: ["1.1.1.1"]
    counting:
      # only a comment was added
      x: 1 # and another
`)
	if err := a.Reload(edited); err != nil {
		t.Fatal(err)
	}
	if byName(a.Runners())["a"] != before {
		t.Error("a comment in the prober block restarted the probe")
	}
	if edited.List[0].Options.Content[0].HeadComment == "" {
		t.Error("fingerprinting stripped the comments of the configuration itself")
	}
}

func TestCheckResultCarriesNonFiniteGauges(t *testing.T) {
	a, err := Build(testServer(), config.Probes{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	spec := config.Probe{Name: "x", Type: "nan", Targets: config.Targets{Static: []config.Target{{Name: "t", Host: "1.1.1.1"}}}}
	res, err := a.Check(context.Background(), CheckRequest{Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatalf("the result cannot be served: %v", err)
	}
}

// Cancellation is asynchronous: a run that misses it must still not outlive the drop.
func TestARemovedProbeIsDroppedAfterItsLastRun(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, `
probes:
  - {name: leaving, type: stubborn, targets: ["1.1.1.1"]}
  - {name: staying, type: counting, targets: ["2.2.2.2"]}
`), discard())
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan error)
	go func() { ran <- a.RunOnce(context.Background(), "leaving") }()
	<-stubborn.entered

	reloaded := make(chan error)
	go func() {
		reloaded <- a.Reload(probesFrom(t, `
probes:
  - {name: staying, type: counting, targets: ["2.2.2.2"]}
`))
	}()
	select {
	case <-reloaded:
		t.Fatal("the reload returned while a run of the removed probe could still publish")
	case <-time.After(50 * time.Millisecond):
	}
	close(stubborn.release)
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	<-ran
	if hasProbe(a.Store.Snapshot(), "leaving") {
		t.Error("a run that finished after the reload brought the removed probe back")
	}
}
