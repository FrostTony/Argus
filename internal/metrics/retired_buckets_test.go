package metrics

import (
	"context"
	"testing"
	"time"
)

func TestBucketChangeEndsTheOldBuckets(t *testing.T) {
	s := NewStore(time.Hour, 0)
	s.KeepRetired(16)
	l := L("probe", "p")
	write := func(b Buckets) {
		rec := NewRecorder(l)
		rec.Observe("probe_duration_seconds", b, 0.05)
		s.Write(context.Background(), rec.Batch(time.Now()))
	}
	write(Buckets{0.1, 1})
	write(Buckets{0.2, 2})
	_, next := s.RetiredSince(0)
	if next == 0 {
		t.Fatal("the histogram was restarted with new bounds, yet nothing was logged as ended: " +
			`remote storage never gets a stale marker for probe_duration_seconds_bucket{le="0.1"}`)
	}
}
