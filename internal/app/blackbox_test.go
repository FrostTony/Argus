package app

import (
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func aliasIndex(t *testing.T, native []metrics.Sample, regex bool) map[string]metrics.Sample {
	t.Helper()
	out := map[string]metrics.Sample{}
	for _, s := range blackboxAliases(native, func(string) bool { return regex }) {
		out[s.Name+s.Labels.Key()] = s
	}
	return out
}

func gaugeOf(t *testing.T, s metrics.Sample) float64 {
	t.Helper()
	g, ok := s.Value.(metrics.Gauge)
	if !ok {
		t.Fatalf("%s is a %T, want a gauge", s.Name, s.Value)
	}
	return float64(g)
}

func TestBlackboxAliasesTranslateTheSeriesBothMeasure(t *testing.T) {
	base := metrics.L("probe", "web", "probe_type", "http", "target", "example.com")
	backend := base.Merge(metrics.L("backend", "93.184.216.34", "family", "ipv4"))

	native := []metrics.Sample{
		{Name: "http_status_code", Labels: backend, Value: metrics.Gauge(200)},
		{Name: "tls_chain_not_after_seconds", Labels: backend, Value: metrics.Gauge(1800000000)},
		{Name: "tls_version_info", Labels: backend, Value: metrics.Info("TLS1.3")},
		{Name: "http_proto_info", Labels: backend, Value: metrics.Info("HTTP/2.0")},
		{Name: probe.SeriesUp, Labels: backend, Value: metrics.Gauge(1)},
		{Name: probe.TimePhase.Last(), Labels: backend.With("phase", "ttfb"), Value: metrics.Gauge(0.25)},
		{Name: "probe_success", Labels: base, Value: metrics.Gauge(0)},
	}

	got := aliasIndex(t, native, false)

	for name, want := range map[string]float64{
		"probe_http_status_code":         200,
		"probe_ssl_earliest_cert_expiry": 1800000000,
		"probe_http_version":             2,
		"probe_ip_protocol":              4,
	} {
		s, ok := got[name+backend.Key()]
		if !ok {
			t.Errorf("%s is missing; a blackbox dashboard would show nothing", name)
			continue
		}
		if v := gaugeOf(t, s); v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}

	// The phase blackbox calls processing is the one Argus calls ttfb.
	phase := backend.With("phase", "processing")
	if s, ok := got["probe_http_duration_seconds"+phase.Key()]; !ok {
		t.Error("probe_http_duration_seconds{phase=processing} is missing")
	} else if v := gaugeOf(t, s); v != 0.25 {
		t.Errorf("phase duration = %v, want 0.25", v)
	}

	// blackbox keys the version on `version`, spelled with a space.
	version := backend.With("version", "TLS 1.3")
	if _, ok := got["probe_tls_version_info"+version.Key()]; !ok {
		t.Error(`probe_tls_version_info{version="TLS 1.3"} is missing`)
	}
}

// The series exists whether or not a pattern failed: an alert cannot fire on a
// series that appears only once the failure it warns about has happened.
func TestRegexFailureIsAlwaysReported(t *testing.T) {
	base := metrics.L("probe", "web", "probe_type", "http", "target", "example.com")
	native := []metrics.Sample{{Name: "probe_success", Labels: base, Value: metrics.Gauge(0)}}

	for _, tc := range []struct {
		regex bool
		want  float64
	}{{false, 0}, {true, 1}} {
		s, ok := aliasIndex(t, native, tc.regex)["probe_failed_due_to_regex"+base.Key()]
		if !ok {
			t.Fatal("probe_failed_due_to_regex is missing")
		}
		if v := gaugeOf(t, s); v != tc.want {
			t.Errorf("probe_failed_due_to_regex = %v, want %v", v, tc.want)
		}
	}
}

// A series Argus measures differently keeps its own name: a dashboard reading
// an approximation under a familiar name is worse than one reading nothing.
func TestBlackboxAliasesLeaveApproximationsAlone(t *testing.T) {
	base := metrics.L("probe", "web", "probe_type", "http", "target", "example.com")
	native := []metrics.Sample{
		{Name: "http_response_size_bytes", Labels: base, Value: metrics.Gauge(1024)},
		{Name: "tls_cert_expiry_days", Labels: base, Value: metrics.Gauge(30)},
	}
	if got := blackboxAliases(native, func(string) bool { return false }); len(got) != 0 {
		t.Errorf("aliased %d series that have no exact twin: %v", len(got), got)
	}
}
