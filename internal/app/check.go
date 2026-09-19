package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/logging"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// CheckResult is one ad-hoc check, with nothing written to the store.
type CheckResult struct {
	Probe    string        `json:"probe"`
	Type     string        `json:"type"`
	Duration string        `json:"duration"`
	Success  bool          `json:"success"`
	Targets  []TargetCheck `json:"targets"`
	Log      []string      `json:"log,omitempty"`
}

// TargetCheck is one target's outcome, broken down by backend.
type TargetCheck struct {
	Target   string         `json:"target"`
	Resolve  float64        `json:"resolve_seconds,omitempty"`
	Up       int            `json:"backends_up"`
	Total    int            `json:"backends_total"`
	Error    string         `json:"error,omitempty"`
	Backends []BackendCheck `json:"backends"`
}

// BackendCheck is what one address answered.
type BackendCheck struct {
	Backend string             `json:"backend,omitempty"`
	Family  string             `json:"family,omitempty"`
	Success bool               `json:"success"`
	Reason  string             `json:"reason,omitempty"`
	Error   string             `json:"error,omitempty"`
	Phases  map[string]float64 `json:"phases,omitempty"`
	Metrics map[string]any     `json:"metrics,omitempty"`
}

// CheckRequest asks for one ad-hoc run.
type CheckRequest struct {
	// Probe names a configured probe to run. Mutually exclusive with Spec.
	Probe string
	Spec  *config.Probe
	// Target overrides the probe's targets with a single one.
	Target string
	Debug  bool
}

// Check runs one probe once and returns the result without touching the store.
func (a *App) Check(ctx context.Context, req CheckRequest) (*CheckResult, error) {
	runner, capture, trace, elapsed, err := a.checkOnce(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &CheckResult{
		Probe:    runner.Name,
		Type:     runner.Kind,
		Duration: elapsed.Round(time.Microsecond).String(),
		Targets:  capture.targets(),
	}
	res.Success = allUp(res.Targets)
	if trace != nil {
		res.Log = trace.lines()
	}
	return res, nil
}

// Samples runs one probe once and returns what it measured.
func (a *App) Samples(ctx context.Context, req CheckRequest) ([]metrics.Sample, error) {
	runner, capture, _, _, err := a.checkOnce(ctx, req)
	if err != nil {
		return nil, err
	}
	out := capture.collected()
	return append(out, rollUp(runner, capture)...), nil
}

// rollUp adds probe_success, one series per target, where probe_up is per
// backend and absent for a target that stopped resolving.
func rollUp(runner *probe.Runner, capture *captureSink) []metrics.Sample {
	targets := map[string]probe.Target{}
	for _, t := range runner.Source.Targets() {
		targets[t.Name] = t
	}
	var out []metrics.Sample
	for _, t := range capture.targets() {
		up := len(t.Backends) > 0
		for _, b := range t.Backends {
			up = up && b.Success
		}
		known, ok := targets[t.Target]
		if !ok {
			known = probe.Target{Name: t.Target}
		}
		out = append(out, metrics.Sample{
			Name: "probe_success",
			// The target as the runner knows it, so the labels match.
			Labels: runner.TargetLabels(known),
			Value:  metrics.Gauge(boolValue(up)),
		})
	}
	return out
}

// checkOnce is the run itself, shared by Check and Samples.
func (a *App) checkOnce(ctx context.Context, req CheckRequest) (*probe.Runner, *captureSink, *traceSink, time.Duration, error) {
	pc, err := a.checkConfig(req)
	if err != nil {
		return nil, nil, nil, 0, err
	}

	var trace *traceSink
	log := a.Log
	if req.Debug {
		trace = newTraceSink()
		log = trace.logger()
	}

	runner, err := a.buildRunner(pc, a.nodeLabels())
	if err != nil {
		return nil, nil, nil, 0, err
	}
	runner.Log = log
	// A one-off check answers now: the schedule is about the periodic run.
	runner.Schedule = nil

	capture := &captureSink{}
	runner.Observe = capture.observe
	start := time.Now()
	runner.RunOnce(ctx, capture)
	return runner, capture, trace, time.Since(start), nil
}

// checkConfig resolves what to run: a configured probe or a supplied one.
func (a *App) checkConfig(req CheckRequest) (config.Probe, error) {
	var pc config.Probe

	switch {
	case req.Spec != nil:
		pc = *req.Spec
		if pc.Name == "" {
			pc.Name = "adhoc"
		}
	case req.Probe != "":
		found, err := a.probeConfig(req.Probe)
		if err != nil {
			return pc, err
		}
		pc = found
	default:
		return pc, fmt.Errorf("name a probe or supply a definition")
	}

	if req.Target != "" {
		var t config.Target
		if err := t.Parse(req.Target); err != nil {
			return pc, err
		}
		pc.Targets = config.Targets{Static: []config.Target{t}}
	}
	if pc.Targets.Empty() {
		return pc, fmt.Errorf("probe %q has no targets and none was given", pc.Name)
	}
	if pc.Timeout == 0 {
		pc.Timeout = a.Cfg.Probing.Timeout
	}
	// A one-off check has no interval to fit into, so any timeout fits.
	pc.Interval = max(pc.Interval, a.Cfg.Probing.Interval,
		config.Duration(max(1, pc.RequestsPerProbe))*pc.Timeout)
	enabled := false
	pc.Disabled = &enabled
	return pc, nil
}

// probeConfig finds a configured probe by name, with inheritance applied.
func (a *App) probeConfig(name string) (config.Probe, error) {
	source := a.Probes()
	list, err := source.Resolve()
	if err != nil {
		return config.Probe{}, err
	}
	for _, pc := range list {
		if pc.Name == name {
			return pc, nil
		}
	}
	return config.Probe{}, fmt.Errorf("probe %q not found", name)
}

func allUp(targets []TargetCheck) bool {
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if len(t.Backends) == 0 {
			return false
		}
		for _, b := range t.Backends {
			if !b.Success {
				return false
			}
		}
	}
	return true
}

