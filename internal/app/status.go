package app

import (
	"cmp"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// Where a check configuration came from; "none" is a node nobody has pushed to.
const (
	SourceFile = "file"
	SourceAPI  = "api"
	SourceNone = "none"
)

// ConfigMeta describes the running check configuration: where it came from and
// which bytes it was. The hash lets the pusher tell whether the node is already
// running what it would send.
type ConfigMeta struct {
	Source    string
	Hash      string
	AppliedAt time.Time
}

// Status is the summary served at /status: what this node currently checks.
type Status struct {
	Node    string `json:"node"`
	Version string `json:"version"`
	Uptime  string `json:"uptime"`
	Series  int    `json:"series"`
	// ConfigSource is file, api or none; ConfigHash is the sha256 of the bytes
	// behind it, and both are empty only before anything was applied.
	ConfigSource    string        `json:"config_source"`
	ConfigHash      string        `json:"config_hash,omitempty"`
	ConfigAppliedAt string        `json:"config_applied_at,omitempty"`
	Resolver        string        `json:"resolver"`
	Probes          []ProbeStatus `json:"probes"`
}

// ProbeStatus is one probe after defaults, templates and discovery.
type ProbeStatus struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Interval   string `json:"interval"`
	Timeout    string `json:"timeout"`
	PerBackend bool   `json:"per_backend"`
	Sleeping   bool   `json:"sleeping,omitempty"`
	// Negative marks a probe whose success is a refusal.
	Negative bool `json:"negative,omitempty"`
	// Failures maps "target|backend" to the message behind a current failure.
	Failures map[string]string `json:"failures,omitempty"`
	Targets  []string          `json:"targets"`
}

// Status takes a snapshot of what the node is doing right now.
func (a *App) Status() Status {
	runners := a.Runners()
	meta := a.ConfigMeta()
	s := Status{
		Node:         a.Cfg.Node.Name,
		Version:      VersionString(),
		Uptime:       time.Since(started).Round(time.Second).String(),
		Series:       a.Store.Len(),
		ConfigSource: cmp.Or(meta.Source, SourceNone),
		ConfigHash:   meta.Hash,
		Resolver:     a.Cfg.Resolver.Mode,
		Probes:       make([]ProbeStatus, 0, len(runners)),
	}
	if !meta.AppliedAt.IsZero() {
		s.ConfigAppliedAt = meta.AppliedAt.UTC().Format(time.RFC3339)
	}
	for _, r := range runners {
		targets := r.Source.Targets()
		names := make([]string, 0, len(targets))
		for _, t := range targets {
			names = append(names, t.Name)
		}
		s.Probes = append(s.Probes, ProbeStatus{
			Name:       r.Name,
			Type:       r.Kind,
			Interval:   r.Interval.String(),
			Timeout:    r.Timeout.String(),
			PerBackend: r.PerBackend,
			Sleeping:   r.Sleeping(),
			Negative:   r.Negative,
			Failures:   messages(r.Failures()),
			Targets:    names,
		})
	}
	return s
}

func messages(failures map[string]probe.Failure) map[string]string {
	if len(failures) == 0 {
		return nil
	}
	out := make(map[string]string, len(failures))
	for k, f := range failures {
		out[k] = f.Message
	}
	return out
}
