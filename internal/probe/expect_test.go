package probe

import (
	"regexp"
	"strings"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

func expectSamples(t *testing.T, pattern, text string, extra ...string) []metrics.Sample {
	t.Helper()
	re := regexp.MustCompile(pattern)
	rec := metrics.NewRecorder(nil)
	ExpectInfo(rec, re, re.FindStringSubmatch(text), extra...)
	return rec.Samples()
}

func TestExpectInfoCarriesNamedGroups(t *testing.T) {
	got := expectSamples(t, `Server: nginx/(?P<version>[\d.]+)`, "Server: nginx/1.25.3\r\n", "validator", "banner")
	if len(got) != 1 {
		t.Fatalf("recorded %d samples, want 1: %v", len(got), got)
	}
	s := got[0]
	if s.Name != SeriesExpect {
		t.Errorf("name = %q, want %q", s.Name, SeriesExpect)
	}
	if v := s.Labels.Get("version"); v != "1.25.3" {
		t.Errorf("version = %q, want 1.25.3", v)
	}
	if v := s.Labels.Get("validator"); v != "banner" {
		t.Errorf("validator = %q, want banner", v)
	}
}

// Naming a group is the whole opt-in: text that changes is the most expensive
// thing to put in a label, and an unnamed pattern asked for nothing.
func TestExpectInfoIsSilentWithoutNamedGroups(t *testing.T) {
	if got := expectSamples(t, `nginx/([\d.]+)`, "nginx/1.25.3", "step", "1"); len(got) != 0 {
		t.Errorf("an unnamed group produced %v", got)
	}
	if got := expectSamples(t, `nginx`, "nginx", "step", "1"); len(got) != 0 {
		t.Errorf("a pattern with no groups produced %v", got)
	}
}

func TestExpectInfoBoundsTheValue(t *testing.T) {
	long := strings.Repeat("ы", 500)
	got := expectSamples(t, `(?P<all>.*)`, long)
	if len(got) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(got))
	}
	v := got[0].Labels.Get("all")
	if len(v) > labelMax {
		t.Errorf("label is %d bytes, want at most %d", len(v), labelMax)
	}
	// Cut on a rune boundary, or the exposition carries invalid UTF-8.
	if strings.ContainsRune(v, '�') || !strings.HasPrefix(long, v) {
		t.Errorf("the value was cut mid-rune: %q", v)
	}
}

func TestExpectInfoFlattensNewlines(t *testing.T) {
	got := expectSamples(t, `(?s)(?P<body>.+)`, "one\ntwo")
	if v := got[0].Labels.Get("body"); v != "one two" {
		t.Errorf("body = %q, want %q", v, "one two")
	}
}

// The value is the server's text. A group named after an identity label would
// replace it, and a forged target= travels from here into probe_success, the
// status page and the check's JSON.
func TestExpectInfoCannotForgeIdentityLabels(t *testing.T) {
	re := regexp.MustCompile(`version (?P<target>[0-9.]+) on (?P<host>\w+)`)
	rec := metrics.NewRecorder(metrics.L("probe", "web", "target", "example.com"))
	ExpectInfo(rec, re, re.FindStringSubmatch("version 1.2.3 on box"), "validator", "v1")

	got := rec.Samples()
	if len(got) != 1 {
		t.Fatalf("recorded %d samples, want 1", len(got))
	}
	if v := got[0].Labels.Get("target"); v != "example.com" {
		t.Errorf("target = %q; the server's text replaced the identity label", v)
	}
	// A group that collides with nothing is still exported.
	if v := got[0].Labels.Get("host"); v != "box" {
		t.Errorf("host = %q, want box", v)
	}
}

func TestExcerptCutsOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("ы", 500)
	got := Excerpt(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a shortened excerpt does not say so: %q", got)
	}
	body := strings.TrimSuffix(got, "…")
	if !strings.HasPrefix(long, body) || strings.ContainsRune(body, '�') {
		t.Errorf("the excerpt was cut mid-rune: %q", body)
	}
	if short := "220 ready"; Excerpt(short) != short {
		t.Errorf("a short reply was changed: %q", Excerpt(short))
	}
}

func TestPatternTreatsEmptyAsNone(t *testing.T) {
	re, err := Pattern("")
	if err != nil || re != nil {
		t.Errorf("Pattern(\"\") = %v, %v; want nil, nil", re, err)
	}
	if _, err := Pattern("("); err == nil {
		t.Error("a broken pattern was accepted")
	}
}
