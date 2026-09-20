package probe

import "github.com/tonyamdfrost-cmd/Argus/internal/metrics"

// The series every probe run produces; the prefix is added by the surfacer.
const (
	SeriesTotal           = "probe_total"
	SeriesSuccess         = "probe_success_total"
	SeriesFailure         = "probe_failure_total"
	SeriesUp              = "probe_up"
	SeriesBackends        = "target_backends"
	SeriesBackendsUp      = "target_backends_up"
	SeriesBackendInfo     = "target_backend_info"
	SeriesResolveTotal    = "resolve_total"
	SeriesResolveFail     = "resolve_failure_total"
	SeriesResolveTTL      = "resolve_ttl_seconds"
	SeriesResolveFallback = "resolve_fallback_total"
	// SeriesTimeout is the deadline one request of this probe runs under. It is
	// configuration rather than measurement, but a latency graph without it does
	// not show where the ceiling is.
	SeriesTimeout = "probe_timeout_seconds"
)

// Timings name both series a measurement produces.
var (
	TimeProbe   = metrics.NewTiming("probe")
	TimePhase   = metrics.NewTiming("probe_phase")
	TimeResolve = metrics.NewTiming("resolve")
)

var targetLevel = map[string]bool{
	SeriesTimeout:         true,
	SeriesBackends:        true,
	SeriesBackendsUp:      true,
	SeriesResolveTotal:    true,
	SeriesResolveFail:     true,
	SeriesResolveTTL:      true,
	SeriesResolveFallback: true,
	TimeResolve.Hist():    true,
	TimeResolve.Last():    true,
}

// TargetLevel reports whether a series belongs to the target as a whole.
func TargetLevel(name string) bool { return targetLevel[name] }
