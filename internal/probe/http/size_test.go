package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// The byte count reaches past the read limit so max_size_bytes can fail.
func TestMaxSizeReachesPastTheReadLimit(t *testing.T) {
	const real = 10_000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), real))
	}))
	defer srv.Close()

	p := newProber(t, `
max_body_bytes: 1000
validators:
  - name: not-too-big
    status_code: ["200"]
    max_size_bytes: 5000
`)
	rec := metrics.NewRecorder(metrics.L("probe", "t"))
	res := p.Probe(context.Background(), request(t, srv), rec)

	var size float64
	for _, s := range rec.Samples() {
		if s.Name == "http_response_size_bytes" {
			size = float64(s.Value.(metrics.Gauge))
		}
	}
	if res.OK() || size <= 5000 {
		t.Fatalf("a %d-byte body: http_response_size_bytes = %v, and max_size_bytes: 5000 passed = %v",
			real, size, res.OK())
	}
}
