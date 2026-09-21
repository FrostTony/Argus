// Package config is the file format: the node's own settings and the check
// configuration, with the merge rules that turn defaults, templates and probes
// into the final list. Decoding is strict and the parsed structures stay data.
package config

import (
	"cmp"
	"fmt"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// MaxRequestsPerProbe caps how many times one run may repeat its check.
const MaxRequestsPerProbe = 100

// Probes is the check configuration: defaults -> template -> probe.
type Probes struct {
	Defaults  Probe            `yaml:"defaults"`
	Templates map[string]Probe `yaml:"templates"`
	List      []Probe          `yaml:"probes"`
}

// Probe holds the prober's own parameters under a key named after its type
// (`http:`, `dns:`), kept as an unparsed YAML node the prober decodes itself.
type Probe struct {
	Name     string   `yaml:"name,omitempty"`
	Type     string   `yaml:"type,omitempty"`
	Template string   `yaml:"template,omitempty"`
	Interval Duration `yaml:"interval,omitempty"`
	Timeout  Duration `yaml:"timeout,omitempty"`
	// Every flag below is a pointer so an inherited true stays refusable.
	Disabled       *bool             `yaml:"disabled,omitempty"`
	PerBackend     *bool             `yaml:"per_backend,omitempty"`
	Labels         map[string]string `yaml:"labels,omitempty"`
	Targets        Targets           `yaml:"targets,omitempty"`
	LatencyBuckets []float64         `yaml:"latency_buckets,omitempty"`
	// PhaseHistograms overrides the node's probing.phase_histograms.
	PhaseHistograms *bool `yaml:"phase_histograms,omitempty"`

	// IPVersion overrides the node's address family for this probe.
	IPVersion string `yaml:"ip_version,omitempty"`
	// IPFallback tries the other family when the preferred one has no address.
	IPFallback *bool `yaml:"ip_fallback,omitempty"`
	// SourceIP binds outgoing connections to one local address.
	SourceIP string `yaml:"source_ip,omitempty"`

	// Hostname is the name presented to the target — Host header and SNI —
	// when it differs from the target's own. blackbox spells it hostname=.
	Hostname string `yaml:"hostname,omitempty"`

	// Schedule limits when the probe runs.
	Schedule *Schedule `yaml:"schedule,omitempty"`
	// NegativeTest inverts the outcome: the check passes when it fails.
	NegativeTest *bool `yaml:"negative_test,omitempty"`
	// RequestsPerProbe repeats the check within one interval.
	RequestsPerProbe int `yaml:"requests_per_probe,omitempty"`
	// RunOn is a regular expression over the node name; empty runs everywhere.
	RunOn string `yaml:"run_on,omitempty"`

	Options yaml.Node `yaml:"-"`
}

// probeFields are the keys a probe answers to itself; anything else is the
// block named after its type. Derived from the struct rather than listed, so
// that a new common option cannot be read as somebody's prober block.
var probeFields = func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeFor[Probe]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}()

func (p *Probe) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a probe must be a mapping", n.Line)
	}
	n, err := resolved(n)
	if err != nil {
		return err
	}
	type plain Probe
	var known plain
	dec := *n
	dec.Content = nil
	var extra []*yaml.Node

	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if probeFields[k.Value] {
			dec.Content = append(dec.Content, k, v)
			continue
		}
		extra = append(extra, k, v)
	}
	// Node.Decode cannot enforce known fields, so nested keys are checked here.
	if err := knownFields(&dec, reflect.TypeFor[plain]()); err != nil {
		return err
	}
	if err := dec.Decode(&known); err != nil {
		return err
	}
	*p = Probe(known)

	switch len(extra) {
	case 0:
	case 2:
		key, value := extra[0], extra[1]
		if p.Type != "" && p.Type != key.Value {
			return fmt.Errorf("line %d: block %q does not match type %q", key.Line, key.Value, p.Type)
		}
		p.Type = cmp.Or(p.Type, key.Value)
		p.Options = *value
	default:
		names := make([]string, 0, len(extra)/2)
		for i := 0; i < len(extra); i += 2 {
			names = append(names, extra[i].Value)
		}
		return fmt.Errorf("line %d: a probe takes one parameter block, found: %s",
			n.Line, strings.Join(names, ", "))
	}
	return nil
}

// MarshalYAML puts the prober block back under its type name.
func (p Probe) MarshalYAML() (any, error) {
	type plain Probe
	node := &yaml.Node{}
	if err := node.Encode(plain(p)); err != nil {
		return nil, err
	}
	if !p.Options.IsZero() {
		opts := p.Options
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p.Type}, &opts)
	}
	return node, nil
}

// Targets is either an inline list or a discovery source.
type Targets struct {
	Static  []Target     `yaml:"static,omitempty"`
	File    *FileTargets `yaml:"file,omitempty"`
	HTTP    *HTTPTargets `yaml:"http,omitempty"`
	Refresh Duration     `yaml:"refresh,omitempty"`
}

