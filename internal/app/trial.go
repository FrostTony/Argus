package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/scenario"
)

// A trial is one run of a scenario that is not configured here: whoever is
// writing it walks it through once and reads the report step by step, before
// it is pushed to run on a schedule. It runs in the background, as a
// configured scenario started through the API does — a walk takes minutes —
// and keeps its report for a while after.
//
// A trial writes no metrics: nothing has been configured yet that a series
// could belong to, and a dashboard must not show a draft failing.

const (
	// maxTrials is how many trials run at once. Each holds a session against
	// a target for minutes, and the node's slots are for its schedule.
	maxTrials = 2
	// trialKept is how long a finished trial's report stays to be read.
	trialKept = 30 * time.Minute
	// trialsKept bounds what is kept, finished ones going first.
	trialsKept = 32
	// trialMargin is time on top of the scenario's own budget (the steps' and
	// the cleanup's, which fit its interval) before the trial is cut short.
	trialMargin = 30 * time.Second
)

// ErrTrialsBusy turns a trial away while maxTrials are running.
var ErrTrialsBusy = errors.New("scenario trials are already running here; wait for one to finish")

// Trial is a trial as the API reports it.
type Trial struct {
	ID       string            `json:"id"`
	Scenario string            `json:"scenario"`
	Started  time.Time         `json:"started"`
	Finished *time.Time        `json:"finished,omitempty"`
	Running  bool              `json:"running"`
	Steps    []string          `json:"steps"`
	Cleanup  []string          `json:"cleanup,omitempty"`
	Targets  []string          `json:"targets"`
	Reports  []scenario.Report `json:"reports"`
	// Error is why the trial did not get as far as a run.
	Error string `json:"error,omitempty"`
}

type trials struct {
	mu   sync.Mutex
	byID map[string]*Trial
}

// ValidateScenario builds a scenario as the node would run it — every
// reference, file, validator and timing checked — and runs nothing.
func (a *App) ValidateScenario(pc config.Probe) (Trial, error) {
	runner, err := a.buildRunner(pc, a.nodeLabels())
	if err != nil {
		return Trial{}, err
	}
	p, ok := runner.Prober.(*scenario.Prober)
	if !ok {
		return Trial{}, fmt.Errorf("%q is not a scenario", pc.Name)
	}
	t := Trial{Scenario: pc.Name, Targets: []string{}, Reports: []scenario.Report{}}
	t.Steps, t.Cleanup = p.Steps()
	for _, tg := range runner.Source.Targets() {
		t.Targets = append(t.Targets, tg.Name)
	}
	return t, nil
}

// TryScenario starts a trial of a scenario this node is not configured with
// and returns at once; its report is read with TrialStatus.
func (a *App) TryScenario(pc config.Probe) (Trial, error) {
	runner, err := a.buildRunner(pc, a.nodeLabels())
	if err != nil {
		return Trial{}, err
	}
	prober, ok := runner.Prober.(*scenario.Prober)
	if !ok {
		return Trial{}, fmt.Errorf("%q is not a scenario", pc.Name)
	}
	runner.Schedule = nil
	// A trial holds its session for minutes; the cap is maxTrials, not the
	// slots scheduled probes and on-demand checks share.
	runner.Sem = nil

	t := &Trial{ID: trialID(), Scenario: pc.Name, Started: time.Now(), Running: true,
		Targets: []string{}, Reports: []scenario.Report{}}
	t.Steps, t.Cleanup = prober.Steps()
	for _, tg := range runner.Source.Targets() {
		t.Targets = append(t.Targets, tg.Name)
	}

	a.mu.Lock()
	if a.rootCtx == nil || a.stopped {
		a.mu.Unlock()
		return Trial{}, ErrNotRunning
	}
	root := a.rootCtx
	a.trials.mu.Lock()
	if a.trials.running() >= maxTrials {
		a.trials.mu.Unlock()
		a.mu.Unlock()
		return Trial{}, ErrTrialsBusy
	}
	a.trials.keep(t)
	snapshot := *t
	a.trials.mu.Unlock()
	// Registered under mu, where Start sets stopped: shutdown waits for it.
	a.live.Add(1)
	a.mu.Unlock()

	budget := pc.Interval.D() + trialMargin
	go func() {
		defer a.live.Done()
		ctx, cancel := context.WithTimeout(root, budget)
		defer cancel()
		runner.RunOnce(ctx, &captureSink{})
		now := time.Now()
		a.trials.mu.Lock()
		defer a.trials.mu.Unlock()
		t.Running, t.Finished, t.Reports = false, &now, prober.Reports()
	}()
	a.Log.Info("scenario trial started", "scenario", pc.Name, "trial", t.ID, "targets", len(t.Targets))
	return snapshot, nil
}

// TrialStatus is a trial as it stands: running, or how each target went.
func (a *App) TrialStatus(id string) (Trial, error) {
	a.trials.mu.Lock()
	defer a.trials.mu.Unlock()
	t, ok := a.trials.byID[id]
	if !ok {
		return Trial{}, fmt.Errorf("%w: no trial %q here; a report is kept for %s", ErrNotFound, id, trialKept)
	}
	out := *t
	out.Reports = slices.Clone(t.Reports)
	return out, nil
}

// running counts the trials still going. Called with mu held.
func (ts *trials) running() int {
	n := 0
	for _, t := range ts.byID {
		if t.Running {
			n++
		}
	}
	return n
}

// keep adds a trial, letting go of the finished ones past their time and,
// beyond trialsKept, the oldest finished. Called with mu held.
func (ts *trials) keep(t *Trial) {
	if ts.byID == nil {
		ts.byID = map[string]*Trial{}
	}
	var finished []*Trial
	for id, old := range ts.byID {
		switch {
		case old.Running:
		case time.Since(*old.Finished) > trialKept:
			delete(ts.byID, id)
		default:
			finished = append(finished, old)
		}
	}
	slices.SortFunc(finished, func(x, y *Trial) int { return x.Finished.Compare(*y.Finished) })
	for len(ts.byID) >= trialsKept && len(finished) > 0 {
		delete(ts.byID, finished[0].ID)
		finished = finished[1:]
	}
	ts.byID[t.ID] = t
}

func trialID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
