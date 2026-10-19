package probe

import (
	"context"
	"errors"
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
	"github.com/tonyamdfrost-cmd/Argus/internal/quota"
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
	// Cron, when set, runs the probe at the wall-clock times it names instead
	// of every Interval from whenever it started; Interval is then the
	// shortest gap between two of its runs.
	Cron *config.Cron
	// Quota, when set, is what every run draws its requests from, on-demand
	// runs included. A run it has no room for is not started.
	Quota *quota.Account
	// Negative inverts the outcome: the check passes when it fails.
	Negative bool
	// Requests repeats the check within one run, for a better latency sample.
	Requests int
	// Hostname is the name every request of this probe presents, whatever the
	// target is called.
	Hostname string

	// Sem caps simultaneous backend requests across the whole node; nil is
	// no cap.
	Sem *Slots
	Log *slog.Logger

	// Capture asks every request of the run for artifacts; see Request.Capture.
	Capture *Capture

	// Observe, when set, receives every outcome with its error intact; the metrics
	// keep only the reason.
	Observe func(req Request, res Result)

	skipped   atomic.Int64
	panics    atomic.Int64
	paused    atomic.Int64
	overQuota atomic.Int64
	sent      atomic.Int64
	// published is the last run that published: when, and what was failing as
	// it did. Taken together, so a reader of the stored results gets one run's
	// samples and that run's failures, not a run still going or one a reload cut.
	published atomic.Pointer[Published]

	// wait replaces waitUntil's sleep in tests, where a minute is long.
	wait func(context.Context, time.Time) bool

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

// Inherit continues from what the runner a reload replaces knew, its prober's
// state included when the prober keeps any. Call it before the first run.
func (r *Runner) Inherit(old *Runner) {
	r.once.Do(func() { r.health = old.state() })
	// The series it wrote stay in the store; so does the run that wrote them.
	r.published.CompareAndSwap(nil, old.published.Load())
	if h, ok := r.Prober.(Heir); ok {
		h.Inherit(old.Prober)
	}
}

// Skipped counts runs dropped because the previous one was still going.
func (r *Runner) Skipped() int64 { return r.skipped.Load() }

// Paused counts runs the schedule suppressed.
func (r *Runner) Paused() int64 { return r.paused.Load() }

// Sleeping reports whether the schedule suppresses this probe.
func (r *Runner) Sleeping() bool { return !r.Schedule.Active(time.Now()) }

// OverQuota counts runs the quota had no room for.
func (r *Runner) OverQuota() int64 { return r.overQuota.Load() }

// Sent counts the requests this runner's runs have sent its targets.
func (r *Runner) Sent() int64 { return r.sent.Load() }

// Published is a run that published its results: when, and the failures
// ("target|backend" to what was wrong) as it published them.
type Published struct {
	At       time.Time
	Failures map[string]Failure
}

// LastPublished is the last run that published; nil before the first.
func (r *Runner) LastPublished() *Published { return r.published.Load() }

// LastRun is when a run last published its results; zero before the first.
func (r *Runner) LastRun() time.Time {
	if p := r.published.Load(); p != nil {
		return p.At
	}
	return time.Time{}
}

// NextRun is when the cron runs the probe next; zero for a probe on an interval.
func (r *Runner) NextRun(now time.Time) time.Time {
	if r.Cron == nil {
		return time.Time{}
	}
	return r.Cron.Next(now)
}

func (r *Runner) Panics() int64 { return r.panics.Load() }

// Start probes until the context is cancelled. The first run is staggered so the
// node's probes do not leave in one wave.
func (r *Runner) Start(ctx context.Context, sink metrics.Sink, jitter float64) {
	if r.Cron != nil {
		r.startCron(ctx, sink)
		return
	}
	if delay := stagger(r.Interval, jitter); delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
	}
	// A reload hands on the last run of the probe this one replaced. It is
	// cancelled and ends soon — a scenario's cleanup aside — and the first run
	// here waits for it rather than skipping, which would leave the changed
	// probe without a result for a whole interval.
	if !r.enter(ctx) {
		return
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()

	// Each run gets its own goroutine so a slow one does not push the schedule back;
	// tick, not the ticker, prevents overlap.
	var wg sync.WaitGroup
	defer wg.Wait()

	run := func(held bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.tick(ctx, sink, held)
		}()
	}

	run(true)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run(false)
		}
	}
}

