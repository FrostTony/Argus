package domain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func TestSlowRegistryIsATimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	p := newProber(t, "server: "+srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	res := p.Probe(ctx, probe.Request{Target: probe.Target{Name: "example.com", Host: "example.com"}},
		metrics.NewRecorder(metrics.Labels{}))
	if got := probe.ReasonOf(res.Err); got != probe.ReasonTimeout {
		t.Fatalf("reason = %q, want %q (err: %v)", got, probe.ReasonTimeout, res.Err)
	}
}

// min_days below 1 is honoured, not truncated to whole days.
func TestFractionalMinDays(t *testing.T) {
	now := time.Now()
	r := serve(t, answer(now.Add(12*time.Hour), now.AddDate(-1, 0, 0)))
	p := newProber(t, "server: "+r.url+"\nmin_days: 0.9")
	res := p.Probe(context.Background(),
		probe.Request{Target: probe.Target{Name: "example.com", Host: "example.com"}},
		metrics.NewRecorder(metrics.Labels{}))
	if res.Err == nil {
		t.Fatal("12h left with min_days: 0.9 passed: the threshold was truncated to 0 days")
	}
}