// FileTargets reads targets from a JSON or YAML file on disk.
type FileTargets struct {
	Path string `yaml:"path"`
}

// HTTPTargets fetches a JSON list of targets from an endpoint.
type HTTPTargets struct {
	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Timeout Duration          `yaml:"timeout,omitempty"`
}

func (t *Targets) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		return n.Decode(&t.Static)
	}
	type plain Targets
	var v plain
	if err := n.Decode(&v); err != nil {
		return err
	}
	*t = Targets(v)
	if len(t.Static) == 0 && t.File == nil && t.HTTP == nil {
		return fmt.Errorf("line %d: targets needs a list, or a file/http source", n.Line)
	}
	return nil
}

// shape reports what a targets node is checked against, by how it is written.
func (Targets) shape(n *yaml.Node) reflect.Type {
	if n.Kind == yaml.SequenceNode {
		return reflect.TypeFor[[]Target]()
	}
	type plain Targets
	return reflect.TypeFor[plain]()
}

func (Target) shape(*yaml.Node) reflect.Type {
	type plain Target
	return reflect.TypeFor[plain]()
}

func (t Targets) Empty() bool { return len(t.Static) == 0 && t.File == nil && t.HTTP == nil }

// Target is written either as a shorthand string ("https://google.com/",
// "db.local:5432") or as a mapping.
type Target struct {
	Name   string            `yaml:"name,omitempty"`
	URL    string            `yaml:"url,omitempty"`
	Host   string            `yaml:"host,omitempty"`
	Port   int               `yaml:"port,omitempty"`
	Labels map[string]string `yaml:"labels,omitempty"`
}

func (t *Target) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return t.parseShorthand(n.Value, n.Line)
	}
	type plain Target
	var v plain
	if err := n.Decode(&v); err != nil {
		return err
	}
	*t = Target(v)
	if t.URL != "" && t.Host == "" {
		if err := t.parseShorthand(t.URL, n.Line); err != nil {
			return err
		}
	}
	if t.Host == "" && t.URL == "" {
		return fmt.Errorf("line %d: a target needs url or host", n.Line)
	}
	if t.Name == "" {
		t.Name = cmp.Or(t.URL, t.Host)
	}
	return nil
}

// Parse fills a target from the shorthand form, for callers outside YAML.
func (t *Target) Parse(s string) error { return t.parseShorthand(s, 0) }

// parseShorthand reads a string containing "://" as a URL and anything else as
// host[:port]; brackets keep an IPv6 address's colons off the port separator.
func (t *Target) parseShorthand(s string, line int) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("line %d: empty target", line)
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return fmt.Errorf("line %d: invalid URL %q: %w", line, s, err)
		}
		if u.Host == "" {
			return fmt.Errorf("line %d: URL %q has no host", line, s)
		}
		t.URL = s
		t.Host = u.Hostname()
		if p := u.Port(); p != "" {
			t.Port, _ = strconv.Atoi(p)
		}
		if t.Name == "" {
			t.Name = s
		}
		return nil
	}
	host, port, err := splitHostPort(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", line, err)
	}
	t.Host, t.Port = host, port
	if t.Name == "" {
		t.Name = s
	}
	return nil
}

func splitHostPort(s string) (string, int, error) {
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("unclosed bracket in IPv6 address %q", s)
		}
		host := s[1:end]
		rest := strings.TrimPrefix(s[end+1:], ":")
		if rest == "" {
			return host, 0, nil
		}
		port, err := strconv.Atoi(rest)
		if err != nil {
			return "", 0, fmt.Errorf("invalid port in %q", s)
		}
		return host, port, nil
	}
	if strings.Count(s, ":") == 1 {
		host, p, _ := strings.Cut(s, ":")
		port, err := strconv.Atoi(p)
		if err != nil {
			return "", 0, fmt.Errorf("invalid port in %q", s)
		}
		return host, port, nil
	}
	return s, 0, nil
}

var validIPVersion = map[string]bool{"ipv4": true, "ipv6": true, "both": true}

// Resolve expands inheritance and returns runnable probes.
func (p *Probes) Resolve() ([]Probe, error) {
	out := make([]Probe, 0, len(p.List))
	seen := make(map[string]int, len(p.List))

	for i, probe := range p.List {
		if probe.Name == "" {
			return nil, fmt.Errorf("probes[%d]: name is missing", i)
		}
		if prev, dup := seen[probe.Name]; dup {
			return nil, fmt.Errorf("probes[%d]: name %q already used by probes[%d]", i, probe.Name, prev)
		}
		seen[probe.Name] = i

		merged := p.Defaults
		if probe.Template != "" {
			tpl, ok := p.Templates[probe.Template]
			if !ok {
				return nil, fmt.Errorf("probes[%d] (%s): template %q not found", i, probe.Name, probe.Template)
			}
			merged = merged.mergeFrom(tpl)
		}
		merged = merged.mergeFrom(probe)
		merged.Name = probe.Name

		if merged.Type == "" {
			return nil, fmt.Errorf("probes[%d] (%s): probe type is missing", i, probe.Name)
		}
		if merged.Targets.Empty() {
			return nil, fmt.Errorf("probes[%d] (%s): targets are missing", i, probe.Name)
		}
		if err := merged.validate(); err != nil {
			return nil, fmt.Errorf("probes[%d] (%s): %w", i, probe.Name, err)
		}
		out = append(out, merged)
	}
	return out, nil
}

