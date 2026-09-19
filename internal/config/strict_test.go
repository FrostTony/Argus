package config

import (
	"strings"
	"testing"
	"time"
)

func TestTyposInsideAProbeAreRejected(t *testing.T) {
	cases := map[string]string{
		"schedule.timezon": `
probes:
  - name: a
    type: tcp
    targets: ["db:5432"]
    schedule:
      timezon: Europe/Moscow
      windows: [{days: [mon], from: "09:00", to: "18:00"}]
`,
		"window.form": `
probes:
  - name: a
    type: tcp
    targets: ["db:5432"]
    schedule:
      windows: [{days: [mon], form: "09:00", to: "18:00"}]
`,
		"targets.refesh": `
probes:
  - name: a
    type: tcp
    targets:
      file: {path: /tmp/x.yaml}
      refesh: 10s
`,
		"target.lables": `
probes:
  - name: a
    type: tcp
    targets:
      - host: db
        port: 5432
        lables: {env: prod}
`,
	}
	for name, doc := range cases {
		if _, err := ParseProbes([]byte(doc)); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: want the unknown field rejected, got %v", name, err)
		}
	}
}

// Probes are decoded by re-serialising their nodes, which drops the anchor an
// alias on its own points at.
func TestAliasedOptionsBlock(t *testing.T) {
	doc := `
templates:
  base:
    http: &opts
      method: GET
probes:
  - name: a
    targets: ["https://example.com/"]
    http: *opts
`
	p, err := ParseProbes([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	list, err := p.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	var dst struct {
		Method string `yaml:"method"`
	}
	if err := list[0].DecodeOptions(&dst); err != nil {
		t.Fatalf("DecodeOptions on aliased block: %v", err)
	}
	if dst.Method != "GET" {
		t.Fatalf("method = %q", dst.Method)
	}
}

func TestMergeKeyInsideAProbe(t *testing.T) {
	doc := `
defaults:
  labels: &common {env: prod}
probes:
  - &base
    name: a
    type: tcp
    interval: 30s
    targets: ["db:5432"]
  - <<: *base
    name: b
    labels: *common
`
	p, err := ParseProbes([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := p.List[1]
	if b.Name != "b" || b.Type != "tcp" || b.Interval.D() != 30*time.Second || b.Labels["env"] != "prod" {
		t.Fatalf("merged probe = %+v", b)
	}
}
