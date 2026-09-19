package metrics

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store folds samples into series: counters add up, gauges and info are
// replaced, distributions merge.
type Store struct {
	mu         sync.RWMutex
	series     map[string]*series
	staleAfter time.Duration
	maxSeries  int
	dropped    atomic.Int64
	now        func() time.Time

	// order is the series sorted by key, rebuilt only when the set changes.
	order []*series
	dirty bool

	// keyBuf and scopeBuf build lookup keys without allocating; guarded by mu.
	keyBuf   []byte
	scopeBuf []byte

	// scopes indexes series by the scope that wrote them.
	scopes map[string]*scope
	// gen numbers the writes; a series not at the current gen was not touched.
	gen uint64

	// retired is a bounded log of removed series, numbered from retiredSeq.
	retired    []Sample
	retiredSeq uint64
	retiredCap int
}

type series struct {
	// key is the map key, duplicated here so sorting compares ready strings.
	key    string
	sample Sample
	// expires is in Unix nanoseconds, and zero when eviction is off.
	expires int64
	gen     uint64
	// gone marks a series removed from the map but still in a scope list.
	gone bool
}

type scope struct {
	key    string
	series []*series
}

// NewStore disables eviction when staleAfter <= 0 and the series cap when
// maxSeries <= 0.
func NewStore(staleAfter time.Duration, maxSeries int) *Store {
	return &Store{
		series:     make(map[string]*series),
		scopes:     make(map[string]*scope),
		staleAfter: staleAfter,
		maxSeries:  maxSeries,
		now:        time.Now,
	}
}

// Write folds a batch into the series it belongs to.
func (s *Store) Write(_ context.Context, b Batch) {
	t := b.Time
	if t.IsZero() {
		t = s.now()
	}
	expires := s.expiry(t, b.Every)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	sc := s.scopeLocked(b.Scope)
	for _, sm := range b.Samples {
		// The key stays a buffer through the lookup: the compiler elides the copy.
		s.keyBuf = sm.Labels.AppendKey(append(append(s.keyBuf[:0], sm.Name...), 0))
		cur, ok := s.series[string(s.keyBuf)]
		if !ok {
			if s.maxSeries > 0 && len(s.series) >= s.maxSeries {
				s.dropped.Add(1)
				continue
			}
			cur = &series{
				key:     string(s.keyBuf),
				sample:  Sample{Name: sm.Name, Labels: sm.Labels, Value: materialize(sm.Value)},
				expires: expires,
				gen:     s.gen,
			}
			s.series[cur.key] = cur
			s.dirty = true
			if sc != nil {
				sc.series = append(sc.series, cur)
			}
			continue
		}
		cur.expires, cur.gen = expires, s.gen
		old := cur.sample
		cur.sample.Value = accumulate(old.Value, sm.Value)
		if !sameSeries(old.Value, cur.sample.Value) {
			s.logRetiredLocked(old)
		}
	}
	if sc != nil {
		s.retireLocked(sc)
	}
}

// scopeLocked finds the scope a batch writes as, creating it on first sight.
func (s *Store) scopeLocked(l Labels) *scope {
	if len(l) == 0 {
		return nil
	}
	s.scopeBuf = l.AppendKey(s.scopeBuf[:0])
	sc, ok := s.scopes[string(s.scopeBuf)]
	if !ok {
		sc = &scope{key: string(s.scopeBuf)}
		s.scopes[sc.key] = sc
	}
	return sc
}

// retireLocked drops the scope's gauges and infos this write did not repeat.
func (s *Store) retireLocked(sc *scope) {
	kept := sc.series[:0]
	for _, se := range sc.series {
		switch {
		case se.gone:
			continue
		case se.gen != s.gen && volatile(se.sample.Value):
			s.removeLocked(se)
			continue
		}
		kept = append(kept, se)
	}
	clear(sc.series[len(kept):])
	sc.series = kept
	if len(kept) == 0 {
		delete(s.scopes, sc.key)
	}
}

// sameSeries reports whether two values of one entry expose the same series: an
// info's text becomes a label and a histogram's bounds become le labels.
func sameSeries(a, b Value) bool {
	switch a := a.(type) {
	case Info:
		return a == b
	case *Dist:
		d, ok := b.(*Dist)
		return ok && sameBuckets(a.Buckets, d.Buckets)
	}
	return true
}

// volatile reports whether a value states the last run rather than a total.
func volatile(v Value) bool {
	k := v.Kind()
	return k == KindGauge || k == KindInfo
}

func (s *Store) removeLocked(se *series) {
	delete(s.series, se.key)
	se.gone = true
	s.dirty = true
	s.logRetiredLocked(se.sample)
}

func (s *Store) logRetiredLocked(sm Sample) {
	if s.retiredCap == 0 {
		return
	}
	if len(s.retired) >= s.retiredCap {
		// Forget the older half; a reader that far behind loses those entries.
		drop := len(s.retired) / 2
		s.retired = append(s.retired[:0], s.retired[drop:]...)
		s.retiredSeq += uint64(drop)
	}
	s.retired = append(s.retired, sm)
}

