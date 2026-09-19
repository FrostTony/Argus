package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestDefaultsToJSON(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(Options{}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("started", "probes", 3)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "started" || rec["level"] != "INFO" || rec["probes"] != float64(3) {
		t.Fatalf("record: %v", rec)
	}
}

func TestTextFormatIsReadable(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(Options{Format: FormatText}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Warn("backend is down", "backend", "1.1.1.1:443")

	out := buf.String()
	if !strings.Contains(out, `msg="backend is down"`) || !strings.Contains(out, "backend=1.1.1.1:443") {
		t.Fatalf("text output: %s", out)
	}
	// The date is dropped, leaving the time of day.
	if strings.Contains(out, "T") && strings.Contains(out, "-") {
		t.Fatalf("timestamp was not shortened: %s", out)
	}
}

func TestFieldsAreOnEveryRecord(t *testing.T) {
	var buf bytes.Buffer
	log, _ := New(Options{Fields: []any{"node", "n1", "version", "1.2.3"}}, &buf)
	log.Info("one")
	log.Error("two")

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !strings.Contains(line, `"node":"n1"`) || !strings.Contains(line, `"version":"1.2.3"`) {
			t.Fatalf("record without identity: %s", line)
		}
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(Options{Level: "warn"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("quiet")
	log.Warn("loud")

	out := buf.String()
	if strings.Contains(out, "quiet") {
		t.Fatalf("info passed a warn filter: %s", out)
	}
	if !strings.Contains(out, "loud") {
		t.Fatalf("warn was filtered: %s", out)
	}
}

func TestSourceIsOptionalAndShort(t *testing.T) {
	var buf bytes.Buffer
	log, _ := New(Options{Format: FormatText, Source: true}, &buf)
	log.Info("here")

	out := buf.String()
	if !strings.Contains(out, "logging_test.go:") {
		t.Fatalf("source missing or not shortened: %s", out)
	}
	if strings.Contains(out, "/internal/logging/") {
		t.Fatalf("source kept its full path: %s", out)
	}
}

func TestRejectsUnknownLevelAndFormat(t *testing.T) {
	if _, err := New(Options{Level: "chatty"}, &bytes.Buffer{}); err == nil {
		t.Error("an unknown level was accepted")
	}
	if _, err := New(Options{Format: "yaml"}, &bytes.Buffer{}); err == nil {
		t.Error("an unknown format was accepted")
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":        slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, err)
		}
	}
}
