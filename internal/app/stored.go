package app

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// Stored is what the probe's own runs last measured, in the shape Samples
// gives an on-demand run: nothing is run and nothing is sent. It is how a page
// watches a probe whose target must not be asked more often than the
// schedule says — one on a cron, one with a quota — where an on-demand run
// every few seconds would be the flood the schedule is there to prevent.
//
// The log carries a line per current failure, worded as an on-demand run's
// trace words it, so a reader of /probe reads both alike, and the time of the
// last run.
func (a *App) Stored(name string) (Exposition, error) {
	r := a.find(name)
	if r == nil {
		return Exposition{}, fmt.Errorf("%w: probe %q", ErrNotFound, name)
	}
	runner := r.runner
	up := map[string][]bool{} // target -> its addresses' verdicts
	out := a.Store.Select(func(s metrics.Sample) bool {
		// The node's own counters carry a probe label too, but no target: they
		// are about the agent, not what the probe measured. Counters and
		// histograms run over the node's lifetime; the page wants the last run,
		// as /probe gives it.
		if s.Labels.Get("probe") != name || s.Labels.Get("target") == "" {
			return false
		}
		k := s.Value.Kind()
		return k != metrics.KindCounter && k != metrics.KindDist
	})
	for _, s := range out {
		if s.Name == probe.SeriesUp {
			t := s.Labels.Get("target")
			up[t] = append(up[t], s.Value == metrics.Gauge(1))
		}
	}
	// probe_success, one per target, as /probe gives it.
	for _, t := range runner.Source.Targets() {
		verdicts, ok := up[t.Name]
		if !ok {
			continue
		}
		success := len(verdicts) > 0 && !slices.Contains(verdicts, false)
		out = append(out, metrics.Sample{Name: "probe_success", Labels: runner.TargetLabels(t),
			Value: metrics.Gauge(metrics.Bool(success))})
	}

	// The failures of the run that wrote these samples - not of one still going,
	// whose backends report as they finish.
	var (
		log      []string
		failures map[string]probe.Failure
		at       = time.Now()
	)
	if last := runner.LastPublished(); last != nil {
		at, failures = last.At, last.Failures
		log = append(log, "INFO stored last_run="+at.UTC().Format(time.RFC3339))
	}
	keys := slices.Sorted(maps.Keys(failures))
	for _, k := range keys {
		f := failures[k]
		// "target|backend": a target name may hold a bar, an address never does.
		cut := strings.LastIndex(k, "|")
		target, backend := k[:cut], k[cut+1:]
		if backend == "resolve" {
			backend = "" // the target as a whole, as a trace names it
		}
		log = append(log, format(map[string]any{
			"level": "WARN", "msg": "backend is down",
			"target": target, "backend": backend, "reason": string(f.Reason), "err": f.Message,
		}))
	}
	exp := Exposition{Samples: out, Log: log}
	if a.Cfg.HTTP.API.ProbeAliases {
		// As an on-demand run gives them: a scrape moved to stored=true keeps its series.
		exp.Aliases = blackboxAliases(out, at, func(target string) bool {
			for k, f := range failures {
				if f.Regex && k[:strings.LastIndex(k, "|")] == target {
					return true
				}
			}
			return false
		})
	}
	return exp, nil
}
