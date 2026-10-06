package external

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func TestParseLineLabels(t *testing.T) {
	for line, want := range map[string][]string{
		`up 1`:                  nil,
		`up{} 1`:                nil,
		`up{a="x y",b="1,2"} 1`: {"a", "x y", "b", "1,2"},
		`up{ a = "}" , } 1`:     {"a", "}"},
		`up{a="say \"hi\"",b="c:\\d",c="l1\nl2"} 1`: {"a", `say "hi"`, "b", `c:\d`, "c", "l1\nl2"},
		`up{a=bare} 1`: {"a", "bare"},
	} {
		name, labels, rest, err := parseLine(line)
		if err != nil {
			t.Errorf("%s: %v", line, err)
			continue
		}
		if name != "up" || rest != "1" || !slices.Equal(labels, want) {
			t.Errorf("%s: got %q %q %q, want up %q 1", line, name, labels, rest, want)
		}
	}
}

func TestParseLineRejectsMalformedLabels(t *testing.T) {
	for _, line := range []string{
		`up{a="1" 1`,
		`up{a="1 1`,
		`up{a} 1`,
		`up{1a="x"} 1`,
		`up{a="1"b="2"} 1`,
		`up`,
	} {
		if _, _, _, err := parseLine(line); err == nil {
			t.Errorf("%s: accepted", line)
		}
	}
}

func TestQuotedLabelValuesReachTheMetric(t *testing.T) {
	p := newProber(t, "command: "+script(t, `echo 'lag{replica="eu west, 1"} 2'`))
	rec := metrics.NewRecorder(nil)
	if res := p.Probe(context.Background(), request(), rec); !res.OK() {
		t.Fatal(res.Err)
	}
	for _, s := range rec.Samples() {
		if s.Name == "external_lag" && s.Labels.Get("replica") == "eu west, 1" {
			return
		}
	}
	t.Fatalf("external_lag{replica=\"eu west, 1\"} missing from %v", rec.Samples())
}

// A command that prints too much is stopped there, not left to the deadline.
func TestOutputIsCapped(t *testing.T) {
	p := newProber(t, `
command: /bin/sh
args: ["-c", "head -c 2097152 /dev/zero; exec sleep 30"]
self_addressed: true
`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	res := p.Probe(ctx, request(), metrics.NewRecorder(metrics.Labels{}))
	if got := probe.ReasonOf(res.Err); got != probe.ReasonContent {
		t.Fatalf("reason = %q, want %q (err: %v)", got, probe.ReasonContent, res.Err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the probe took %s: the command was not stopped at the cap", took)
	}
}