// captureSink collects a run's samples instead of storing them.
type captureSink struct {
	mu      sync.Mutex
	samples []metrics.Sample
	// errs holds the message behind each failure, keyed by target and backend.
	errs map[key]string
}

type key struct{ target, backend string }

func (c *captureSink) Write(_ context.Context, b metrics.Batch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, b.Samples...)
}

func (c *captureSink) collected() []metrics.Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.samples)
}

func (c *captureSink) observe(req probe.Request, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.errs == nil {
		c.errs = map[key]string{}
	}
	var backend string
	if req.Backend.Valid() {
		backend = req.Backend.String()
	}
	c.errs[key{req.Target.Name, backend}] = err.Error()
}

// targets regroups the flat sample stream by target and backend.
func (c *captureSink) targets() []TargetCheck {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]TargetCheck, 0)
	index := map[string]int{}
	target := func(name string) *TargetCheck {
		i, ok := index[name]
		if !ok {
			index[name] = len(out)
			i = len(out)
			out = append(out, TargetCheck{Target: name})
		}
		return &out[i]
	}

	order := make([]key, 0, len(c.samples))
	byKey := make(map[key]*BackendCheck, len(c.samples))

	for _, s := range c.samples {
		name := s.Labels.Get("target")
		tc := target(name)
		if probe.TargetLevel(s.Name) {
			absorbTarget(tc, s)
			continue
		}

		k := key{name, s.Labels.Get("backend")}
		entry, ok := byKey[k]
		if !ok {
			entry = &BackendCheck{
				Backend: k.backend,
				Family:  s.Labels.Get("family"),
				Phases:  map[string]float64{},
				Metrics: map[string]any{},
			}
			byKey[k] = entry
			order = append(order, k)
		}
		absorb(entry, s)
	}

	for _, k := range order {
		tc := target(k.target)
		entry := byKey[k]
		entry.Error = c.errs[k]
		tc.Backends = append(tc.Backends, *entry)
	}
	// A target that never got as far as a backend reports why here.
	for i := range out {
		if len(out[i].Backends) > 0 || out[i].Total > 0 {
			continue
		}
		if msg, ok := c.errs[key{out[i].Target, ""}]; ok {
			out[i].Error = msg
			continue
		}
		out[i].Error = "the target produced no result"
	}
	return out
}

func absorbTarget(tc *TargetCheck, s metrics.Sample) {
	g, ok := s.Value.(metrics.Gauge)
	if !ok {
		return
	}
	switch s.Name {
	case probe.SeriesBackends:
		tc.Total = int(g)
	case probe.SeriesBackendsUp:
		tc.Up = int(g)
	case probe.TimeResolve.Last():
		tc.Resolve = float64(g)
	}
}

func absorb(entry *BackendCheck, s metrics.Sample) {
	switch s.Name {
	case probe.SeriesUp:
		entry.Success = s.Value == metrics.Gauge(1)
		return
	case probe.SeriesFailure:
		entry.Reason = s.Labels.Get("reason")
		return
	case probe.TimeProbe.Last():
		entry.Phases["total"] = float64(s.Value.(metrics.Gauge))
		return
	case probe.TimePhase.Last():
		if g, ok := s.Value.(metrics.Gauge); ok {
			entry.Phases[s.Labels.Get("phase")] = float64(g)
		}
		return
	case probe.TimeResolve.Last():
		if g, ok := s.Value.(metrics.Gauge); ok {
			entry.Phases["resolve"] = float64(g)
		}
		return
	}
	// Counters and distributions say nothing new for a single run.
	switch v := s.Value.(type) {
	case metrics.Gauge:
		entry.Metrics[s.Name] = float64(v)
	case metrics.Info:
		entry.Metrics[s.Name] = string(v)
	}
}

// traceSink captures the run's log lines for the response.
type traceSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newTraceSink() *traceSink { return &traceSink{} }

func (t *traceSink) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}

func (t *traceSink) logger() *slog.Logger {
	log, err := logging.New(logging.Options{Level: "debug", Format: logging.FormatJSON}, t)
	if err != nil {
		return slog.New(slog.NewJSONHandler(t, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return log
}

func (t *traceSink) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []string
	for _, raw := range bytes.Split(bytes.TrimSpace(t.buf.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			out = append(out, string(raw))
			continue
		}
		out = append(out, format(rec))
	}
	return out
}

func format(rec map[string]any) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%v %v", rec["level"], rec["msg"])
	for _, k := range []string{"target", "backend", "reason", "err", "duration"} {
		if v, ok := rec[k]; ok {
			fmt.Fprintf(&b, " %s=%v", k, v)
		}
	}
	return b.String()
}
