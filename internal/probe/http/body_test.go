package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
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

// A validator that reads the body would see an empty one.
func TestBodyValidatorsNeedReadBody(t *testing.T) {
	for _, v := range []string{`body_regex: ok`, `body_not_regex: down`, `json_path: status`} {
		if _, err := options("read_body: false\nvalidators:\n  - " + v + "\n"); err == nil {
			t.Errorf("%s accepted with read_body: false", v)
		}
	}
}

func TestMaxSizeHoldsWithoutReadBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 10_000))
	}))
	defer srv.Close()

	p := newProber(t, `
read_body: false
max_body_bytes: 1000
validators:
  - name: small
    max_size_bytes: 5000
`)
	res := p.Probe(context.Background(), request(t, srv), metrics.NewRecorder(metrics.Labels{}))
	if got := probe.ReasonOf(res.Err); got != probe.ReasonContent {
		t.Fatalf("a 10000-byte body against max_size_bytes 5000: reason %q, want %q (err %v)", got, probe.ReasonContent, res.Err)
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