// validate checks what a probe says about itself, whoever supplied it.
func (p Probe) validate() error {
	if p.IPVersion != "" && !validIPVersion[p.IPVersion] {
		return fmt.Errorf("ip_version: want ipv4|ipv6|both, got %q", p.IPVersion)
	}
	if p.SourceIP != "" {
		if _, err := netip.ParseAddr(p.SourceIP); err != nil {
			return fmt.Errorf("source_ip %q is not an address", p.SourceIP)
		}
	}
	// Compiled and thrown away: the runner compiles a copy of its own.
	if _, err := p.Schedule.Compile(); err != nil {
		return err
	}
	if p.RunOn != "" {
		if _, err := regexp.Compile(p.RunOn); err != nil {
			return fmt.Errorf("run_on: %w", err)
		}
	}
	if p.RequestsPerProbe < 0 || p.RequestsPerProbe > MaxRequestsPerProbe {
		return fmt.Errorf("requests_per_probe: want 0-%d, got %d", MaxRequestsPerProbe, p.RequestsPerProbe)
	}
	// A negative duration is not "unset": it wins over the default.
	if p.Interval < 0 || p.Timeout < 0 {
		return fmt.Errorf("interval and timeout cannot be negative")
	}
	if p.Targets.Refresh < 0 {
		return fmt.Errorf("targets.refresh cannot be negative")
	}
	if p.Targets.HTTP != nil && p.Targets.HTTP.Timeout < 0 {
		return fmt.Errorf("targets.http.timeout cannot be negative")
	}
	if err := metrics.Buckets(p.LatencyBuckets).Valid(); err != nil {
		return fmt.Errorf("latency_buckets: %w", err)
	}
	// What is left unset comes from the node; the runner checks again with both.
	if p.Interval > 0 && p.Timeout > 0 {
		return CheckTiming(p.Interval.D(), p.Timeout.D(), p.RequestsPerProbe)
	}
	return nil
}

// CheckTiming validates the interval and timeout a probe ends up with, once the
// node's defaults have filled what it left out.
func CheckTiming(interval, timeout time.Duration, requests int) error {
	if timeout > interval {
		return fmt.Errorf("timeout (%s) exceeds interval (%s)", timeout, interval)
	}
	// Each repeat has the whole timeout to itself.
	if worst := time.Duration(max(1, requests)) * timeout; worst > interval {
		return fmt.Errorf("requests_per_probe (%d) x timeout (%s) exceeds interval (%s)", requests, timeout, interval)
	}
	return nil
}

// mergeFrom overlays other on p: a set scalar or flag wins, a non-empty target
// list or options block replaces, and labels are unioned with other winning.
func (p Probe) mergeFrom(other Probe) Probe {
	res := p
	res.Type = cmp.Or(other.Type, p.Type)
	res.IPVersion = cmp.Or(other.IPVersion, p.IPVersion)
	res.IPFallback = pick(other.IPFallback, p.IPFallback)
	res.SourceIP = cmp.Or(other.SourceIP, p.SourceIP)
	res.NegativeTest = pick(other.NegativeTest, p.NegativeTest)
	res.RequestsPerProbe = cmp.Or(other.RequestsPerProbe, p.RequestsPerProbe)
	res.RunOn = cmp.Or(other.RunOn, p.RunOn)
	if other.Schedule != nil {
		res.Schedule = other.Schedule
	}
	res.Template = other.Template
	res.Disabled = pick(other.Disabled, p.Disabled)
	res.Interval = cmp.Or(other.Interval, p.Interval)
	res.Timeout = cmp.Or(other.Timeout, p.Timeout)
	res.PerBackend = pick(other.PerBackend, p.PerBackend)
	if !other.Targets.Empty() {
		res.Targets = other.Targets
	}
	if len(other.LatencyBuckets) > 0 {
		res.LatencyBuckets = other.LatencyBuckets
	}
	res.PhaseHistograms = pick(other.PhaseHistograms, p.PhaseHistograms)
	if !other.Options.IsZero() {
		res.Options = other.Options
	}
	res.Labels = mergeLabels(p.Labels, other.Labels)
	return res
}

// pick returns the override when it is set.
func pick(override, base *bool) *bool {
	if override != nil {
		return override
	}
	return base
}

// Enabled reports whether a flag is on, treating "unset" as the given default.
func Enabled(flag *bool, def bool) bool {
	if flag == nil {
		return def
	}
	return *flag
}

func mergeLabels(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// DecodeOptions parses the prober's parameter block; unknown fields are an error.
func (p Probe) DecodeOptions(dst any) error {
	if p.Options.IsZero() {
		return nil
	}
	dec := p.Options
	return decodeStrict(&dec, dst)
}
