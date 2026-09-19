package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDiscoveryTargetsTolerateExtraFields(t *testing.T) {
	body := `[{"host":"a.example","port":443,"id":17,"owner":"team-x"}]`
	var raw []Target
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("discovery payload with extra fields: %v", err)
	}
}

// resolved() runs only under Probe.UnmarshalYAML, so on this path the node is
// re-serialised with the alias but without its anchor.
func TestDiscoveryTargetsWithAnchors(t *testing.T) {
	body := `
- host: a.example
  labels: &l {team: x}
- host: b.example
  labels: *l
`
	var raw []Target
	if err := yaml.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("target file with an anchor: %v", err)
	}
	if len(raw) != 2 || raw[1].Labels["team"] != "x" {
		t.Fatalf("got %+v", raw)
	}
}
