package surfacer

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang/snappy"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// A pushed series that merely stops arriving keeps answering with its last
// value for the whole lookback window, so a retired one is ended explicitly.
func TestRemoteWriteEndsRetiredSeries(t *testing.T) {
	var (
		mu       sync.Mutex
		requests [][]decodedSeries
	)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		decoded, err := snappy.Decode(nil, raw)
		if err != nil {
			t.Errorf("snappy: %v", err)
			return
		}
		mu.Lock()
		requests = append(requests, decodeWriteRequest(t, decoded))
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

	gone := metrics.L("probe", "web", "backend", "2.2.2.2:443")
	kept := metrics.L("probe", "web", "backend", "1.1.1.1:443")
	for _, scope := range []metrics.Labels{gone, kept} {
		rec := metrics.NewRecorder(scope)
		rec.Gauge("probe_up", 0)
		rec.Info("tls_version_info", "TLS1.2")
		b := rec.Batch(time.Now())
		b.Scope = scope
		store.Write(context.Background(), b)
	}
	// One backend leaves DNS; the other's new TLS version is a new series under
	// its val label, and the end of the old one.
	store.Write(context.Background(), metrics.Retire(gone, time.Now()))
	rec := metrics.NewRecorder(kept)
	rec.Gauge("probe_up", 1)
	rec.Info("tls_version_info", "TLS1.3")
	b := rec.Batch(time.Now())
	b.Scope = kept
	store.Write(context.Background(), b)

	_ = rw.Close() // flushes once on the way out

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want the endings and then the live series", len(requests))
	}
	ended := map[string]bool{}
	for _, s := range requests[0] {
		if !isStale(s.value) {
			t.Errorf("%v went out among the endings with value %v", s.labels, s.value)
		}
		ended[s.labels["__name__"]+"|"+s.labels["backend"]+"|"+s.labels["val"]] = true
	}
	for _, want := range []string{
		"probe_up|2.2.2.2:443|",
		"tls_version_info|2.2.2.2:443|TLS1.2",
		"tls_version_info|1.1.1.1:443|TLS1.2",
	} {
		if !ended[want] {
			t.Errorf("%s was not ended; got %v", want, ended)
		}
	}
	if ended["probe_up|1.1.1.1:443|"] {
		t.Error("a live series was ended")
	}
	for _, s := range requests[1] {
		if isStale(s.value) {
			t.Errorf("a stale marker among the live series: %v", s.labels)
		}
	}
}

func isStale(v float64) bool { return math.Float64bits(v) == 0x7ff0000000000002 }
