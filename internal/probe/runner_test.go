package probe

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
)

type fakeProber struct {
	mu        sync.Mutex
	backends  []string
	delay     time.Duration
	panics    bool
	ignoreCtx bool
	fail      error
	calls     atomic.Int64
}

func (f *fakeProber) Probe(ctx context.Context, req Request, _ *metrics.Recorder) Result {
	f.calls.Add(1)
	f.mu.Lock()
	f.backends = append(f.backends, req.Backend.String())
	f.mu.Unlock()
	if f.panics {
		panic("boom")
	}
	if f.delay > 0 {
		if f.ignoreCtx {
			time.Sleep(f.delay)
		} else {
			select {
			case <-time.After(f.delay):
			case <-ctx.Done():
			}
		}
	}
	return Result{Err: f.fail}
}

func (f *fakeProber) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.backends...)
}

type selfAddressed struct{ *fakeProber }

func (selfAddressed) SelfAddressed() bool { return true }

type collector struct {
	mu      sync.Mutex
	samples []metrics.Sample
}

func (c *collector) Write(_ context.Context, b metrics.Batch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, b.Samples...)
}

func (c *collector) find(name string, label, value string) (metrics.Sample, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.samples {
		if s.Name == name && (label == "" || s.Labels.Get(label) == value) {
			return s, true
		}
	}
	return metrics.Sample{}, false
}

func (c *collector) gauge(t *testing.T, name string) float64 {
	t.Helper()
	s, ok := c.find(name, "", "")
	if !ok {
		t.Fatalf("metric %s was not emitted", name)
	}
	return float64(s.Value.(metrics.Gauge))
}

func newRunner(p Prober, addrs ...string) *Runner {
	parsed := make(resolve.Static, 0, len(addrs))
	for _, a := range addrs {
		parsed = append(parsed, netip.MustParseAddr(a))
	}
	return &Runner{
		Name:        "test",
		Kind:        "fake",
		Prober:      p,
		Source:      StaticTargets{{Name: "site", Host: "site.example", Port: 443}},
		Interval:    time.Hour,
		Timeout:     time.Second,
		Resolver:    parsed,
		Family:      resolve.Both,
		MaxBackends: 8,
		PerBackend:  true,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRunnerFansOutAcrossBackends(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p, "1.1.1.1", "2.2.2.2", "2606:4700::1")
	c := &collector{}
	r.RunOnce(context.Background(), c)

	if got := p.calls.Load(); got != 3 {
		t.Fatalf("prober called %d times, want 3", got)
	}
	want := map[string]bool{"1.1.1.1:443": true, "2.2.2.2:443": true, "[2606:4700::1]:443": true}
	for _, b := range p.seen() {
		if !want[b] {
			t.Fatalf("unexpected backend %q", b)
		}
		delete(want, b)
	}
	if len(want) != 0 {
		t.Fatalf("backends never probed: %v", want)
	}
	if got := c.gauge(t, SeriesBackends); got != 3 {
		t.Fatalf("target_backends = %v, want 3", got)
	}
	if got := c.gauge(t, SeriesBackendsUp); got != 3 {
		t.Fatalf("target_backends_up = %v, want 3", got)
	}
}

func TestRunnerCountsPartialOutage(t *testing.T) {
	p := &fakeProber{fail: Fail(ReasonConnect, "refused")}
	r := newRunner(p, "1.1.1.1", "2.2.2.2")
	c := &collector{}
	r.RunOnce(context.Background(), c)

	if got := c.gauge(t, SeriesBackendsUp); got != 0 {
		t.Fatalf("target_backends_up = %v, want 0", got)
	}
	if _, ok := c.find(SeriesFailure, "reason", string(ReasonConnect)); !ok {
		t.Fatal("the failure reason did not reach the metrics")
	}
}

func TestRunnerSurvivesAPanickingProber(t *testing.T) {
	p := &fakeProber{panics: true}
	r := newRunner(p, "1.1.1.1")
	c := &collector{}
	r.RunOnce(context.Background(), c)

	if r.Panics() != 1 {
		t.Fatalf("panics counted: %d, want 1", r.Panics())
	}
	if _, ok := c.find(SeriesFailure, "reason", string(ReasonInternal)); !ok {
		t.Fatal("a panic did not turn into a failed check")
	}
	if got := c.gauge(t, SeriesUp); got != 0 {
		t.Fatalf("probe_up = %v after a panic", got)
	}
}

func TestRunnerSkipsOverlappingRuns(t *testing.T) {
	p := &fakeProber{delay: 150 * time.Millisecond, ignoreCtx: true}
	r := newRunner(p, "1.1.1.1")
	r.Interval = 20 * time.Millisecond
	r.Timeout = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r.Start(ctx, &collector{}, 0)

	if r.Skipped() == 0 {
		t.Fatal("no run was skipped while one was still in flight")
	}
}

// A self-addressed prober is neither resolved nor fanned out.
func TestRunnerSkipsResolutionForSelfAddressed(t *testing.T) {
	inner := &fakeProber{}
	p := selfAddressed{inner}
	r := newRunner(p, "1.1.1.1", "2.2.2.2")
	c := &collector{}
	r.RunOnce(context.Background(), c)

	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("prober called %d times, want 1", got)
	}
	if _, ok := c.find(SeriesBackends, "", ""); ok {
		t.Fatal("a self-addressed probe reported backends")
	}
	if s, ok := c.find(SeriesUp, "", ""); !ok || s.Labels.Get("backend") != "" {
		t.Fatalf("a self-addressed probe got a backend label: %v", s.Labels)
	}
}

