package probe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

type swapSource struct{ v atomic.Pointer[[]Target] }

func (s *swapSource) Targets() []Target { return *s.v.Load() }

// A relabelled target is the same unit, so the series under its old labels are
// retired.
func TestRelabelledTargetRetiresItsOldSeries(t *testing.T) {
	src := &swapSource{}
	set := func(env string) {
		ts := []Target{{Name: "site", Host: "site.example", Port: 443, Labels: metrics.L("env", env)}}
		src.v.Store(&ts)
	}
	set("a")
	r := newRunner(&fakeProber{fail: Fail(ReasonConnect, "refused")}, "1.1.1.1")
	r.Source = src
	store := metrics.NewStore(10*time.Minute, 0)

	r.RunOnce(context.Background(), store)
	set("b")
	r.RunOnce(context.Background(), store)
	r.RunOnce(context.Background(), store)

	for _, s := range store.Snapshot() {
		if s.Name == SeriesUp && s.Labels.Get("env") == "a" {
			t.Fatalf("the target is env=b now, yet %s{%s} is still exposed", s.Name, s.Labels)
		}
	}
}
