package resolve

import (
	"context"
	"fmt"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	calls atomic.Int64
	ttl   time.Duration
}

func (f *fake) Resolve(context.Context, string, Family) (Result, error) {
	f.calls.Add(1)
	return Result{
		Addrs:    []netip.Addr{netip.MustParseAddr("1.2.3.4")},
		TTL:      f.ttl,
		Duration: time.Millisecond,
		Server:   "fake",
	}, nil
}

func TestCacheServesFromTTL(t *testing.T) {
	inner := &fake{ttl: time.Minute}
	c := NewCache(inner, 0, time.Second, time.Hour)

	first, err := c.Resolve(context.Background(), "a.example", Both)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached || first.Duration == 0 {
		t.Fatal("the first lookup must be a real one")
	}
	second, _ := c.Resolve(context.Background(), "a.example", Both)
	// A hit is marked Cached and keeps the real lookup's duration.
	if !second.Cached || second.Duration != first.Duration {
		t.Fatalf("cache hit: cached=%v duration=%s, want the real lookup's %s", second.Cached, second.Duration, first.Duration)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("inner resolver called %d times", inner.calls.Load())
	}
}

func TestCacheCollapsesConcurrentLookups(t *testing.T) {
	inner := &fake{ttl: time.Minute}
	c := NewCache(inner, 0, time.Second, time.Hour)

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := c.Resolve(context.Background(), "b.example", Both); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("inner resolver called %d times, want 1", got)
	}
}

func TestCacheEvictsPastTheCap(t *testing.T) {
	c := NewCache(&fake{ttl: time.Minute}, 0, time.Second, time.Hour)
	c.MaxEntries = 10

	for i := 0; i < 100; i++ {
		if _, err := c.Resolve(context.Background(), fmt.Sprintf("h%d.example", i), Both); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Len(); got > c.MaxEntries {
		t.Fatalf("cache holds %d entries, cap is %d", got, c.MaxEntries)
	}
}

// failing answers with an error on the first call and succeeds afterwards.
type failing struct {
	calls atomic.Int64
	err   error
}

func (f *failing) Resolve(context.Context, string, Family) (Result, error) {
	if f.calls.Add(1) == 1 {
		return Result{Server: "fake"}, f.err
	}
	return Result{Addrs: []netip.Addr{netip.MustParseAddr("1.2.3.4")}, Server: "fake"}, nil
}

func TestCachedFailureStaysAFailure(t *testing.T) {
	inner := &failing{err: ErrNoAddresses}
	c := NewCache(inner, 0, time.Minute, time.Hour)

	if _, err := c.Resolve(context.Background(), "a.example", Both); err == nil {
		t.Fatal("the first lookup must report the failure")
	}
	res, err := c.Resolve(context.Background(), "a.example", Both)
	if err == nil {
		t.Fatalf("the cached failure came back as a success: %+v", res)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("the failure was not cached: %d lookups", inner.calls.Load())
	}
}

func TestCacheWaiterDoesNotBlockOnAFinishedLookup(t *testing.T) {
	inner := &slowOnce{started: make(chan struct{}), release: make(chan struct{})}
	c := NewCache(inner, 0, time.Minute, time.Hour)

	go func() { _, _ = c.Resolve(context.Background(), "a.example", Both) }()
	<-inner.started

	// A second caller registers as a waiter, then the leader completes.
	waiting := make(chan error, 1)
	go func() {
		_, err := c.Resolve(context.Background(), "a.example", Both)
		waiting <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(inner.release)

	select {
	case err := <-waiting:
		if err != nil {
			t.Fatalf("the waiter failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never woke: a probe run would be wedged until the node restarts")
	}
}

type slowOnce struct {
	once    atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (s *slowOnce) Resolve(context.Context, string, Family) (Result, error) {
	if s.once.CompareAndSwap(false, true) {
		close(s.started)
		<-s.release
	}
	return Result{Addrs: []netip.Addr{netip.MustParseAddr("1.2.3.4")}, Server: "fake"}, nil
}