// startCron runs the probe at its cron's times. There is no stagger and no
// run at start: the point of a cron is that every node runs at the minute it
// names and at no other, a restart and a reload included. The jitter that
// spreads an interval's first run is what a cron is chosen to avoid.
func (r *Runner) startCron(ctx context.Context, sink metrics.Sink) {
	// A reload hands on the last run of the probe this one replaced; the next
	// tick must not overlap it, so its end is waited for here.
	if !r.enter(ctx) {
		return
	}
	r.leave()

	var wg sync.WaitGroup
	defer wg.Wait()
	// last is the time the previous tick was for. Each wait is measured on the
	// wall clock, so a clock stepped back while waiting could otherwise wake
	// before that time is past and name it again.
	var last time.Time
	for {
		from := time.Now()
		if from.Before(last) {
			from = last
		}
		next := r.Cron.Next(from)
		if next.IsZero() {
			// Validation refuses a cron with no next run within its horizon;
			// one reached anyway waits for the configuration to change.
			r.Log.Error("cron names no time ahead; the probe will not run", "probe", r.Name, "cron", r.Cron.String())
			<-ctx.Done()
			return
		}
		if !r.waitUntil(ctx, next) {
			return
		}
		last = next
		// Its own goroutine, as an interval's tick is: a slow run must not
		// push the next one back, and tick prevents overlap.
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.tick(ctx, sink, false)
		}()
	}
}

