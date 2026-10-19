package probe

import (
	"context"
	"math"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func near(got, want float64) bool { return math.Abs(got-want) < 0.02 }

// The case the graph showed: at the top of every minute the node's probes
// leave together, hold ten slots for six seconds and are done. Read at any
// moment, the load is the same.
func TestSlotsLoadDoesNotDependOnWhenItIsRead(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []int{0, 3, 6, 10, 45, 59} {
		clock := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
		s := newSlots(128, clock.now)
		for sec := 0; sec < 180+phase; sec++ {
			switch sec % 60 {
			case 0:
				for range 10 {
					s.Acquire(ctx)
				}
			case 6:
				for range 10 {
					s.Release()
				}
			}
			clock.advance(time.Second)
		}
		// Ten slots for six seconds of each minute: one slot on average.
		if got := s.Load(); !near(got, 1) {
			t.Errorf("read %ds into the minute (%d in use): load %.3f, want 1", phase, s.InUse(), got)
		}
	}
}

func TestSlotsLoadBeforeAWholeMinute(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s := newSlots(4, clock.now)
	s.Acquire(context.Background())
	s.Acquire(context.Background())
	clock.advance(time.Second)
	s.Release()
	s.Release()
	clock.advance(time.Second)
	// Two slots for one of the two seconds there have been.
	if got := s.Load(); !near(got, 1) {
		t.Fatalf("load %.3f, want 1", got)
	}
}

func TestSlotsLoadCountsWhatIsHeldNow(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s := newSlots(4, clock.now)
	clock.advance(2 * time.Minute)
	s.Acquire(context.Background())
	clock.advance(30 * time.Second)
	// A run longer than the scrape interval does not read as idle.
	if got := s.Load(); !near(got, 0.5) {
		t.Fatalf("load %.3f with one slot held for half the minute, want 0.5", got)
	}
	clock.advance(2 * time.Minute)
	if got := s.Load(); !near(got, 1) {
		t.Fatalf("load %.3f with one slot held all minute, want 1", got)
	}
}

func TestSlotsLoadForgetsOlderThanAMinute(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	s := newSlots(4, clock.now)
	for range 4 {
		s.Acquire(context.Background())
	}
	clock.advance(5 * time.Minute)
	for range 4 {
		s.Release()
	}
	clock.advance(61 * time.Second)
	if got := s.Load(); got != 0 {
		t.Fatalf("load %.3f a minute after the last slot was given back", got)
	}
}

func TestSlotsFullWaitsForTheContext(t *testing.T) {
	s := NewSlots(1)
	s.Acquire(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if s.Acquire(ctx) {
		t.Fatal("a second slot out of one")
	}
}

func TestNilSlotsAreNoLimit(t *testing.T) {
	var s *Slots
	if !s.Acquire(context.Background()) || s.Cap() != 0 || s.InUse() != 0 || s.Load() != 0 {
		t.Fatal("nil slots")
	}
	s.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.Acquire(ctx) {
		t.Fatal("nil slots must still honour a cancelled context")
	}
}
