package metrics

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// checkIndex verifies the scope index against the series map.
func checkIndex(t *testing.T, s *Store) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	owned := map[*series]string{}
	for key, sc := range s.scopes {
		if key != sc.key {
			t.Fatalf("scope key mismatch %q vs %q", key, sc.key)
		}
		if len(sc.series) == 0 {
			t.Fatalf("empty scope %q left in the index", key)
		}
		for _, se := range sc.series {
			if se.gone {
				t.Fatalf("scope %q still points at removed series %q", key, se.key)
			}
			if cur, ok := s.series[se.key]; !ok || cur != se {
				t.Fatalf("scope %q points at %q which is not the live series", key, se.key)
			}
			if prev, dup := owned[se]; dup {
				t.Fatalf("series %q is in two scopes: %q and %q", se.key, prev, key)
			}
			owned[se] = key
		}
	}
}

func TestStoreStress(t *testing.T) {
	var clock struct {
		sync.Mutex
		t time.Time
	}
	clock.t = time.Unix(1_700_000_000, 0)
	now := func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return clock.t
	}
	advance := func(d time.Duration) {
		clock.Lock()
		clock.t = clock.t.Add(d)
		clock.Unlock()
	}

	s := NewStore(50*time.Millisecond, 500)
	s.now = now
	s.KeepRetired(64)

	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	buckets := Buckets{0.1, 1}
	other := Buckets{0.2, 2}

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				scope := L("probe", "p", "target", fmt.Sprintf("t%d", rng.Intn(6)), "backend", fmt.Sprintf("b%d", rng.Intn(4)))
				rec := NewRecorder(scope)
				if rng.Intn(3) > 0 {
					rec.Gauge("probe_up", float64(rng.Intn(2)))
				}
				if rng.Intn(2) == 0 {
					rec.Gauge("status", 200)
				}
				if rng.Intn(2) == 0 {
					rec.Info("ver_info", fmt.Sprintf("v%d", rng.Intn(3)))
				}
				rec.Count("probe_total", 1)
				b := buckets
				if rng.Intn(10) == 0 {
					b = other
				}
				rec.Observe("probe_duration_seconds", b, rng.Float64())
				batch := rec.Batch(now())
				switch rng.Intn(5) {
				case 0: // unscoped
				case 1:
					batch = Retire(scope, now())
				default:
					batch.Scope, batch.Every = scope, 10*time.Millisecond
				}
				s.Write(ctx, batch)
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		var cursor uint64
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			snap := s.Snapshot()
			for j := 1; j < len(snap); j++ {
				if snap[j-1].Name > snap[j].Name {
					t.Errorf("snapshot not grouped by name: %q before %q", snap[j-1].Name, snap[j].Name)
					return
				}
			}
			var gone []Sample
			gone, next := s.RetiredSince(cursor)
			if next < cursor {
				t.Errorf("cursor went backwards: %d -> %d", cursor, next)
			}
			_ = gone
			cursor = next
			switch i % 7 {
			case 0:
				s.Sweep()
			case 3:
				s.Drop(func(sm Sample) bool { return sm.Labels.Get("target") == "t3" })
			case 5:
				advance(20 * time.Millisecond)
			}
			s.Len()
		}
	}()

	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()
	checkIndex(t, s)

	// Everything ages out in the end, and the index with it.
	advance(time.Hour)
	s.Sweep()
	checkIndex(t, s)
	if s.Len() != 0 {
		t.Fatalf("series left after everything went stale: %d", s.Len())
	}
	s.mu.Lock()
	scopes, retired := len(s.scopes), len(s.retired)
	s.mu.Unlock()
	if scopes != 0 {
		t.Fatalf("scopes left after everything went stale: %d", scopes)
	}
	if retired > 64+1 {
		t.Fatalf("retired log grew past its cap: %d", retired)
	}
}

// Covers a reader behind a trimmed log, one at its end, and one past it.
func TestRetiredCursor(t *testing.T) {
	s := NewStore(time.Hour, 0)
	s.KeepRetired(8)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		scope := L("backend", fmt.Sprintf("b%d", i))
		rec := NewRecorder(scope)
		rec.Gauge("probe_up", 1)
		b := rec.Batch(time.Now())
		b.Scope = scope
		s.Write(ctx, b)
		s.Write(ctx, Retire(scope, time.Now()))
	}
	gone, next := s.RetiredSince(0)
	if next != 100 {
		t.Fatalf("next = %d, want 100", next)
	}
	if len(gone) == 0 || len(gone) > 8 {
		t.Fatalf("a reader behind the trim got %d entries", len(gone))
	}
	if g, n := s.RetiredSince(next); len(g) != 0 || n != next {
		t.Fatalf("at the end: %d entries, next %d", len(g), n)
	}
	if g, n := s.RetiredSince(next + 1000); len(g) != 0 || n != next {
		t.Fatalf("past the end: %d entries, next %d", len(g), n)
	}
	if g, _ := s.RetiredSince(next - 1); len(g) != 1 || g[0].Labels.Get("backend") != "b99" {
		t.Fatalf("one behind: %v", g)
	}
}
