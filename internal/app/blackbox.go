package app

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// This file renders a run a second time under blackbox_exporter's names, for
// /probe only: /metrics would pay for one measurement under two names on every
// backend of every probe, forever.
//
// Only exact twins are translated. An approximation under a familiar name is
// worse than a missing series: the dashboard shows a number, and nobody checks
// which number it is.

// blackboxRenames are series whose value carries over untouched.
var blackboxRenames = map[string]string{
	"http_status_code":              "probe_http_status_code",
	"http_content_length":           "probe_http_content_length",
	"http_redirects":                "probe_http_redirects",
	"resolve_last_duration_seconds": "probe_dns_lookup_time_seconds",
	// blackbox publishes the duration as a gauge; the Argus series under that
	// name is a histogram, so the gauge beside it is what carries over.
	"probe_last_duration_seconds": "probe_duration_seconds",
	"dns_answers":                 "probe_dns_answer_rrs",
	"dns_authority_rrs":           "probe_dns_authority_rrs",
	"dns_additional_rrs":          "probe_dns_additional_rrs",
	"icmp_reply_hop_limit":        "probe_icmp_reply_hop_limit",
	"http_ssl":                    "probe_http_ssl",
	"http_last_modified_seconds":  "probe_http_last_modified_timestamp_seconds",
}

// blackboxPhases renames the phases Argus and blackbox both measure but spell
// differently. A phase absent here carries its own name: an extra phase in the
// series is harmless, an Argus phase reported as a blackbox one is not.
var blackboxPhases = map[string]string{"ttfb": "processing"}

// phaseSeries is the per-probe-type name blackbox splits its timings under.
var phaseSeries = map[string]string{
	"http": "probe_http_duration_seconds",
	"dns":  "probe_dns_duration_seconds",
	"icmp": "probe_icmp_duration_seconds",
}

// blackboxAliases returns the extra samples, given the run's native ones.
func blackboxAliases(native []metrics.Sample, now time.Time, regexFailed func(target string) bool) []metrics.Sample {
	out := make([]metrics.Sample, 0, len(native)/4)
	for _, s := range native {
		switch {
		case blackboxRenames[s.Name] != "":
			out = append(out, metrics.Sample{Name: blackboxRenames[s.Name], Labels: s.Labels, Value: s.Value})

		case s.Name == probe.SeriesUp:
			// blackbox reports the family it reached the target over as a number.
			if v, ok := ipProtocol(s.Labels.Get("family")); ok {
				out = append(out, metrics.Sample{Name: "probe_ip_protocol", Labels: s.Labels, Value: metrics.Gauge(v)})
			}

		case s.Name == probe.TimePhase.Last():
			if alias, ok := phaseAlias(s); ok {
				out = append(out, alias)
			}

		case s.Name == "tls_chain_expiry_days":
			// blackbox has the date itself. The days left were counted moments
			// ago from a whole-second notAfter, so the date comes back exact.
			if days, ok := s.Value.(metrics.Gauge); ok {
				out = append(out, metrics.Sample{
					Name:   "probe_ssl_earliest_cert_expiry",
					Labels: s.Labels,
					Value:  metrics.Gauge(expiryDate(now, float64(days))),
				})
			}

		case s.Name == "http_proto_info":
			if v, ok := httpVersion(text(s.Value)); ok {
				out = append(out, metrics.Sample{Name: "probe_http_version", Labels: s.Labels, Value: metrics.Gauge(v)})
			}

		case s.Name == "tls_version_info":
			// A gauge with the version as a label, which is how blackbox has it:
			// an info sample here would be rendered under val=, not version=.
			out = append(out, metrics.Sample{
				Name:   "probe_tls_version_info",
				Labels: s.Labels.With("version", spacedTLS(text(s.Value))),
				Value:  metrics.Gauge(1),
			})

		case s.Name == "probe_success":
			// Always emitted, 0 and all: an alert cannot fire on a series that
			// only exists once the thing it warns about has already happened.
			out = append(out, metrics.Sample{
				Name:   "probe_failed_due_to_regex",
				Labels: s.Labels,
				Value:  metrics.Gauge(metrics.Bool(regexFailed(s.Labels.Get("target")))),
			})
		}
	}
	return out
}

// phaseAlias translates one phase gauge, if the probe type has a blackbox twin.
func phaseAlias(s metrics.Sample) (metrics.Sample, bool) {
	name, ok := phaseSeries[s.Labels.Get("probe_type")]
	if !ok {
		return metrics.Sample{}, false
	}
	phase := s.Labels.Get("phase")
	if alias, ok := blackboxPhases[phase]; ok {
		s.Labels = s.Labels.With("phase", alias)
	}
	return metrics.Sample{Name: name, Labels: s.Labels, Value: s.Value}, true
}

func ipProtocol(family string) (float64, bool) {
	switch family {
	case "ipv4":
		return 4, true
	case "ipv6":
		return 6, true
	}
	return 0, false
}

// httpVersion turns "HTTP/1.1" into the 1.1 blackbox reports.
func httpVersion(proto string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimPrefix(proto, "HTTP/"), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// spacedTLS is "TLS1.3" as blackbox spells it.
func spacedTLS(v string) string {
	if rest, ok := strings.CutPrefix(v, "TLS"); ok && rest != "" {
		return "TLS " + rest
	}
	return v
}

// expiryDate turns days left at now into a unix timestamp, to the second.
func expiryDate(now time.Time, days float64) float64 {
	return math.Round(float64(now.UnixMilli())/1e3 + days*86400)
}

func text(v metrics.Value) string {
	s, _ := v.(metrics.Info)
	return string(s)
}