// A target that does not resolve is one failed check with reason=dns.
func TestRunnerReportsResolutionFailure(t *testing.T) {
	r := newRunner(&fakeProber{})
	r.Resolver = resolve.Static{}
	c := &collector{}
	r.RunOnce(context.Background(), c)

	if _, ok := c.find(SeriesFailure, "reason", string(ReasonDNS)); !ok {
		t.Fatal("a resolution failure was not reported")
	}
	if got := c.gauge(t, SeriesBackends); got != 0 {
		t.Fatalf("target_backends = %v, want 0", got)
	}
}

func TestRunnerTreatsIPLiteralAsItsOwnBackend(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p)
	r.Source = StaticTargets{{Name: "ip", Host: "9.9.9.9", Port: 53}}
	r.Resolver = nil // resolving would panic
	c := &collector{}
	r.RunOnce(context.Background(), c)

	if got := p.seen(); len(got) != 1 || got[0] != "9.9.9.9:53" {
		t.Fatalf("backends: %v", got)
	}
	if _, ok := c.find(SeriesResolveTotal, "", ""); ok {
		t.Fatal("an IP literal was counted as a resolution")
	}
}

func TestRunnerRespectsConcurrencyLimit(t *testing.T) {
	var inFlight, peak atomic.Int64
	p := &fakeProber{delay: 20 * time.Millisecond}
	r := newRunner(p, "1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4")
	r.Sem = make(chan struct{}, 2)
	r.Prober = proberFunc(func(ctx context.Context, req Request, rec *metrics.Recorder) Result {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		defer inFlight.Add(-1)
		return p.Probe(ctx, req, rec)
	})

	r.RunOnce(context.Background(), &collector{})
	if got := peak.Load(); got > 2 {
		t.Fatalf("%d probes ran at once, limit is 2", got)
	}
}

type proberFunc func(context.Context, Request, *metrics.Recorder) Result

func (f proberFunc) Probe(ctx context.Context, r Request, rec *metrics.Recorder) Result {
	return f(ctx, r, rec)
}

// A negative test passes when the check fails.
func TestRunnerNegativeTest(t *testing.T) {
	failing := &fakeProber{fail: Fail(ReasonConnect, "refused")}
	r := newRunner(failing, "1.1.1.1")
	r.Negative = true
	c := &collector{}
	r.RunOnce(context.Background(), c)

	if got := c.gauge(t, SeriesUp); got != 1 {
		t.Fatalf("probe_up = %v for a failing negative test, want 1", got)
	}

	working := &fakeProber{}
	r2 := newRunner(working, "1.1.1.1")
	r2.Negative = true
	c2 := &collector{}
	r2.RunOnce(context.Background(), c2)

	if got := c2.gauge(t, SeriesUp); got != 0 {
		t.Fatalf("probe_up = %v for a succeeding negative test, want 0", got)
	}
}

func TestRunnerRequestsPerProbe(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p, "1.1.1.1")
	r.Requests = 4
	r.RunOnce(context.Background(), &collector{})

	if got := p.calls.Load(); got != 4 {
		t.Fatalf("prober called %d times, want 4", got)
	}
}

func TestRunnerStopsRepeatingAfterFailure(t *testing.T) {
	p := &fakeProber{fail: Fail(ReasonConnect, "refused")}
	r := newRunner(p, "1.1.1.1")
	r.Requests = 5
	r.RunOnce(context.Background(), &collector{})

	if got := p.calls.Load(); got != 1 {
		t.Fatalf("prober called %d times after a failure, want 1", got)
	}
}

func TestRunnerSchedulePauses(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p, "1.1.1.1")
	r.Interval = 10 * time.Millisecond
	schedule, err := (&config.Schedule{
		Timezone: "UTC",
		Windows:  []config.Window{{From: "00:00", To: "00:01"}},
	}).Compile()
	if err != nil {
		t.Fatal(err)
	}
	r.Schedule = schedule

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	r.Start(ctx, &collector{}, 0)

	if got := p.calls.Load(); got != 0 {
		t.Fatalf("a paused probe ran %d times", got)
	}
	if r.Paused() == 0 {
		t.Fatal("the suppressed runs were not counted")
	}
	if !r.Sleeping() {
		t.Fatal("the probe does not report that it is asleep")
	}
}

// emptyResolver answers without an error and without records.
type emptyResolver struct{}

func (emptyResolver) Resolve(context.Context, string, resolve.Family) (resolve.Result, error) {
	return resolve.Result{Server: "empty"}, nil
}

func TestTargetResolvingToNothingFails(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p)
	r.Resolver = emptyResolver{}
	c := &collector{}
	r.RunOnce(context.Background(), c)

	if got := p.calls.Load(); got != 0 {
		t.Fatalf("the prober was called %d times for a target with no addresses", got)
	}
	if got := c.gauge(t, SeriesUp); got != 0 {
		t.Fatalf("probe_up = %v, want 0", got)
	}
	if got := c.gauge(t, SeriesBackends); got != 0 {
		t.Fatalf("target_backends = %v, want 0", got)
	}
	msg := r.Failures()["site|resolve"]
	if !strings.Contains(msg.Message, "no address") {
		t.Fatalf("failure message = %q, want it to say there was no address", msg.Message)
	}
}

func TestFailureMessageDropsTheReasonPrefix(t *testing.T) {
	p := &fakeProber{fail: Fail(ReasonConnect, "dial tcp 1.1.1.1:443: connection refused")}
	r := newRunner(p, "1.1.1.1")
	r.RunOnce(context.Background(), &collector{})

	why := r.Failures()["site|1.1.1.1:443"]
	if why.Reason != ReasonConnect || strings.HasPrefix(why.Message, "connect:") {
		t.Fatalf("failure = %+v, want the reason beside the message and not inside it", why)
	}
	if !strings.Contains(why.Message, "connection refused") {
		t.Fatalf("message lost the detail: %q", why.Message)
	}
}
