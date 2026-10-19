package probe

import (
	"context"
	"sync"
	"time"
)

// slotsWindow is the span, in seconds, the load of the slots is averaged
// over: the interval most probes run on, so that each of them is in the
// window once whatever its phase.
const slotsWindow = 60

// Slots caps the requests in flight across a node and keeps the slot-time
// held, so that their load reads as an average rather than as an instant.
// An instant is a poor measure: probes on a shared interval leave in a burst,
// and a scrape that falls between bursts reads zero every time however busy
// the node is.
//
// A nil *Slots is no limit at all.
type Slots struct {
	ch    chan struct{}
	clock func() time.Time

	mu      sync.Mutex
	created time.Time
	held    int
	since   time.Time     // when held last changed
	busy    time.Duration // slot-time held before since
	// marks holds the slot-time held up to the start of each of the last
	// slotsWindow seconds, one more than the window so both ends are in it.
	marks  [slotsWindow + 1]time.Duration
	marked int64 // the last second marked
}

// NewSlots makes n slots; fewer than one is one.
func NewSlots(n int) *Slots { return newSlots(n, time.Now) }

func newSlots(n int, clock func() time.Time) *Slots {
	now := clock()
	return &Slots{ch: make(chan struct{}, max(1, n)), clock: clock,
		created: now, since: now, marked: now.Unix()}
}

// Acquire takes a slot, or reports false once the context has ended.
func (s *Slots) Acquire(ctx context.Context) bool {
	if s == nil {
		return ctx.Err() == nil
	}
	select {
	case s.ch <- struct{}{}:
		s.account(+1)
		return true
	case <-ctx.Done():
		return false
	}
}

// Release gives back a slot Acquire took.
func (s *Slots) Release() {
	if s == nil {
		return
	}
	s.account(-1)
	<-s.ch
}

// Cap is how many slots there are; zero for no limit.
func (s *Slots) Cap() int {
	if s == nil {
		return 0
	}
	return cap(s.ch)
}

// InUse is how many slots are taken at this instant.
func (s *Slots) InUse() int {
	if s == nil {
		return 0
	}
	return len(s.ch)
}

// Load is how many slots were in use on average over the last minute, or
// since the slots were made when that is sooner.
func (s *Slots) Load() float64 {
	if s == nil {
		return 0
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mark(now)

	from, base := time.Unix(now.Unix()-slotsWindow, 0), time.Duration(0)
	if from.After(s.created) {
		base = s.marks[from.Unix()%(slotsWindow+1)]
	} else {
		from = s.created
	}
	span := now.Sub(from)
	if span <= 0 {
		return float64(s.held)
	}
	return max(0, float64(s.busyAt(now)-base)/float64(span))
}

func (s *Slots) account(delta int) {
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mark(now) // the seconds passed were held at the count before this change
	s.busy = s.busyAt(now)
	s.held += delta
	s.since = now
}

// busyAt is the slot-time held up to t, t no earlier than since: held has not
// changed in between.
func (s *Slots) busyAt(t time.Time) time.Duration {
	return s.busy + time.Duration(s.held)*max(0, t.Sub(s.since))
}

// mark records the slot-time at the start of every second up to now; seconds
// older than the window are not worth writing.
func (s *Slots) mark(now time.Time) {
	sec := now.Unix()
	for t := max(s.marked+1, sec-slotsWindow); t <= sec; t++ {
		s.marks[t%(slotsWindow+1)] = s.busyAt(time.Unix(t, 0))
	}
	s.marked = max(s.marked, sec)
}
