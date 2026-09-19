package config

import (
	"strings"
	"testing"
)

func TestErrorLinesPointIntoTheFile(t *testing.T) {
	doc := strings.Repeat("# padding\n", 40) + `probes:
  - name: x
    type: http
    targets: [a.example]
    requests_per_probe: many
`
	_, err := ParseProbes([]byte(doc))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "line 45") {
		t.Fatalf("the mistake is on line 45 of the file, the error says: %v", err)
	}
}
