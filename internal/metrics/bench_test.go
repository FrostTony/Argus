package metrics

import (
	"context"
	"fmt"
	"testing"
	"time"
)

var (
	tProbeBench = NewTiming("probe")
	tPhaseBench = NewTiming("probe_phase")
)

func benchLabels() Labels {
	return L("node", "n1", "node_region", "eu", "probe", "web", "probe_type", "http",
		"target", "https://example.com/", "backend", "1.2.3.4:443", "family", "ipv4")
}

func BenchmarkLabelsMerge(b *testing.B) {
	base := benchLabels()
	extra := L("phase", "tls")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = base.Merge(extra)
	}
}

func BenchmarkLabelsKey(b *testing.B) {
	l := benchLabels()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = l.Key()
	}
}

// One probe run of one backend emits roughly this many samples.
func BenchmarkRecorderProbeRun(b *testing.B) {
	base := benchLabels()
	buckets := DefaultLatencyBuckets
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := NewRecorder(base)
		r.Count("probe_total", 1)
		r.Count("probe_success_total", 1)
		r.Gauge("probe_up", 1)
		r.Duration(tProbeBench, buckets, 12*time.Millisecond)
		for _, phase := range []string{"connect", "tls", "write", "ttfb", "transfer"} {
			r.Duration(tPhaseBench, buckets, time.Millisecond, "phase", phase)
		}
		r.Gauge("http_status_code", 200)
		r.Info("tls_version_info", "TLS1.3")
		_ = r.Samples()
	}
}

func BenchmarkStoreWrite(b *testing.B) {
	s := NewStore(time.Hour, 0)
	base := benchLabels()
	batch := Batch{Time: time.Now(), Samples: []Sample{
		{Name: "probe_total", Labels: base, Value: Counter(1)},
		{Name: "probe_up", Labels: base, Value: Gauge(1)},
	}}
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(ctx, batch)
	}
}

func BenchmarkStoreWriteScoped(b *testing.B) {
	s := NewStore(time.Hour, 0)
	rec := NewRecorder(benchLabels())
	rec.Count("probe_total", 1)
	rec.Gauge("probe_up", 1)
	rec.Duration(tProbeBench, DefaultLatencyBuckets, 12*time.Millisecond)
	for _, phase := range []string{"connect", "tls", "write", "ttfb", "transfer"} {
		rec.Duration(tPhaseBench, DefaultLatencyBuckets, time.Millisecond, "phase", phase)
	}
	rec.Gauge("http_status_code", 200)
	rec.Info("tls_version_info", "TLS1.3")
	batch := rec.Batch(time.Now())
	batch.Scope, batch.Every = rec.Base(), time.Minute

	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(ctx, batch)
	}
}

func BenchmarkStoreSnapshot(b *testing.B) {
	for _, n := range []int{1_000, 50_000} {
		b.Run(fmt.Sprintf("series=%d", n), func(b *testing.B) {
			s := NewStore(time.Hour, 0)
			ctx := context.Background()
			for i := 0; i < n; i++ {
				s.Write(ctx, Batch{Time: time.Now(), Samples: []Sample{{
					Name:   "probe_duration_seconds",
					Labels: benchLabels().With("backend", fmt.Sprintf("10.0.%d.%d:443", i/256, i%256)),
					Value:  NewDist(DefaultLatencyBuckets),
				}}})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = s.Snapshot()
			}
		})
	}
}

// The whole write path of one backend's run: build the samples, store them.
func BenchmarkProbeRunToStore(b *testing.B) {
	s := NewStore(time.Hour, 0)
	ctx := context.Background()
	base := benchLabels()
	buckets := DefaultLatencyBuckets
	phases := []string{"connect", "tls", "write", "ttfb", "transfer"}
	now := time.Now()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := NewRecorder(base)
		r.Count("probe_total", 1)
		r.Count("probe_success_total", 1)
		r.Gauge("probe_up", 1)
		r.Duration(tProbeBench, buckets, 12*time.Millisecond)
		for _, phase := range phases {
			r.Duration(tPhaseBench, buckets, time.Millisecond, "phase", phase)
		}
		r.Gauge("http_status_code", 200)
		r.Info("tls_version_info", "TLS1.3")
		s.Write(ctx, r.Batch(now))
	}
}
