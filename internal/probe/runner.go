package probe

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/netip"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
)

// TargetSource supplies a probe's targets, read on every run so discovery can
// change the list without a restart.
type TargetSource interface {
	Targets() []Target
}

// StaticTargets is a fixed list from the configuration file.
type StaticTargets []Target

func (s StaticTargets) Targets() []Target { return s }

// Runner executes one probe: resolves targets, fans them out across backends,
// calls the prober and records the shared metrics.
type Runner struct {
	Name     string
	Kind     string
	Prober   Prober
	Source   TargetSource
	Interval time.Duration
	Timeout  time.Duration
	Labels   metrics.Labels
	Buckets  metrics.Buckets
	// PhaseBuckets are the phases' bounds; none keeps only sum and count.
	PhaseBuckets metrics.Buckets

	Resolver    resolve.Resolver
	Family      resolve.Family
	MaxBackends int
	PerBackend  bool
	// Fallback tries the other address family when the preferred one yields nothing.
	Fallback bool
	SourceIP netip.Addr

	// Schedule, when set, decides whether a tick runs. It is owned by this runner,
	// never shared with configuration a reload rewrites.
	Schedule *config.Compiled
	// Negative inverts the outcome: the check passes when it fails.
	Negative bool
	// Requests repeats the check within one run, for a better latency sample.
	Requests int
	// Hostname is the name every request of this probe presents, whatever the
	// target is called.
	Hostname string

	// Sem caps simultaneous backend requests across the whole node.
	Sem chan struct{}
	Log *slog.Logger

	// Observe, when set, receives every outcome with its error intact; the metrics
	// keep only the reason.
	Observe func(req Request, err error)

	inFlight atomic.Bool
	skipped  atomic.Int64
	panics   atomic.Int64
	paused   atomic.Int64

	once   sync.Once
	health *health
}

// Down reports how many backends of this probe are currently failing.
func (r *Runner) Down() int { return r.state().down() }

// Failures maps "target|backend" to what is wrong with it right now.
func (r *Runner) Failures() map[string]Failure { return r.state().failures() }

func (r *Runner) state() *health {
	r.once.Do(func() { r.health = newHealth() })
	return r.health
}

// Inherit continues from what the runner a reload replaces knew. Call it before
// the first run.
func (r *Runner) Inherit(old *Runner) {
	r.once.Do(func() { r.health = old.state() })
}

// Skipped counts runs dropped because the previous one was still going.
func (r *Runner) Skipped() int64 { return r.skipped.Load() }

// Paused counts runs the schedule suppressed.
func (r *Runner) Paused() int64 { return r.paused.Load() }

// Sleeping reports whether the schedule suppresses this probe.
func (r *Runner) Sleeping() bool { return !r.Schedule.Active(time.Now()) }

func (r *Runner) Panics() int64 { return r.panics.Load() }

// Start probes until the context is cancelled. The first run is staggered so the
// node's probes do not leave in one wave.
func (r *Runner) Start(ctx context.Context, sink metrics.Sink, jitter float64) {
	if delay := stagger(r.Interval, jitter); delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()

	// Each run gets its own goroutine so a slow one does not push the schedule back;
	// tick, not the ticker, prevents overlap.
	var wg sync.WaitGroup
	defer wg.Wait()

	run := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.tick(ctx, sink)
		}()
	}

	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// RunNow runs immediately unless a run is in progress, and reports whether it
// ran: on-demand runs share health state with the scheduled ones.
func (r *Runner) RunNow(ctx context.Context, sink metrics.Sink) bool {
	if !r.inFlight.CompareAndSwap(false, true) {
		return false
	}
	defer r.inFlight.Store(false)
	r.RunOnce(ctx, sink)
	return true
}

// tick skips the run when the previous one is still going.
func (r *Runner) tick(ctx context.Context, sink metrics.Sink) {
	if !r.Schedule.Active(time.Now()) {
		r.paused.Add(1)
		return
	}
	if !r.inFlight.CompareAndSwap(false, true) {
		r.skipped.Add(1)
		r.Log.Warn("previous run still in progress, skipping",
			"probe", r.Name, "interval", r.Interval, "skipped_total", r.skipped.Load())
		return
	}
	defer r.inFlight.Store(false)
	r.RunOnce(ctx, sink)
}

// familyWord names the family only when it narrows anything.
func familyWord(f resolve.Family) string {
	if f == resolve.Both || f == "" {
		return ""
	}
	return string(f) + " "
}

