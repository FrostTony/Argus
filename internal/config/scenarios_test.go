package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const scenarioBody = `
interval: 15m
timeout: 8m
run_on: ^msk-
labels: {team: video}
targets: [https://admin.example/]
steps:
  - {name: login, http: {path: /login}}
`

func TestScenarioFileIsAProbe(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "video-pipeline.yaml", scenarioBody)

	list, err := LoadScenarios(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("scenarios = %d, want 1", len(list))
	}
	sc := list[0]
	if sc.Name != "video-pipeline" || sc.Type != ScenarioType {
		t.Errorf("name %q type %q: the file names the scenario", sc.Name, sc.Type)
	}
	if sc.Interval.D() != 15*time.Minute || sc.Timeout.D() != 8*time.Minute || sc.RunOn != "^msk-" {
		t.Errorf("schedule = %s/%s/%q", sc.Interval, sc.Timeout, sc.RunOn)
	}
	if sc.Labels["team"] != "video" || len(sc.Targets.Static) != 1 {
		t.Errorf("labels %v targets %v", sc.Labels, sc.Targets.Static)
	}
	// Everything that is not about when and where is the prober's.
	var opts struct {
		Steps []map[string]any `yaml:"steps"`
	}
	if err := sc.DecodeOptions(&opts); err != nil || len(opts.Steps) != 1 {
		t.Errorf("options = %+v, %v", opts, err)
	}
}

func TestScenarioFileSaysItsOwnName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yml", "name: uploads\n"+scenarioBody)
	list, err := LoadScenarios(dir)
	if err != nil || list[0].Name != "uploads" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestScenarioFileRefusals(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"probe option that means nothing here": {"hostname: x\n" + scenarioBody, "hostname does not apply to a scenario"},
		"a type":                               {"type: http\n" + scenarioBody, "type does not apply"},
		"no interval": {strings.Replace(scenarioBody, "interval: 15m\n", "", 1),
			"interval and timeout are required"},
		"no targets": {strings.Replace(scenarioBody, "targets: [https://admin.example/]\n", "", 1),
			"targets are missing"},
		"no steps": {"interval: 1m\ntimeout: 30s\ntargets: [https://a.example/]\n", "steps are missing"},
		"timeout past interval": {strings.Replace(scenarioBody, "timeout: 8m", "timeout: 20m", 1),
			"exceeds interval"},
		"unknown head field": {"interval: 1m\ntimeout: 30s\ntargets: {statik: []}\nsteps: []\n", "statik"},
		"not a mapping":      {"- a\n- b\n", "must be a mapping"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := write(t, dir, "s.yaml", tc.body)
			_, err := LoadScenarios(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Errorf("err = %v, want the file and %q", err, tc.want)
			}
		})
	}
}

func TestScenarioNamesAreUnique(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "name: same\n"+scenarioBody)
	write(t, dir, "b.yaml", "name: same\n"+scenarioBody)
	if _, err := LoadScenarios(dir); err == nil || !strings.Contains(err.Error(), "already defined in") {
		t.Errorf("err = %v", err)
	}

	list, err := LoadScenarios(write(t, t.TempDir(), "one.yaml", scenarioBody))
	if err != nil {
		t.Fatal(err)
	}
	p := Probes{
		List:      []Probe{{Name: "one", Type: "http", Targets: list[0].Targets}},
		Scenarios: list,
	}
	if _, err := p.Resolve(); err == nil || !strings.Contains(err.Error(), "already used by probes[0]") {
		t.Errorf("err = %v: a scenario and a probe share the metrics' probe label", err)
	}
}

func TestEmptyScenarioDirectoryIsNoError(t *testing.T) {
	list, err := LoadScenarios(t.TempDir())
	if err != nil || len(list) != 0 {
		t.Errorf("list = %v, err = %v", list, err)
	}
}

func TestScenarioTakesNothingFromDefaults(t *testing.T) {
	list, err := LoadScenarios(write(t, t.TempDir(), "s.yaml", scenarioBody))
	if err != nil {
		t.Fatal(err)
	}
	p := Probes{
		Defaults:  Probe{Timeout: Duration(5 * time.Second), Labels: map[string]string{"env": "prod"}},
		Scenarios: list,
	}
	out, err := p.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Timeout.D() != 8*time.Minute || out[0].Labels["env"] != "" {
		t.Errorf("resolved = %s %v: a default made for one request must not cut a scenario short",
			out[0].Timeout, out[0].Labels)
	}
}

func TestScenarioTypeIsRefusedInProbes(t *testing.T) {
	_, err := ParseProbes([]byte(`
probes:
  - {name: s, type: scenario, targets: ["https://a.example/"], scenario: {steps: []}}
`))
	if err == nil || !strings.Contains(err.Error(), "scenarios.paths") {
		t.Errorf("err = %v: a scenario lives in its own file", err)
	}
}

func TestScenarioPathsAreTakenFromTheServerFile(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "argus.yaml", "scenarios:\n  paths: [scenarios.d, /abs/elsewhere]\n")
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "scenarios.d"), "/abs/elsewhere"}
	if strings.Join(cfg.Scenarios.Paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", cfg.Scenarios.Paths, want)
	}
}