// KeepRetired makes the store remember the last n series it removed.
func (s *Store) KeepRetired(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retiredCap = max(s.retiredCap, n)
}

// RetiredSince returns the series removed since the cursor and still gone, and
// the cursor to come back with. Start from zero.
func (s *Store) RetiredSince(cursor uint64) ([]Sample, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.retiredSeq + uint64(len(s.retired))
	if cursor >= next {
		return nil, next
	}
	var out []Sample
	for _, sm := range s.retired[max(cursor, s.retiredSeq)-s.retiredSeq:] {
		// A series that is live again is not reported as gone.
		s.keyBuf = sm.Labels.AppendKey(append(append(s.keyBuf[:0], sm.Name...), 0))
		if cur, alive := s.series[string(s.keyBuf)]; !alive || !sameSeries(sm.Value, cur.sample.Value) {
			out = append(out, sm)
		}
	}
	return out, next
}

func (s *Store) compactScopesLocked() {
	for key, sc := range s.scopes {
		sc.series = slices.DeleteFunc(sc.series, func(se *series) bool { return se.gone })
		if len(sc.series) == 0 {
			delete(s.scopes, key)
		}
	}
}

func accumulate(old, add Value) Value {
	switch o := old.(type) {
	case Counter:
		if a, ok := add.(Counter); ok {
			return o + a
		}
	case *Dist:
		// Edited bounds start the histogram over, which reads as a counter reset.
		switch a := add.(type) {
		case Observation:
			if sameBuckets(o.Buckets, a.Buckets) {
				o.Observe(a.Value)
				return o
			}
		case *Dist:
			if sameBuckets(o.Buckets, a.Buckets) {
				o.Merge(a)
				return o
			}
		}
	}
	// The name changed kind: start over rather than stay stuck on the old one.
	return materialize(add)
}

// sameBuckets is slices.Equal with the identical-slice case first.
func sameBuckets(a, b Buckets) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || &a[0] == &b[0] || slices.Equal(a, b)
}

// Snapshot returns live series sorted by name then labels, as the Prometheus
// exposition format requires series of one metric to be adjacent.
func (s *Store) Snapshot() []Sample {
	now := s.now().UnixNano()

	// Reordering takes the write lock, and only a changed series set needs it.
	s.mu.RLock()
	if s.dirty {
		s.mu.RUnlock()
		s.mu.Lock()
		s.reorderLocked()
		s.mu.Unlock()
		s.mu.RLock()
	}
	defer s.mu.RUnlock()

	out := make([]Sample, 0, len(s.order))
	// Histograms are copied so a write during the scrape cannot change them.
	arena := newDistArena(s.order, now)
	for _, se := range s.order {
		if se.stale(now) {
			continue
		}
		v := se.sample.Value
		if d, ok := v.(*Dist); ok {
			v = arena.copy(d)
		}
		out = append(out, Sample{Name: se.sample.Name, Labels: se.sample.Labels, Value: v})
	}
	return out
}

// distArena holds a snapshot's histogram copies in two slices.
type distArena struct {
	dists  []Dist
	counts []uint64
}

func newDistArena(order []*series, now int64) *distArena {
	n, slots := 0, 0
	for _, se := range order {
		if se.stale(now) {
			continue
		}
		if d, ok := se.sample.Value.(*Dist); ok {
			n++
			slots += len(d.Counts)
		}
	}
	return &distArena{dists: make([]Dist, 0, n), counts: make([]uint64, 0, slots)}
}

func (a *distArena) copy(d *Dist) *Dist {
	start := len(a.counts)
	a.counts = append(a.counts, d.Counts...)
	a.dists = append(a.dists, Dist{
		Buckets: d.Buckets,
		Counts:  a.counts[start:len(a.counts):len(a.counts)],
		Sum:     d.Sum,
		Count:   d.Count,
	})
	return &a.dists[len(a.dists)-1]
}

func (s *Store) reorderLocked() {
	if !s.dirty {
		return
	}
	s.order = s.order[:0]
	for _, se := range s.series {
		s.order = append(s.order, se)
	}
	slices.SortFunc(s.order, func(a, b *series) int { return strings.Compare(a.key, b.key) })
	s.dirty = false
}

// Sweep removes stale series for good; Snapshot only hides them.
func (s *Store) Sweep() int {
	if s.staleAfter <= 0 {
		return 0
	}
	now := s.now().UnixNano()
	return s.drop(func(se *series) bool { return se.stale(now) })
}

func (s *Store) Drop(match func(Sample) bool) int {
	return s.drop(func(se *series) bool { return match(se.sample) })
}

func (s *Store) drop(match func(*series) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, se := range s.series {
		if match(se) {
			s.removeLocked(se)
			n++
		}
	}
	if n > 0 {
		s.compactScopesLocked()
	}
	return n
}

// Dropped counts samples refused because the series cap was reached.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.series)
}

// expiry is stale_after past the moment the writer of t is due back.
func (s *Store) expiry(t time.Time, every time.Duration) int64 {
	if s.staleAfter <= 0 {
		return 0
	}
	return t.Add(every + s.staleAfter).UnixNano()
}

func (se *series) stale(now int64) bool { return se.expires != 0 && se.expires < now }
