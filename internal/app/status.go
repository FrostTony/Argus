package app

import (
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// Status is the summary served at /status: what this node currently checks.
type Status struct {
	Node     string        `json:"node"`
	Uptime   string        `json:"uptime"`
	Series   int           `json:"series"`
	Resolver string        `json:"resolver"`
	Probes   []ProbeStatus `json:"probes"`
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
	s := Status{
		Node:     a.Cfg.Node.Name,
		Uptime:   time.Since(started).Round(time.Second).String(),
		Series:   a.Store.Len(),
		Resolver: a.Cfg.Resolver.Mode,
		Probes:   make([]ProbeStatus, 0, len(runners)),
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
