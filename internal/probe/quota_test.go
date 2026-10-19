package probe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/quota"
)

func withQuota(r *Runner, n int) *quota.Account {
	l := quota.Open("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Quota = l.Account(r.Name, &config.Quota{Requests: n, Per: config.PerDay}, time.UTC)
	return r.Quota
}

func TestQuotaAdmitsWholeRunsOnly(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p, "1.1.1.1", "2.2.2.2") // two addresses: a run is two requests
	acct := withQuota(r, 3)

	if err := r.Run(context.Background(), &collector{}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// One left, two needed: the run does not start, rather than check half.
	err := r.Run(context.Background(), &collector{})
	var qe *QuotaError
	if !errors.Is(err, ErrQuota) || !errors.As(err, &qe) || qe.Need != 2 {
		t.Fatalf("second run: err = %v, want a quota refusal needing 2", err)
	}
	if got := p.calls.Load(); got != 2 {
		t.Fatalf("prober called %d times, want 2", got)
	}
	if u := acct.Usage(time.Now()); u.Used != 2 {
		t.Fatalf("used = %d, want 2", u.Used)
	}
	if r.OverQuota() != 1 || r.Sent() != 2 {
		t.Fatalf("over quota %d, sent %d; want 1 and 2", r.OverQuota(), r.Sent())
	}
}

func TestQuotaRefusesBeforeResolving(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p, "1.1.1.1")
	withQuota(r, 1)
	r.RunOnce(context.Background(), &collector{})

	c := &collector{}
	r.Resolver = emptyResolver{} // would fail the target, were it asked
	if err := r.Run(context.Background(), c); !errors.Is(err, ErrQuota) {
		t.Fatalf("err = %v, want ErrQuota", err)
	}
	if len(c.samples) != 0 {
		t.Fatalf("a refused run published %d samples", len(c.samples))
	}
}

func TestQuotaCountsRepeatsAndWhatTheProberSent(t *testing.T) {
	// A check that followed two redirects sent three requests.
	hops := proberFunc(func(context.Context, Request, *metrics.Recorder) Result { return Result{Requests: 3} })
	r := newRunner(hops, "1.1.1.1")
	r.Requests = 2
	acct := withQuota(r, 100)

	if err := r.Run(context.Background(), &collector{}); err != nil {
		t.Fatal(err)
	}
	if u := acct.Usage(time.Now()); u.Used != 6 {
		t.Fatalf("used = %d, want 2 repeats x 3 requests", u.Used)
	}
}

func TestQuotaReservesWhatACosterSays(t *testing.T) {
	r := newRunner(costly{&fakeProber{}, 4}, "1.1.1.1")
	withQuota(r, 3)
	if err := r.Run(context.Background(), &collector{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("err = %v: a check that takes 4 must not start with 3 left", err)
	}
}

type costly struct {
	*fakeProber
	n int
}

func (c costly) Cost() int { return c.n }

func TestRunNowReportsTheQuota(t *testing.T) {
	r := newRunner(&fakeProber{}, "1.1.1.1")
	withQuota(r, 1)
	if ran, err := r.RunNow(context.Background(), &collector{}); !ran || err != nil {
		t.Fatalf("first: ran=%v err=%v", ran, err)
	}
	if ran, err := r.RunNow(context.Background(), &collector{}); !ran || !errors.Is(err, ErrQuota) {
		t.Fatalf("second: ran=%v err=%v, want the quota named", ran, err)
	}
	if spent, _ := r.QuotaState(time.Now()); !spent {
		t.Fatal("QuotaState does not say the quota is spent")
	}
}

func TestCronRunnerDoesNotRunAtStart(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p, "1.1.1.1")
	cron, err := config.ParseCron("0 0 1 1 *", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	r.Cron = cron

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	r.Start(ctx, &collector{}, 0.5)
	// An interval probe runs at once; a cron waits for its time.
	if got := p.calls.Load(); got != 0 {
		t.Fatalf("a cron probe ran %d times at start", got)
	}
	if next := r.NextRun(time.Now()); next.Month() != time.January || next.Day() != 1 {
		t.Fatalf("next run %s, want the first of January", next)
	}
}

func TestCronSeriesOutliveTheGap(t *testing.T) {
	r := newRunner(&fakeProber{}, "1.1.1.1")
	cron, err := config.ParseCron("0 9 * * mon-fri", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	r.Cron, r.Interval = cron, cron.MinGap()
	friday := time.Date(2026, 10, 23, 9, 0, 5, 0, time.UTC)
	// Written on Friday, read on Monday: the weekend, not a day.
	if got := r.every(friday); got < 71*time.Hour {
		t.Fatalf("a series written on Friday lives %s, want past Monday's run", got)
	}
}

func TestCronRunnerRunsAtItsTimes(t *testing.T) {
	p := &fakeProber{}
	r := newRunner(p, "1.1.1.1")
	cron, err := config.ParseCron("*/5 * * * *", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	r.Cron = cron
	var asked []time.Time
	r.wait = func(_ context.Context, at time.Time) bool {
		asked = append(asked, at)
		// The tick before this one finishes first: they would skip each other.
		deadline := time.Now().Add(5 * time.Second)
		for p.calls.Load() < int64(len(asked)-1) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return len(asked) <= 3
	}
	ctx := context.Background()
	r.Start(ctx, &collector{}, 0)

	if got := p.calls.Load(); got != 3 {
		t.Fatalf("prober called %d times, want once per time waited for", got)
	}
	for _, at := range asked {
		if at.Minute()%5 != 0 || at.Second() != 0 {
			t.Fatalf("waited for %s, which */5 does not name", at)
		}
	}
}
