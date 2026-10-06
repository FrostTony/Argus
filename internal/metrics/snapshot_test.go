package metrics

import (
	"context"
	"testing"
)

// A write may land between Snapshot's reorder and its read lock; the series it
// retires must not be shown beside values written after their removal.
func TestSnapshotSkipsSeriesRemovedAfterReorder(t *testing.T) {
	s := NewStore(0, 0)
	ctx := context.Background()
	scope := L("scope", "a")
	s.Write(ctx, Batch{Scope: scope, Samples: []Sample{
		{Name: "gone", Labels: scope, Value: Gauge(1)},
		{Name: "kept", Labels: scope, Value: Gauge(1)},
	}})
	s.Snapshot() // reorders
	s.Write(ctx, Batch{Scope: scope, Samples: []Sample{{Name: "kept", Labels: scope, Value: Gauge(0)}}})

	s.mu.RLock()
	snap := s.snapshotLocked(s.now().UnixNano())
	s.mu.RUnlock()
	if len(snap) != 1 || snap[0].Name != "kept" {
		t.Fatalf("snapshot = %v, want only the kept series", snap)
	}
}