func stagger(interval time.Duration, jitter float64) time.Duration {
	if jitter <= 0 || interval <= 0 {
		return 0
	}
	return time.Duration(rand.Float64() * jitter * float64(interval))
}

// RunOnce probes every target once: resolution first, then the probes. The
// semaphore is acquired before a goroutine starts, so it bounds stacks too.
func (r *Runner) RunOnce(ctx context.Context, sink metrics.Sink) {
	targets := r.Source.Targets()
	r.state().begin()

	var (
		mu    sync.Mutex
		runs  = make([]*targetRun, 0, len(targets))
		units []unit
	)
	forEach(ctx, targets, r.resolveWorkers(len(targets)), func(t Target) {
		tr := &targetRun{target: t, rec: metrics.NewRecorder(r.TargetLabels(t))}
		tr.rec.Gauge(SeriesTimeout, r.Timeout.Seconds())
		local := r.plan(ctx, tr)
		mu.Lock()
		runs = append(runs, tr)
		units = append(units, local...)
		mu.Unlock()
	})

	r.probeAll(ctx, units)

	// A cancelled run reached only some backends; publishing would call the rest down.
	if ctx.Err() != nil {
		return
	}

	now := time.Now()
	// A unit that left DNS or discovery has nobody left to write its series.
	for _, scope := range r.state().prune() {
		sink.Write(ctx, metrics.Retire(scope, now))
	}
	for _, tr := range runs {
		if tr.fanout {
			tr.rec.Gauge(SeriesBackendsUp, float64(tr.up))
		}
		r.publish(ctx, sink, tr.rec, now)
		for _, rec := range tr.backends {
			r.publish(ctx, sink, rec, now)
		}
	}
}

// publish ships one recorder as the whole truth about its scope: a gauge the run
// did not write is dropped.
func (r *Runner) publish(ctx context.Context, sink metrics.Sink, rec *metrics.Recorder, now time.Time) {
	b := rec.Batch(now)
	b.Scope, b.Every = rec.Base(), r.Interval
	sink.Write(ctx, b)
}

// targetRun is one target's slice of a run.
type targetRun struct {
	target Target
	rec    *metrics.Recorder
	// fanout is false when there are no backends to count.
	fanout bool

	mu sync.Mutex
	up int
	// backends holds the recorder of every backend that has a scope of its own.
	backends []*metrics.Recorder
}

// unit is one probe call: one backend of one target.
type unit struct {
	run     *targetRun
	backend Backend
	rec     *metrics.Recorder
}

// plan resolves a target and turns it into the probe calls it needs.
func (r *Runner) plan(ctx context.Context, tr *targetRun) []unit {
	if sa, ok := r.Prober.(Addressing); ok && sa.SelfAddressed() {
		return []unit{{run: tr, rec: tr.rec}}
	}

	backends, err := r.backends(ctx, tr.target, tr.rec)
	if err != nil {
		tr.rec.Count(SeriesTotal, 1)
		tr.rec.Count(SeriesFailure, 1, "reason", string(ReasonDNS))
		tr.rec.Gauge(SeriesUp, 0)
		tr.rec.Gauge(SeriesBackends, 0)
		tr.rec.Gauge(SeriesBackendsUp, 0)
		r.Log.Debug("resolution failed", "probe", r.Name, "target", tr.target.Name, "err", err)
		if r.state().changed(unitKey{tr.target.Name, resolveUnit}, tr.rec.Base(), false, err) {
			r.Log.Warn("target stopped resolving",
				"probe", r.Name, "type", r.Kind, "target", tr.target.Name,
				"host", tr.target.Host, "err", Message(err))
		}
		if r.Observe != nil {
			r.Observe(r.request(tr.target, Backend{}), Wrap(ReasonDNS, err))
		}
		return nil
	}

	if r.state().changed(unitKey{tr.target.Name, resolveUnit}, tr.rec.Base(), true, nil) {
		r.Log.Info("target resolves again",
			"probe", r.Name, "target", tr.target.Name, "backends", len(backends))
	}

	tr.fanout = true
	tr.rec.Gauge(SeriesBackends, float64(len(backends)))

	out := make([]unit, 0, len(backends))
	for _, b := range backends {
		labels := tr.rec.Base()
		// With fan-out off there is no address to name.
		if b.Valid() {
			labels = labels.With("backend", b.String()).With("family", b.Family())
		}
		// A recorder per backend: Recorder is not safe for concurrent writes.
		out = append(out, unit{run: tr, backend: b, rec: metrics.NewRecorder(labels)})
	}
	return out
}

