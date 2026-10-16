package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/scenario"
)

// ErrNotFound says nothing of that name runs on this node.
var ErrNotFound = errors.New("not found")

// ErrNotRunning refuses a background run before Start or after shutdown, when
// nothing would wait for it.
var ErrNotRunning = errors.New("the node is not running")

// ScenarioStatus is a scenario as the API describes it: what it does and how
// its last run against each target went.
type ScenarioStatus struct {
	Name     string            `json:"name"`
	Interval string            `json:"interval"`
	Timeout  string            `json:"timeout"`
	Targets  []string          `json:"targets"`
	Running  bool              `json:"running"`
	Steps    []string          `json:"steps"`
	Cleanup  []string          `json:"cleanup,omitempty"`
	LastRuns []scenario.Report `json:"last_runs"`
}

// Scenarios lists the scenarios this node runs, in configuration order.
func (a *App) Scenarios() []ScenarioStatus {
	out := []ScenarioStatus{}
	for _, r := range a.current() {
		if st, ok := describe(r); ok {
			out = append(out, st)
		}
	}
	return out
}

// Scenario describes one scenario this node runs.
func (a *App) Scenario(name string) (ScenarioStatus, error) {
	if r := a.find(name); r != nil {
		if st, ok := describe(r); ok {
			return st, nil
		}
	}
	return ScenarioStatus{}, fmt.Errorf("%w: no scenario %q runs on this node", ErrNotFound, name)
}

// IsScenario reports whether the named probe running here is a scenario.
func (a *App) IsScenario(name string) bool {
	r := a.find(name)
	return r != nil && r.runner.Kind == config.ScenarioType
}

func describe(r *running) (ScenarioStatus, bool) {
	p, ok := r.runner.Prober.(*scenario.Prober)
	if !ok {
		return ScenarioStatus{}, false
	}
	steps, cleanup := p.Steps()
	st := ScenarioStatus{
		Name:     r.runner.Name,
		Interval: r.runner.Interval.String(),
		Timeout:  r.runner.Timeout.String(),
		Targets:  []string{},
		Running:  r.runner.Busy(),
		Steps:    steps,
		Cleanup:  cleanup,
		LastRuns: []scenario.Report{},
	}
	current := map[string]bool{}
	for _, t := range r.runner.Source.Targets() {
		st.Targets = append(st.Targets, t.Name)
		current[t.Name] = true
	}
	// A report outlives a reload; one of a target since removed is history.
	for _, rep := range p.Reports() {
		if current[rep.Target] {
			st.LastRuns = append(st.LastRuns, rep)
		}
	}
	return st, true
}

// RunScenario starts a run of the named scenario off its schedule and returns
// at once: a run takes minutes, longer than an API request may last. Its
// outcome goes where a scheduled run's does — the metrics and LastRuns. It
// ends with the node and with the scenario's place in the configuration.
func (a *App) RunScenario(name string) error {
	r := a.find(name)
	if r == nil || r.runner.Kind != config.ScenarioType {
		return fmt.Errorf("%w: no scenario %q runs on this node", ErrNotFound, name)
	}
	a.mu.Lock()
	if a.rootCtx == nil || a.stopped {
		a.mu.Unlock()
		return ErrNotRunning
	}
	root := a.rootCtx
	// Registered under mu, where Start sets stopped: shutdown waits for it.
	a.live.Add(1)
	a.mu.Unlock()
	return r.runInBackground(root, a.Sink, a.live.Done)
}

// runInBackground is runNow for a run nobody waits on. done is called exactly
// once, whether the run started or not.
func (r *running) runInBackground(ctx context.Context, sink metrics.Sink, done func()) error {
	ctx, leave, ok := r.enter(ctx)
	if !ok {
		done()
		return fmt.Errorf("%w: %q has just left the configuration", ErrNotFound, r.runner.Name)
	}
	finish := func() {
		leave()
		done()
	}
	if !r.runner.Go(ctx, sink, finish) {
		finish()
		return ErrBusy
	}
	return nil
}

// find is the running probe of that name.
func (a *App) find(name string) *running {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.probes[name]
}

// ErrScenario turns a scenario away from the paths that run a check inside a
// request: it changes the target and takes minutes.
var ErrScenario = errors.New("a scenario changes things on the target and runs for minutes, " +
	"so it does not run inside a request")

// ScenarioOnDemand is ErrScenario for the scenario named, with where to go instead.
func ScenarioOnDemand(name string) error {
	return fmt.Errorf("%q: %w; start it with POST /api/scenarios/%s/run", name, ErrScenario, name)
}
