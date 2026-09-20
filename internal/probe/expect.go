package probe

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// SeriesExpect carries what a pattern matched, as labels. blackbox_exporter
// spells it probe_expect_info and Argus has no twin of its own.
const SeriesExpect = "probe_expect_info"

const (
	// labelMax bounds one label. What matched is the server's text, and a
	// server having a bad day can answer with a great deal of it.
	labelMax = 128
	// excerptMax bounds that same text in a failure message.
	excerptMax = 120
)

// Pattern compiles an optional expression; empty means there is none.
func Pattern(s string) (*regexp.Regexp, error) {
	if s == "" {
		return nil, nil
	}
	return regexp.Compile(s)
}

// Named reports whether a pattern asked for anything to be exported. Running a
// pattern a second time to collect its groups is only worth it when it has
// some, and on a large body it is not a free question.
func Named(re *regexp.Regexp) bool {
	if re == nil {
		return false
	}
	for _, name := range re.SubexpNames() {
		if name != "" {
			return true
		}
	}
	return false
}

// ExpectInfo records the named capture groups of a match, one label each.
//
// Named groups only, and that is the whole opt-in: an unnamed pattern says
// nothing about which part of the answer is worth carrying into a time series,
// and a label is the most expensive place to put text that changes.
func ExpectInfo(rec *metrics.Recorder, re *regexp.Regexp, match []string, extra ...string) {
	if rec == nil || re == nil || len(match) == 0 {
		return
	}
	labels := append([]string{}, extra...)
	for i, name := range re.SubexpNames() {
		if name == "" || i >= len(match) {
			continue
		}
		name = metrics.SanitizeName(name)
		// The value is the server's text. A group named after an identity label
		// would replace it, and a forged target= would travel from here into
		// probe_success, the status page and the check's JSON.
		if rec.Base().Get(name) != "" {
			continue
		}
		labels = append(labels, name, cut(flatten(match[i]), labelMax))
	}
	// Only the extra labels means no group was named: nothing was asked for.
	if len(labels) == len(extra) {
		return
	}
	rec.Gauge(SeriesExpect, 1, labels...)
}

// Excerpt is what a server said, short enough to put in a failure message.
func Excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= excerptMax {
		return s
	}
	return cut(s, excerptMax) + "…"
}

func flatten(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// cut shortens on a rune boundary, so the result stays valid UTF-8.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	n := max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
