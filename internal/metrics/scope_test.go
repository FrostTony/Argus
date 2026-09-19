package metrics

import (
	"context"
	"testing"
	"time"
)

func names(samples []Sample) map[string]bool {
	out := make(map[string]bool, len(samples))
	for _, s := range samples {
		out[s.Name] = true
	}
	return out
}

func scoped(scope Labels, fill func(*Recorder)) Batch {
	rec := NewRecorder(scope)
	fill(rec)
	b := rec.Batch(time.Now())
	b.Scope = scope
	return b
}

func TestScopedBatchRetiresWhatItDoesNotRepeat(t *testing.T) {
	s := NewStore(time.Hour, 0)
	backend := L("probe", "web", "backend", "1.1.1.1:443")

	s.Write(context.Background(), scoped(backend, func(r *Recorder) {
		r.Gauge("probe_up", 1)
		r.Gauge("http_status_code", 200)
		r.Info("tls_version_info", "TLS1.3")
		r.Count("probe_total", 1)
		r.Observe("probe_duration_seconds", Buckets{1}, 0.1)
	}))
	s.Write(context.Background(), scoped(backend, func(r *Recorder) {
		r.Gauge("probe_up", 0)
		r.Count("probe_total", 1)
	}))

	got := names(s.Snapshot())
	for _, gone := range []string{"http_status_code", "tls_version_info"} {
		if got[gone] {
			t.Errorf("%s survived a run that did not write it", gone)
		}
	}
	// Totals are not statements about the last run.
	for _, kept := range []string{"probe_up", "probe_total", "probe_duration_seconds"} {
		if !got[kept] {
			t.Errorf("%s was dropped", kept)
		}
	}
}

func TestScopesDoNotRetireEachOther(t *testing.T) {
	s := NewStore(time.Hour, 0)
	target := L("probe", "web", "target", "site")
	backend := target.With("backend", "1.1.1.1:443")

	s.Write(context.Background(), scoped(backend, func(r *Recorder) { r.Gauge("probe_up", 1) }))
	s.Write(context.Background(), scoped(target, func(r *Recorder) { r.Gauge("target_backends", 1) }))
	// No scope, no claim to completeness.
	s.Write(context.Background(), Batch{Samples: []Sample{{Name: "uptime_seconds", Value: Gauge(1)}}})
	s.Write(context.Background(), scoped(target, func(r *Recorder) { r.Gauge("target_backends", 1) }))

	if got := names(s.Snapshot()); !got["probe_up"] || !got["uptime_seconds"] {
		t.Fatalf("a batch retired series outside its scope: %v", got)
	}
}

func TestRetireDropsTheScopesGauges(t *testing.T) {
	s := NewStore(time.Hour, 0)
	backend := L("probe", "web", "backend", "2.2.2.2:443")
	s.Write(context.Background(), scoped(backend, func(r *Recorder) {
		r.Gauge("probe_up", 0)
		r.Count("probe_total", 1)
	}))
	s.Write(context.Background(), Retire(backend, time.Now()))

	got := names(s.Snapshot())
	if got["probe_up"] || !got["probe_total"] {
		t.Fatalf("after Retire: %v, want the gauge gone and the total kept", got)
	}
	if len(s.scopes) != 1 {
		t.Fatalf("scopes = %d, want the one still holding the counter", len(s.scopes))
	}
}

func TestScopeIndexFollowsRemovals(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := NewStore(time.Minute, 0)
	s.now = func() time.Time { return now }

	b := scoped(L("probe", "web", "backend", "1.1.1.1:443"), func(r *Recorder) { r.Count("probe_total", 1) })
	b.Time = now
	s.Write(context.Background(), b)

	now = now.Add(2 * time.Minute)
	if n := s.Sweep(); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if len(s.scopes) != 0 {
		t.Fatalf("scopes = %d after the last series left", len(s.scopes))
	}
}

func TestSeriesOutlivesItsWritersInterval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := NewStore(10*time.Minute, 0)
	s.now = func() time.Time { return now }

	s.Write(context.Background(), Batch{
		Time:    now,
		Every:   12 * time.Hour,
		Samples: []Sample{{Name: "domain_expiry_days", Value: Gauge(300)}},
	})

	now = now.Add(12*time.Hour + 5*time.Minute)
	if len(s.Snapshot()) != 1 {
		t.Fatal("the series went stale before its writer was due back")
	}
	now = now.Add(10 * time.Minute)
	if len(s.Snapshot()) != 0 {
		t.Fatal("the series outlived stale_after past a missed run")
	}
}

func TestHistogramFollowsNewBuckets(t *testing.T) {
	s := NewStore(0, 0)
	observe := func(b Buckets) {
		rec := NewRecorder(L("probe", "p"))
		rec.Observe("probe_duration_seconds", b, 0.5)
		s.Write(context.Background(), rec.Batch(time.Now()))
	}
	observe(Buckets{1, 2})
	observe(Buckets{1, 2})
	observe(Buckets{0.1, 0.25, 1})

	d := s.Snapshot()[0].Value.(*Dist)
	if len(d.Buckets) != 3 || d.Count != 1 {
		t.Fatalf("buckets %v count %d, want the new bounds and a histogram started over", d.Buckets, d.Count)
	}
}