// probeAll runs the probe calls within the node's concurrency limit.
func (r *Runner) probeAll(ctx context.Context, units []unit) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for _, u := range units {
		if !r.acquire(ctx) {
			return
		}
		wg.Add(1)
		go func(u unit) {
			defer wg.Done()
			defer r.release()

			ok := r.probeOne(ctx, r.request(u.run.target, u.backend), u.rec)
			if u.rec == u.run.rec {
				return // self-addressed: the prober wrote into the target's recorder
			}
			u.run.mu.Lock()
			defer u.run.mu.Unlock()
			if ok {
				u.run.up++
			}
			if u.backend.Valid() {
				u.run.backends = append(u.run.backends, u.rec)
				return
			}
			// Fan-out off: the one check has no labels of its own.
			u.run.rec.Append(u.rec.Samples()...)
		}(u)
	}
}

func (r *Runner) request(t Target, b Backend) Request {
	return Request{Target: t, Backend: b, Buckets: r.Buckets, SourceIP: r.SourceIP, Hostname: r.Hostname}
}

func (r *Runner) acquire(ctx context.Context) bool {
	if r.Sem == nil {
		return ctx.Err() == nil
	}
	select {
	case r.Sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Runner) release() {
	if r.Sem != nil {
		<-r.Sem
	}
}

// resolveWorkers keeps resolution from spawning a goroutine per target.
func (r *Runner) resolveWorkers(targets int) int {
	n := cap(r.Sem)
	if n == 0 {
		n = defaultResolveWorkers
	}
	return min(n, targets)
}

const defaultResolveWorkers = 32

// forEach runs fn over items with at most n goroutines at a time.
func forEach[T any](ctx context.Context, items []T, n int, fn func(T)) {
	if len(items) == 0 {
		return
	}
	if n <= 0 || n > len(items) {
		n = len(items)
	}

	work := make(chan T)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			for item := range work {
				fn(item)
			}
		}()
	}
	for _, item := range items {
		select {
		case work <- item:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return
		}
	}
	close(work)
	wg.Wait()
}

// TargetLabels are the labels this runner gives a target's own series.
func (r *Runner) TargetLabels(t Target) metrics.Labels {
	return r.Labels.Merge(metrics.L("probe", r.Name, "target", t.Name)).Merge(t.Labels)
}

func (r *Runner) backends(ctx context.Context, t Target, rec *metrics.Recorder) ([]Backend, error) {
	port := t.Port
	if port == 0 && t.URL != nil {
		port = defaultPort(t.URL.Scheme)
	}

	if addr, ok := resolve.Literal(t.Host); ok {
		return []Backend{{Addr: addr, Port: port}}, nil
	}

	if !r.PerBackend {
		// Fan-out disabled: behave like an ordinary client and let the OS pick.
		return []Backend{{Port: port}}, nil
	}

	res, err := r.resolveWithFallback(ctx, t.Host, rec)
	if err != nil {
		return nil, err
	}
	if res.TTL > 0 {
		rec.Gauge(SeriesResolveTTL, res.TTL.Seconds())
	}

	addrs := resolve.Limit(res.Addrs, r.MaxBackends)
	if len(addrs) == 0 {
		// An answer without records is no error to the resolver, but nothing to check.
		return nil, fmt.Errorf("%s resolved to no %saddress", t.Host, familyWord(r.Family))
	}
	out := make([]Backend, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, Backend{Addr: a, Port: port})
	}
	return out, nil
}

// resolveWithFallback records the lookup and, when allowed, retries the other family.
func (r *Runner) resolveWithFallback(ctx context.Context, host string, rec *metrics.Recorder) (resolve.Result, error) {
	res, err := r.resolveOnce(ctx, host, r.Family, rec)
	if err == nil || !r.Fallback {
		return res, err
	}
	other := resolve.IPv6
	if r.Family == resolve.IPv6 {
		other = resolve.IPv4
	}
	if r.Family == resolve.Both {
		return res, err
	}
	fallback, ferr := r.resolveOnce(ctx, host, other, rec)
	if ferr != nil {
		return res, err // report the preferred family's failure
	}
	rec.Count(SeriesResolveFallback, 1, "family", string(other))
	return fallback, nil
}

