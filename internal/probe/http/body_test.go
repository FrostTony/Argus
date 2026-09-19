package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// The size is counted whether or not the body is kept.
func TestBodyIsKeptOnlyForValidatorsThatReadIt(t *testing.T) {
	for options, want := range map[string]bool{
		``: false,
		`validators: [{name: s, status_code: ["200"], min_size_bytes: 10}]`: false,
		`validators: [{name: b, body_regex: "ok"}]`:                         true,
		`validators: [{name: j, json_path: "status", json_equals: "ok"}]`:   true,
	} {
		if got := newProber(t, options).(*Prober).keepBody; got != want {
			t.Errorf("%q: keepBody = %v, want %v", options, got, want)
		}
	}
}

func BenchmarkProbeLargeBody(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 512<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	t := &testing.T{}
	p := newProber(t, ``)
	req := request(t, srv)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := metrics.NewRecorder(metrics.L("probe", "bench"))
		if res := p.Probe(context.Background(), req, rec); !res.OK() {
			b.Fatal(res.Err)
		}
	}
}
