package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// BenchmarkProbeCapture is one check of a page, as a schedule runs it and as
// /api/check?capture=true does.
func BenchmarkProbeCapture(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10, 1 << 20} {
		page := []byte(strings.Repeat("<p>x</p>", size/8))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(page)
		}))
		p := newProber(&testing.T{}, `max_body_bytes: 2097152`)
		for _, capture := range []bool{false, true} {
			b.Run(fmt.Sprintf("%dKiB/capture=%v", size>>10, capture), func(b *testing.B) {
				req := request(&testing.T{}, srv)
				if capture {
					req.Capture = &probe.Capture{}
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for b.Loop() {
					res := p.Probe(context.Background(), req, metrics.NewRecorder(metrics.Labels{}))
					if res.Err != nil {
						b.Fatal(res.Err)
					}
				}
			})
		}
		srv.Close()
	}
}

// BenchmarkProbeBodyRegex is a scheduled check that reads the page: what every
// probe with body_regex or json_path pays on every run.
func BenchmarkProbeBodyRegex(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		page := []byte(strings.Repeat("<p>x</p>", size/8) + "ok")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(page)
		}))
		p := newProber(&testing.T{}, "max_body_bytes: 2097152\nvalidators: [{name: v, body_regex: 'ok$'}]")
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			req := request(&testing.T{}, srv)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				if res := p.Probe(context.Background(), req, metrics.NewRecorder(metrics.Labels{})); res.Err != nil {
					b.Fatal(res.Err)
				}
			}
		})
		srv.Close()
	}
}

// BenchmarkProbeJSONPath is a scheduled check with a json_path validator.
func BenchmarkProbeJSONPath(b *testing.B) {
	for _, size := range []int{1 << 10, 64 << 10} {
		var sb strings.Builder
		sb.WriteString(`{"status":"ok","items":[`)
		for i := 0; sb.Len() < size; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"id":%d,"name":"item-%d"}`, i, i)
		}
		sb.WriteString(`]}`)
		page := []byte(sb.String())
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(page) }))
		p := newProber(&testing.T{}, "validators: [{name: v, json_path: status, json_equals: ok}]")
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			req := request(&testing.T{}, srv)
			b.ReportAllocs()
			for b.Loop() {
				if res := p.Probe(context.Background(), req, metrics.NewRecorder(metrics.Labels{})); res.Err != nil {
					b.Fatal(res.Err)
				}
			}
		})
		srv.Close()
	}
}