func (r *Runner) resolveOnce(ctx context.Context, host string, family resolve.Family, rec *metrics.Recorder) (resolve.Result, error) {
	res, err := r.Resolver.Resolve(ctx, host, family)
	switch {
	case !res.Cached:
		rec.Count(SeriesResolveTotal, 1, "server", res.Server)
		if err != nil {
			rec.Count(SeriesResolveFail, 1, "server", res.Server)
		} else {
			rec.Duration(TimeResolve, r.Buckets, res.Duration, "server", res.Server)
		}
	case err == nil && res.Duration > 0:
		// A cached answer is no lookup, but the gauge still says how long the last took.
		rec.Gauge(TimeResolve.Last(), res.Duration.Seconds(), "server", res.Server)
	}
	return res, err
}

func (r *Runner) probeOne(ctx context.Context, req Request, rec *metrics.Recorder) bool {
	var res Result
	var elapsed time.Duration
	for range max(1, r.Requests) {
		res, elapsed = r.attempt(ctx, req, rec)
		if !res.OK() {
			break // a failure is the answer; repeating it only adds load
		}
		rec.Count(SeriesSuccess, 1)
	}

	if r.Observe != nil {
		r.Observe(req, res.Err)
	}
	if res.OK() {
		rec.Gauge(SeriesUp, 1)
		r.logResult(req, rec.Base(), true, elapsed, nil)
		return true
	}

	rec.Count(SeriesFailure, 1, "reason", string(ReasonOf(res.Err)))
	rec.Gauge(SeriesUp, 0)
	r.logResult(req, rec.Base(), false, elapsed, res.Err)
	return false
}

// attempt is one request of a run, under a timeout of its own.
func (r *Runner) attempt(ctx context.Context, req Request, rec *metrics.Recorder) (Result, time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	start := time.Now()
	res := r.call(ctx, req, rec)
	elapsed := max(0, time.Since(start)-res.Overhead)

	rec.Count(SeriesTotal, 1)
	rec.Duration(TimeProbe, r.Buckets, elapsed)
	for _, ph := range res.Phases {
		rec.Duration(TimePhase, r.PhaseBuckets, ph.D, "phase", ph.Name)
	}
	if r.Negative {
		res.Err = invert(res.Err)
	}
	return res, elapsed
}

// invert turns a negative test around: a success is the failure worth reporting.
func invert(err error) error {
	if err == nil {
		return Fail(ReasonContent, "negative test: the check succeeded but was expected to fail")
	}
	return nil
}

// logResult writes every outcome at debug and every change at info or warn.
func (r *Runner) logResult(req Request, scope metrics.Labels, ok bool, elapsed time.Duration, err error) {
	attrs := []any{
		"probe", r.Name,
		"type", r.Kind,
		"target", req.Target.Name,
		"duration", elapsed.Round(time.Microsecond).String(),
	}
	if req.Backend.Valid() {
		attrs = append(attrs, "backend", req.Backend.String(), "family", req.Backend.Family())
	}

	if r.Log.Enabled(context.Background(), slog.LevelDebug) {
		detail := attrs
		if err != nil {
			detail = append(append([]any{}, attrs...), "reason", string(ReasonOf(err)), "err", Message(err))
		}
		r.Log.Debug("check "+outcome(ok), detail...)
	}

	if !r.state().changed(unitKey{req.Target.Name, req.Backend.String()}, scope, ok, err) {
		return
	}
	if ok {
		r.Log.Info("backend recovered", attrs...)
		return
	}
	r.Log.Warn("backend is down",
		append(attrs, "reason", string(ReasonOf(err)), "err", Message(err))...)
}

func outcome(ok bool) string {
	if ok {
		return "succeeded"
	}
	return "failed"
}

// call isolates the prober: a panic fails one check, not the agent.
func (r *Runner) call(ctx context.Context, req Request, rec *metrics.Recorder) (res Result) {
	defer func() {
		if v := recover(); v != nil {
			r.panics.Add(1)
			res = Result{Err: Fail(ReasonInternal, "prober panicked: %v", v)}
			r.Log.Error("prober panicked",
				"probe", r.Name, "target", req.Target.Name, "backend", req.Backend.String(),
				"panic", v, "stack", string(debug.Stack()))
		}
	}()
	return r.Prober.Probe(ctx, req, rec)
}

func defaultPort(scheme string) int {
	switch scheme {
	case "https", "wss", "grpc", "grpcs":
		return 443
	case "http", "ws":
		return 80
	}
	return 0
}
