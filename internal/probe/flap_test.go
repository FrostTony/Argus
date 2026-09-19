package probe

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
)

// familyResolver answers with a duration, a TTL and a server name, failing the
// families it was told to fail.
type familyResolver struct {
	failV4 bool
	calls  atomic.Int64
}

func (f *familyResolver) Resolve(_ context.Context, _ string, fam resolve.Family) (resolve.Result, error) {
	f.calls.Add(1)
	res := resolve.Result{Server: "10.0.0.53:53", Duration: 3 * time.Millisecond, TTL: time.Second}
	if fam == resolve.IPv4 && f.failV4 {
		return res, resolve.ErrNoAddresses
	}
	if fam == resolve.IPv6 {
		res.Addrs = []netip.Addr{netip.MustParseAddr("2606:4700::1")}
		return res, nil
	}
	res.Addrs = []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2.2.2.2")}
	return res, nil
}

type richProber struct{}

func (richProber) Probe(_ context.Context, _ Request, rec *metrics.Recorder) Result {
	var res Result
	res.Add("connect", time.Millisecond)
	res.Add("ttfb", 2*time.Millisecond)
	rec.Gauge("http_status_code", 200)
	rec.Info("http_proto_info", "HTTP/2.0")
	return res
}

type richSelf struct{ richProber }

func (richSelf) SelfAddressed() bool { return true }

func volatileSet(store *metrics.Store) []string {
	var out []string
	for _, s := range store.Snapshot() {
		switch s.Value.(type) {
		case metrics.Gauge, metrics.Info:
			out = append(out, s.Name+"{"+s.Labels.String()+"}")
		}
	}
	sort.Strings(out)
	return out
}

// Between healthy runs the set of gauges and infos must not change, whichever
// code path a run takes.
func TestNoFlapBetweenHealthyRuns(t *testing.T) {
	cases := map[string]func(*Runner){
		"per-backend, cached resolver": func(r *Runner) {},
		"family fallback": func(r *Runner) {
			r.Family, r.Fallback = resolve.IPv4, true
			r.Resolver = resolve.NewCache(&familyResolver{failV4: true}, 0, 0, 0)
		},
		"per_backend off":   func(r *Runner) { r.PerBackend = false },
		"self addressed":    func(r *Runner) { r.Prober = richSelf{} },
		"repeated requests": func(r *Runner) { r.Requests = 3 },
		"literal address":   func(r *Runner) { r.Source = StaticTargets{{Name: "lit", Host: "9.9.9.9", Port: 443}} },
		"negative":          func(r *Runner) { r.Negative = true },
		"two targets": func(r *Runner) {
			r.Source = StaticTargets{{Name: "a", Host: "a.example", Port: 443}, {Name: "b", Host: "b.example", Port: 80}}
		},
		"limited backends": func(r *Runner) { r.MaxBackends = 1 },
		"target with labels": func(r *Runner) {
			r.Source = StaticTargets{{Name: "a", Host: "a.example", Port: 443, Labels: metrics.L("env", "prod")}}
		},
	}
	for name, tweak := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRunner(richProber{})
			r.Resolver = resolve.NewCache(&familyResolver{}, 0, 0, 0)
			tweak(r)

			store := metrics.NewStore(10*time.Minute, 0)
			store.KeepRetired(1024)
			r.RunOnce(context.Background(), store) // real lookup
			want := volatileSet(store)
			if len(want) == 0 {
				t.Fatal("nothing written")
			}
			_, cursor := store.RetiredSince(0)

			r.RunOnce(context.Background(), store) // cache hit
			if got := volatileSet(store); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("cached run changed the series set:\nwant %v\ngot  %v", want, got)
			}
			time.Sleep(1100 * time.Millisecond)    // the cache's floor is one second
			r.RunOnce(context.Background(), store) // real lookup again
			if got := volatileSet(store); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("third run changed the series set:\nwant %v\ngot  %v", want, got)
			}
			s.checkNoRetirements(t, store, cursor)
		})
	}
}

var s retirements

type retirements struct{}

func (retirements) checkNoRetirements(t *testing.T, store *metrics.Store, cursor uint64) {
	t.Helper()
	// RetiredSince hides what came back, so count by cursor movement.
	_, next := store.RetiredSince(cursor)
	if next != cursor {
		gone, _ := store.RetiredSince(cursor)
		t.Fatalf("healthy runs retired %d series (still gone: %v)", next-cursor, gone)
	}
}

// RunNow, the ticker, a status reader and a reload-style cancel, all at once.
func TestRunnerRace(t *testing.T) {
	res := &flaky{addrs: []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2.2.2.2")}}
	p := &gaugeProber{}
	r := newRunner(p)
	r.Resolver = res
	r.Interval = 5 * time.Millisecond
	r.Timeout = 5 * time.Millisecond
	r.Sem = make(chan struct{}, 2)
	store := metrics.NewStore(time.Minute, 0)
	store.KeepRetired(128)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.Start(ctx, store, 0) }()
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				r.RunNow(ctx, store)
				_ = r.Failures()
				_ = r.Down()
				store.Snapshot()
				store.RetiredSince(0)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			res.fail.Store(i%5 == 0)
			p.fail.Store(i%3 == 0)
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(700 * time.Millisecond)
	cancel()
	wg.Wait()

	res.fail.Store(false)
	p.fail.Store(false)
	r.RunOnce(context.Background(), store)
	up := liveGauges(store, SeriesUp)
	if len(up) != 2 || up["1.1.1.1:443"] != 1 || up["2.2.2.2:443"] != 1 {
		t.Fatalf("after a clean run probe_up = %v", up)
	}
	if r.Down() != 0 {
		t.Fatalf("down = %d after a clean run", r.Down())
	}
}

var _ = errors.New
