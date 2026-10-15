package metrics

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// An info value is a label, and its text is often the target's.
func TestInfoValuesAreBounded(t *testing.T) {
	rec := NewRecorder(nil)
	rec.Info("long", strings.Repeat("я", 1<<20))
	got := string(rec.Samples()[0].Value.(Info))
	if len(got) > infoMax || !utf8.ValidString(got) || got == "" {
		t.Errorf("%d bytes, valid UTF-8 %v", len(got), utf8.ValidString(got))
	}
}