// waitUntil sleeps until t and reports false if the context ended first.
func (r *Runner) waitUntil(ctx context.Context, t time.Time) bool {
	if r.wait != nil {
		return r.wait(ctx, t)
	}
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RunNow runs immediately unless a run is in progress, and reports whether it
// ran and, if the quota turned it away, why: on-demand runs share health
// state and quota with the scheduled ones.
func (r *Runner) RunNow(ctx context.Context, sink metrics.Sink) (bool, error) {
	if !r.tryEnter() {
		return false, nil
	}
	defer r.leave()
	err := r.Run(ctx, sink)
	if !errors.Is(err, ErrQuota) {
		err = nil
	}
	return true, err
}

// Go starts a run in the background unless one is in progress, and reports
// whether it did; done is called once the run has ended. It is RunNow for a
// run that lasts longer than whoever asked for it is willing to wait.
func (r *Runner) Go(ctx context.Context, sink metrics.Sink, done func()) bool {
	if !r.tryEnter() {
		return false
	}
	go func() {
		defer done()
		defer r.leave()
		r.RunOnce(ctx, sink)
	}()
	return true
}

// Busy reports whether a run is in progress.
func (r *Runner) Busy() bool { return len(r.state().slot) > 0 }

// tryEnter takes the run slot if nothing holds it.
func (r *Runner) tryEnter() bool {
	select {
	case r.state().slot <- struct{}{}:
		return true
	default:
		return false
	}
}

// enter waits for the run slot, or for the context to end.
func (r *Runner) enter(ctx context.Context) bool {
	select {
	case r.state().slot <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Runner) leave() { <-r.state().slot }

// tick skips the run when the previous one is still going. held says the
// slot was already taken for it.
func (r *Runner) tick(ctx context.Context, sink metrics.Sink, held bool) {
	if !r.Schedule.Active(time.Now()) {
		if held {
			r.leave()
		}
		r.paused.Add(1)
		return
	}
	if !held && !r.tryEnter() {
		r.skipped.Add(1)
		r.Log.Warn("previous run still in progress, skipping",
			"probe", r.Name, "interval", r.Interval, "skipped_total", r.skipped.Load())
		return
	}
	defer r.leave()
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

// ErrQuota is what Run returns when the probe's quota has no room for the run.
var ErrQuota = errors.New("quota exhausted")

// QuotaError is ErrQuota with the numbers: what the run needed, what is used
// and when the period turns.
type QuotaError struct {
	Probe string
	Need  int
	Usage quota.Usage
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("probe %q: quota exhausted: %d of %d requests per %s used, a run needs %d; it resets at %s",
		e.Probe, e.Usage.Used, e.Usage.Limit, e.Usage.Per, e.Need, e.Usage.Resets.Format(time.RFC3339))
}

func (e *QuotaError) Is(target error) bool { return target == ErrQuota }

// RunOnce probes every target once. A run the quota refuses is counted and
// logged; see Run for a caller who needs to know.
func (r *Runner) RunOnce(ctx context.Context, sink metrics.Sink) { _ = r.Run(ctx, sink) }

// PerCheck is how many requests one check of one address takes at the least,
// its repeats included.
func (r *Runner) PerCheck() int {
	n := 1
	if c, ok := r.Prober.(Coster); ok {
		n = max(1, c.Cost())
	}
	return n * max(1, r.Requests)
}

// CheckQuota answers whether the quota has room for a check at all: what a
// run started in the background is asked before it is promised. The run
// itself still reserves what it needs once it knows the addresses.
func (r *Runner) CheckQuota(now time.Time) error {
	if out, u := r.QuotaState(now); out {
		return r.refuse(r.PerCheck(), u)
	}
	return nil
}

// QuotaState is the probe's quota as it stands, and whether it is spent:
// too little left for a single check. Without a quota it is zero and false.
func (r *Runner) QuotaState(now time.Time) (bool, quota.Usage) {
	if r.Quota == nil {
		return false, quota.Usage{}
	}
	u := r.Quota.Usage(now)
	return u.Remaining() < int64(r.PerCheck()), u
}

// refuse records a run the quota turned away, logging the first of a period.
func (r *Runner) refuse(need int, u quota.Usage) error {
	r.overQuota.Add(1)
	if r.Quota.FirstRefusal(u.Resets) {
		r.Log.Warn("quota exhausted, runs wait for the next period",
			"probe", r.Name, "used", u.Used, "limit", u.Limit, "per", u.Per,
			"needed", need, "resets", u.Resets.Format(time.RFC3339))
	}
	return &QuotaError{Probe: r.Name, Need: need, Usage: u}
}

// Run probes every target once: resolution first, then the probes. The
// semaphore is acquired before a goroutine starts, so it bounds stacks too.
//
// With a quota, the run is admitted whole or not at all: once resolution says
// how many addresses there are, the run reserves a check of each, and starts
// only if they all fit. What the checks actually sent — redirect hops, a
// scenario's steps — replaces the reservation afterwards.
func (r *Runner) Run(ctx context.Context, sink metrics.Sink) error {
	// An exhausted quota is refused before anything is asked of DNS.
	if err := r.CheckQuota(time.Now()); err != nil {
		return err
	}
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

	var reservedAt time.Time
	need := len(units) * r.PerCheck()
	if r.Quota != nil {
		reservedAt = time.Now()
		if ok, u := r.Quota.Reserve(need, reservedAt); !ok {
			return r.refuse(need, u)
		}
	}

	sent := r.probeAll(ctx, units)
	r.sent.Add(sent)
	r.Quota.Settle(need, int(sent), reservedAt)

	// A cancelled run reached only some backends; publishing would call the rest down.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	now := time.Now()
	// A unit that left DNS or discovery has nobody left to write its series.
	for _, scope := range r.state().prune() {
		sink.Write(ctx, metrics.Retire(scope, now))
	}
	// One lifetime for all the run's series: a cron's next time is the same for
	// every target and backend, and asking it per recorder is a search apiece.
	every := r.every(now)
	for _, tr := range runs {
		if tr.fanout {
			tr.rec.Gauge(SeriesBackendsUp, float64(tr.up))
		}
		r.publish(ctx, sink, tr.rec, now, every)
		for _, rec := range tr.backends {
			r.publish(ctx, sink, rec, now, every)
		}
	}
	// After the writes: the time and failures of a run are not shown before its samples.
	r.published.Store(&Published{At: now, Failures: r.state().failures()})
	return nil
}

// publish ships one recorder as the whole truth about its scope: a gauge the run
// did not write is dropped.
func (r *Runner) publish(ctx context.Context, sink metrics.Sink, rec *metrics.Recorder, now time.Time, every time.Duration) {
	b := rec.Batch(now)
	b.Scope, b.Every = rec.Base(), every
	sink.Write(ctx, b)
}

// every is how long a series written now has to last: until the next run has
// had its time to write it again. For a cron that is the wait for its next
// time, which on "0 9 * * mon-fri" is a weekend — an interval's length would
// let the series of a rare probe expire between its runs.
func (r *Runner) every(now time.Time) time.Duration {
	if r.Cron == nil {
		return r.Interval
	}
	next := r.Cron.Next(now)
	if next.IsZero() {
		return r.Interval
	}
	return next.Sub(now) + time.Duration(max(1, r.Requests))*r.Timeout
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
		err = &Error{Reason: ReasonDNS, Err: err}
		tr.rec.Count(SeriesTotal, 1)
		tr.rec.Count(SeriesFailure, 1, "reason", string(ReasonDNS))
		tr.rec.Gauge(SeriesUp, 0)
		tr.rec.Gauge(SeriesBackends, 0)
		tr.rec.Gauge(SeriesBackendsUp, 0)
		r.Log.Debug("resolution failed", "probe", r.Name, "target", tr.target.Name, "err", Message(err))
		// A cancelled lookup says nothing about the name.
		if ctx.Err() == nil && r.state().changed(unitKey{tr.target.Name, resolveUnit}, tr.rec.Base(), false, err) {
			r.Log.Warn("target stopped resolving",
				"probe", r.Name, "type", r.Kind, "target", tr.target.Name,
				"host", tr.target.Host, "err", Message(err))
		}
		if r.Observe != nil {
			r.Observe(r.request(tr.target, Backend{}), Result{Err: err})
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

// probeAll runs the probe calls within the node's concurrency limit and
// reports how many requests they sent.
func (r *Runner) probeAll(ctx context.Context, units []unit) int64 {
	var (
		wg   sync.WaitGroup
		sent atomic.Int64
	)
	for _, u := range units {
		if !r.acquire(ctx) {
			wg.Wait()
			return sent.Load()
		}
		wg.Add(1)
		go func(u unit) {
			defer wg.Done()
			defer r.release()

			ok, n := r.probeOne(ctx, r.request(u.run.target, u.backend), u.rec)
			sent.Add(int64(n))
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
	wg.Wait()
	return sent.Load()
}

func (r *Runner) request(t Target, b Backend) Request {
	return Request{
		Target: t, Backend: b, Buckets: r.Buckets,
		SourceIP: r.SourceIP, Hostname: r.Hostname, Capture: r.Capture,
	}
}

func (r *Runner) acquire(ctx context.Context) bool { return r.Sem.Acquire(ctx) }

func (r *Runner) release() { r.Sem.Release() }

// resolveWorkers keeps resolution from spawning a goroutine per target.
func (r *Runner) resolveWorkers(targets int) int {
	n := r.Sem.Cap()
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

// TargetLabels are the labels this runner gives a target's own series. Identity
// is merged last: a target label must not move series out of reach of the probe
// that owns them.
func (r *Runner) TargetLabels(t Target) metrics.Labels {
	return r.Labels.Merge(t.Labels).Merge(metrics.L("probe", r.Name, "target", t.Name))
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

// probeOne checks one address, its repeats included, and reports whether it
// passed and how many requests that took.
func (r *Runner) probeOne(ctx context.Context, req Request, rec *metrics.Recorder) (bool, int) {
	var res Result
	var elapsed time.Duration
	sent := 0
	for range max(1, r.Requests) {
		res, elapsed = r.attempt(ctx, req, rec)
		sent += res.Sent()
		if !res.OK() {
			break // a failure is the answer; repeating it only adds load
		}
		rec.Count(SeriesSuccess, 1)
	}

	if r.Observe != nil {
		r.Observe(req, res)
	}
	ok := res.OK()
	if ok {
		rec.Gauge(SeriesUp, 1)
	} else {
		rec.Count(SeriesFailure, 1, "reason", string(ReasonOf(res.Err)))
		rec.Gauge(SeriesUp, 0)
	}
	// A cancelled run says nothing about the backend: its health stays as it was.
	if ctx.Err() == nil {
		r.logResult(req, rec.Base(), ok, elapsed, res.Err)
	}
	return ok, sent
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
