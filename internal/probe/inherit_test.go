package probe

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// A reload hands on the run of the probe it replaced: the successor neither
// runs beside it nor skips its first run because of it, but waits it out.
func TestSuccessorWaitsForThePredecessorsRun(t *testing.T) {
	newRunner := func(p Prober) *Runner {
		return &Runner{
			Name: "p", Prober: p, Interval: time.Hour, Timeout: time.Minute,
			Source: StaticTargets{{Name: "t", Host: "127.0.0.1"}},
			Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	oldProber := selfAddressed{&fakeProber{delay: 300 * time.Millisecond, ignoreCtx: true}}
	old := newRunner(oldProber)
	if !old.Go(context.Background(), metrics.Fanout{}, func() {}) {
		t.Fatal("the old run did not start")
	}

	nextProber := selfAddressed{&fakeProber{}}
	next := newRunner(nextProber)
	next.Inherit(old)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go next.Start(ctx, metrics.Fanout{}, 0)

	time.Sleep(150 * time.Millisecond)
	if n := nextProber.calls.Load(); n != 0 {
		t.Errorf("the successor ran %d time(s) beside its predecessor", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for nextProber.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if nextProber.calls.Load() != 1 || next.Skipped() != 0 {
		t.Errorf("calls = %d, skipped = %d: want the first run once the old one ended",
			nextProber.calls.Load(), next.Skipped())
	}
}
