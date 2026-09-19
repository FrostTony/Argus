package surfacer

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang/snappy"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// Stale markers the receiver answers 4xx to are given up on: retried first on
// every flush, they would stop the live snapshot from ever leaving.
func TestRejectedStaleMarkersDoNotBlockLiveSamples(t *testing.T) {
	var (
		mu   sync.Mutex
		live int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		decoded, err := snappy.Decode(nil, raw)
		if err != nil {
			t.Errorf("snappy: %v", err)
			return
		}
		for _, s := range decodeWriteRequest(t, decoded) {
			if isStale(s.value) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		mu.Lock()
		live++
		mu.Unlock()
	}))
	defer srv.Close()

	store := metrics.NewStore(time.Hour, 0)
	rw := NewRemoteWrite(RemoteWriteOptions{
		URL:      srv.URL,
		Interval: time.Hour,
		Store:    store,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer rw.Close()

	write := func(scope metrics.Labels, up float64) {
		rec := metrics.NewRecorder(scope)
		rec.Gauge("probe_up", up)
		b := rec.Batch(time.Now())
		b.Scope = scope
		store.Write(context.Background(), b)
	}
	kept := metrics.L("probe", "web", "backend", "1.1.1.1:443")
	gone := metrics.L("probe", "web", "backend", "2.2.2.2:443")
	write(kept, 1)
	write(gone, 0)
	rw.flush()
	mu.Lock()
	before := live
	mu.Unlock()
	if before != 1 {
		t.Fatalf("live requests before the retirement = %d, want 1", before)
	}

	store.Write(context.Background(), metrics.Retire(gone, time.Now()))
	for range 5 {
		write(kept, 1)
		rw.flush()
	}
	mu.Lock()
	defer mu.Unlock()
	if live == before {
		t.Fatalf("after one backend was retired and its stale marker rejected with 400, "+
			"5 flushes delivered %d live requests: the surfacer is wedged (stale queue=%d)",
			live-before, len(rw.stale))
	}
}
