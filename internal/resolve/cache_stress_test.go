package resolve

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type slowInner struct{ calls atomic.Int64 }

func (s *slowInner) Resolve(ctx context.Context, host string, _ Family) (Result, error) {
	s.calls.Add(1)
	select {
	case <-time.After(time.Duration(1+rand.Intn(3)) * time.Millisecond):
		return Result{Addrs: []netip.Addr{netip.MustParseAddr("1.1.1.1")}, Server: "s", Duration: time.Millisecond}, nil
	case <-ctx.Done():
		return Result{Server: "s"}, ctx.Err()
	}
}

// A live caller never inherits another's cancellation, nobody parks forever,
// and no abandoned entry is kept.
func TestCacheAbandonStress(t *testing.T) {
	inner := &slowInner{}
	c := NewCache(inner, 0, 0, 0)
	c.MaxEntries = 8
	now := time.Now()
	var tick atomic.Int64
	c.now = func() time.Time { return now.Add(time.Duration(tick.Load()) * time.Second) }

	var wg sync.WaitGroup
	var inherited atomic.Int64
	deadline := time.Now().Add(1500 * time.Millisecond)
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for time.Now().Before(deadline) {
				host := fmt.Sprintf("h%d.example", rng.Intn(12))
				ctx, cancel := context.WithCancel(context.Background())
				doomed := rng.Intn(3) == 0
				if doomed {
					after := time.Duration(rng.Intn(2000)) * time.Microsecond
					go func() { time.Sleep(after); cancel() }()
				}
				_, err := c.Resolve(ctx, host, Both)
				if !doomed && err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					inherited.Add(1)
				}
				cancel()
				if rng.Intn(50) == 0 {
					tick.Add(2)
				}
			}
		}(g)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("resolvers parked forever")
	}
	if n := inherited.Load(); n > 0 {
		t.Fatalf("%d callers with a live context were handed a cancellation", n)
	}
	if n := c.Len(); n > 12 {
		t.Fatalf("cache holds %d entries for 12 names", n)
	}
	c.mu.Lock()
	for k, e := range c.entries {
		if e.inflight != nil {
			t.Errorf("entry %s still in flight after everyone returned", k)
		}
	}
	c.mu.Unlock()
}
