package probe

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
)

func counter(c *collector, name string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var sum float64
	for _, s := range c.samples {
		if v, ok := s.Value.(metrics.Counter); ok && s.Name == name {
			sum += float64(v)
		}
	}
	return sum
}

// Success is counted per request, like the total.
func TestRepeatedRequestsCountSuccessPerRequest(t *testing.T) {
	r := newRunner(&fakeProber{}, "1.1.1.1")
	r.Requests = 5
	c := &collector{}
	r.RunOnce(context.Background(), c)
	total, success := counter(c, SeriesTotal), counter(c, SeriesSuccess)
	if total != success {
		t.Fatalf("healthy target: probe_total=%v probe_success_total=%v, ratio %.0f%%", total, success, 100*success/total)
	}
}

func TestEachRequestHasItsOwnTimeout(t *testing.T) {
	p := &slowOK{delay: 40 * time.Millisecond}
	r := newRunner(p, "1.1.1.1")
	r.Timeout = 100 * time.Millisecond
	r.Requests = 4
	c := &collector{}
	r.RunOnce(context.Background(), c)
	if up := c.gauge(t, SeriesUp); up != 1 {
		t.Fatalf("every request took 40ms of a 100ms timeout, yet probe_up=%v", up)
	}
}

type slowOK struct{ delay time.Duration }

func (s *slowOK) Probe(ctx context.Context, _ Request, _ *metrics.Recorder) Result {
	select {
	case <-time.After(s.delay):
		return Result{}
	case <-ctx.Done():
		return Result{Err: Wrap(ReasonTimeout, ctx.Err())}
	}
}

// flaky resolves or fails on demand.
type flaky struct {
	fail  atomic.Bool
	addrs []netip.Addr
}

func (f *flaky) Resolve(context.Context, string, resolve.Family) (resolve.Result, error) {
	if f.fail.Load() {
		return resolve.Result{Server: "static"}, errors.New("SERVFAIL")
	}
	return resolve.Result{Addrs: f.addrs, Server: "static"}, nil
}

// gaugeProber writes a gauge of its own only when it succeeds.
type gaugeProber struct{ fail atomic.Bool }

func (g *gaugeProber) Probe(_ context.Context, _ Request, rec *metrics.Recorder) Result {
	if g.fail.Load() {
		return Result{Err: Fail(ReasonConnect, "refused")}
	}
	rec.Gauge("http_status_code", 200)
	return Result{}
}

func liveGauges(store *metrics.Store, name string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range store.Snapshot() {
		if g, ok := s.Value.(metrics.Gauge); ok && s.Name == name {
			out[s.Labels.Get("backend")] = float64(g)
		}
	}
	return out
}

// The target's own probe_up=0 is retired once the target resolves again.
func TestResolveRecoveryRetiresTheTargetsDown(t *testing.T) {
	res := &flaky{addrs: []netip.Addr{netip.MustParseAddr("1.1.1.1")}}
	r := newRunner(&fakeProber{})
	r.Resolver = res
	store := metrics.NewStore(10*time.Minute, 0)

	res.fail.Store(true)
	r.RunOnce(context.Background(), store)
	res.fail.Store(false)
	r.RunOnce(context.Background(), store)

	for backend, v := range liveGauges(store, SeriesUp) {
		if v == 0 {
			t.Fatalf("target is healthy again, yet probe_up{backend=%q}=0 is still exposed", backend)
		}
	}
}

func TestVanishedBackendIsRetired(t *testing.T) {
	res := &flaky{addrs: []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2.2.2.2")}}
	r := newRunner(&fakeProber{fail: Fail(ReasonConnect, "refused")})
	r.Resolver = res
	store := metrics.NewStore(10*time.Minute, 0)

	r.RunOnce(context.Background(), store)
	res.addrs = res.addrs[:1]
	r.RunOnce(context.Background(), store)

	if _, ok := liveGauges(store, SeriesUp)["2.2.2.2:443"]; ok {
		t.Fatal("2.2.2.2 left DNS, yet its probe_up is still exposed")
	}
}

// A gauge must not outlive the run that wrote it: no http_status_code=200 next
// to probe_up=0.
func TestFailedRunDropsGaugesItDidNotWrite(t *testing.T) {
	p := &gaugeProber{}
	r := newRunner(p, "1.1.1.1")
	store := metrics.NewStore(10*time.Minute, 0)

	r.RunOnce(context.Background(), store)
	p.fail.Store(true)
	r.RunOnce(context.Background(), store)

	if v, ok := liveGauges(store, "http_status_code")["1.1.1.1:443"]; ok {
		t.Fatalf("the check failed at connect, yet http_status_code=%v is still exposed", v)
	}
}

func TestCancelledRunForgetsNothing(t *testing.T) {
	r := newRunner(&fakeProber{fail: Fail(ReasonConnect, "refused")}, "1.1.1.1", "2.2.2.2")
	r.RunOnce(context.Background(), &collector{})
	if r.Down() != 2 {
		t.Fatalf("down=%d, want 2", r.Down())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.RunOnce(ctx, &collector{})
	if r.Down() != 2 {
		t.Fatalf("a cancelled run probed nothing, yet backends_down went from 2 to %d", r.Down())
	}
}

func TestRepeatedPhaseIsOnePhase(t *testing.T) {
	var res Result
	res.Add("read", 10*time.Millisecond)
	res.Add("read", 30*time.Millisecond)
	if len(res.Phases) != 1 || res.Phases[0].D != 40*time.Millisecond {
		t.Fatalf("phases = %+v, want one read phase of 40ms", res.Phases)
	}
}
