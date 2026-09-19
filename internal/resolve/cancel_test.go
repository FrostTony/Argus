package resolve

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

type ctxResolver struct{}

func (ctxResolver) Resolve(ctx context.Context, _ string, _ Family) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Addrs: []netip.Addr{netip.MustParseAddr("1.1.1.1")}}, nil
}

func TestCancelledLookupIsNotCached(t *testing.T) {
	c := NewCache(ctxResolver{}, 0, 5*time.Second, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = c.Resolve(ctx, "example.com", Both)

	if _, err := c.Resolve(context.Background(), "example.com", Both); err != nil {
		t.Fatalf("a healthy caller was handed someone else's cancellation: %v", err)
	}
}

// slowResolver answers only once released, and fails if its caller left first.
type slowResolver struct{ release chan struct{} }

func (s slowResolver) Resolve(ctx context.Context, _ string, _ Family) (Result, error) {
	select {
	case <-s.release:
		return Result{Addrs: []netip.Addr{netip.MustParseAddr("1.1.1.1")}}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func TestWaitersSurviveAnAbandonedLookup(t *testing.T) {
	inner := slowResolver{release: make(chan struct{})}
	c := NewCache(inner, 0, 5*time.Second, time.Minute)

	leader, cancel := context.WithCancel(context.Background())
	go func() { _, _ = c.Resolve(leader, "example.com", Both) }()
	for c.Len() == 0 {
		time.Sleep(time.Millisecond)
	}

	got := make(chan error, 1)
	go func() {
		_, err := c.Resolve(context.Background(), "example.com", Both)
		got <- err
	}()
	time.Sleep(10 * time.Millisecond) // let the waiter park on the leader
	cancel()
	close(inner.release)

	if err := <-got; err != nil {
		t.Fatalf("the waiter inherited the leader's cancellation: %v", err)
	}
}
