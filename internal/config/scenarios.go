package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// ScenarioType is the probe type every scenario file becomes.
const ScenarioType = "scenario"

// Scenarios is where the node finds its scenarios: multi-step checks that act
// on the target the way a user would, kept apart from probes.d because they
// change things and run for minutes.
type Scenarios struct {
	// Paths are files and directories of *.yaml, one scenario per file. A
	// relative path is taken from the directory argus.yaml is in.
	Paths []string `yaml:"paths"`
}

// anchor makes relative paths relative to dir, the directory of the file that
// named them: the node's working directory is an accident of how it started.
func (s *Scenarios) anchor(dir string) {
	for i, p := range s.Paths {
		if p != "" && !filepath.IsAbs(p) {
			s.Paths[i] = filepath.Join(dir, p)
		}
	}
}

// scenarioHead is what a scenario file says about when and where it runs, in
// the words a probe uses. Everything else in the file is the scenario itself,
// which the scenario prober reads.
type scenarioHead struct {
	Name            string            `yaml:"name"`
	Interval        Duration          `yaml:"interval"`
	Timeout         Duration          `yaml:"timeout"`
	Disabled        *bool             `yaml:"disabled"`
	Labels          map[string]string `yaml:"labels"`
	Targets         Targets           `yaml:"targets"`
	RunOn           string            `yaml:"run_on"`
	Schedule        *Schedule         `yaml:"schedule"`
	Cron            Crons             `yaml:"cron"`
	Timezone        string            `yaml:"timezone"`
	Quota           *Quota            `yaml:"quota"`
	SourceIP        string            `yaml:"source_ip"`
	LatencyBuckets  []float64         `yaml:"latency_buckets"`
	PhaseHistograms *bool             `yaml:"phase_histograms"`
}

var headFields = yamlFields(reflect.TypeFor[scenarioHead]())

func isHead(key string) bool {
	_, ok := headFields[key]
	return ok
}

// scenarioFile is one file of the scenarios directory, read as a probe.
type scenarioFile struct{ probe Probe }

func (f *scenarioFile) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a scenario must be a mapping", n.Line)
	}
	n, err := resolved(n)
	if err != nil {
		return err
	}
	head := *n
	head.Content = nil
	rest := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Line: n.Line, Column: n.Column}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		switch {
		case isHead(k.Value):
			head.Content = append(head.Content, k, v)
		case probeFields[k.Value]:
			// A probe option that means nothing here would otherwise reach the
			// prober and be reported as an unknown field of the steps.
			return fmt.Errorf("line %d: %s does not apply to a scenario", k.Line, k.Value)
		default:
			rest.Content = append(rest.Content, k, v)
		}
	}
	if err := knownFields(&head, reflect.TypeFor[scenarioHead]()); err != nil {
		return err
	}
	var h scenarioHead
	if err := head.Decode(&h); err != nil {
		return err
	}
	if h.Cron.Set() && h.Interval != 0 {
		return fmt.Errorf("line %d: interval and cron are alternatives: give one", n.Line)
	}
	f.probe = Probe{
		Name:            h.Name,
		Type:            ScenarioType,
		Interval:        h.Interval,
		Timeout:         h.Timeout,
		Disabled:        h.Disabled,
		Labels:          h.Labels,
		Targets:         h.Targets,
		RunOn:           h.RunOn,
		Schedule:        h.Schedule,
		Cron:            h.Cron,
		Timezone:        h.Timezone,
		Quota:           h.Quota,
		SourceIP:        h.SourceIP,
		LatencyBuckets:  h.LatencyBuckets,
		PhaseHistograms: h.PhaseHistograms,
		Options:         rest,
	}
	return nil
}

// LoadScenarios reads every scenario under paths, one per file. A file's name,
// less its extension, names the scenario unless the file says otherwise. An
// empty directory is no error: a node may have no scenarios yet.
func LoadScenarios(paths ...string) ([]Probe, error) {
	files, err := expandPaths(paths)
	if err != nil {
		return nil, fmt.Errorf("scenarios: %w", err)
	}
	out := make([]Probe, 0, len(files))
	from := make(map[string]string, len(files))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("scenarios: %w", err)
		}
		var f scenarioFile
		if err := decodeDocument(data, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		p := f.probe
		if p.Type == "" {
			return nil, fmt.Errorf("%s: the file is empty", path)
		}
		if p.Name == "" {
			p.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		if p.Dir, err = filepath.Abs(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("scenarios: %w", err)
		}
		if err := p.validateScenario(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if prev, dup := from[p.Name]; dup {
			return nil, fmt.Errorf("%s: scenario %q is already defined in %s", path, p.Name, prev)
		}
		from[p.Name] = path
		out = append(out, p)
	}
	return out, nil
}

// validateScenario is validate plus what a scenario must say for itself. The
// node's interval and timeout suit a request that takes a second; a scenario
// that runs for minutes and changes the target on every run gets neither by
// accident.
func (p Probe) validateScenario() error {
	if (p.Interval == 0 && !p.Cron.Set()) || p.Timeout == 0 {
		return fmt.Errorf("scenario %q: interval and timeout are required; cron may stand for the interval", p.Name)
	}
	if p.Targets.Empty() {
		return fmt.Errorf("scenario %q: targets are missing", p.Name)
	}
	if len(p.Options.Content) == 0 {
		return fmt.Errorf("scenario %q: steps are missing", p.Name)
	}
	if err := p.validate(); err != nil {
		return fmt.Errorf("scenario %q: %w", p.Name, err)
	}
	return nil
}

// DocumentScenarios are the scenarios a check document carries under
// scenarios:, each the mapping a scenario file holds plus the name a file
// takes from its own name.
type DocumentScenarios []Probe

func (d *DocumentScenarios) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return err
	}
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: scenarios is a list, one scenario per item", n.Line)
	}
	out := make(DocumentScenarios, 0, len(n.Content))
	seen := make(map[string]int, len(n.Content))
	for i, item := range n.Content {
		var f scenarioFile
		if err := f.UnmarshalYAML(item); err != nil {
			return fmt.Errorf("scenarios[%d]: %w", i, err)
		}
		p := f.probe
		if p.Name == "" {
			return fmt.Errorf("scenarios[%d]: name is missing", i)
		}
		if prev, dup := seen[p.Name]; dup {
			return fmt.Errorf("scenarios[%d]: scenario %q is already defined by scenarios[%d]", i, p.Name, prev)
		}
		seen[p.Name] = i
		if err := p.validateScenario(); err != nil {
			return fmt.Errorf("scenarios[%d]: %w", i, err)
		}
		out = append(out, p)
	}
	*d = out
	return nil
}

// anchor places the scenarios of a document read from a file beside it, as a
// scenario file's own are. A document pushed through the API has no
// directory, and its scenarios name files by absolute path or not at all.
func (d DocumentScenarios) anchor(file string) error {
	if len(d) == 0 {
		return nil
	}
	dir, err := filepath.Abs(filepath.Dir(file))
	if err != nil {
		return fmt.Errorf("scenarios: %w", err)
	}
	for i := range d {
		d[i].Dir = dir
	}
	return nil
}

// ParseScenario reads one scenario as a document carries it, for the API that
// validates or tries a scenario before it is pushed.
func ParseScenario(data []byte) (Probe, error) {
	var f scenarioFile
	if err := decodeDocument(data, &f); err != nil {
		return Probe{}, err
	}
	p := f.probe
	if p.Type == "" {
		return p, fmt.Errorf("the scenario is empty")
	}
	if p.Name == "" {
		return p, fmt.Errorf("name is missing")
	}
	return p, p.validateScenario()
}
