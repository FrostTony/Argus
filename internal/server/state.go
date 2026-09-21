package server

import (
	"slices"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// stateView is the status page's model: everything the node knows right now.
type stateView struct {
	Node       string      `json:"node"`
	Version    string      `json:"version"`
	Uptime     string      `json:"uptime"`
	StaleAfter string      `json:"stale_after"`
	Time       int64       `json:"time"`
	Up         int         `json:"up"`
	Down       int         `json:"down"`
	Series     int         `json:"series"`
	Probes     []probeData `json:"probes"`
}

type probeData struct {
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	Interval string       `json:"interval"`
	Sleeping bool         `json:"sleeping,omitempty"`
	Negative bool         `json:"negative,omitempty"`
	Targets  []targetData `json:"targets"`
}

type targetData struct {
	probe string
	// declared is target_backends; zero with counted set means resolution failed.
	declared int
	counted  bool

	Target  string  `json:"target"`
	Up      int     `json:"up"`
	Total   int     `json:"total"`
	Resolve float64 `json:"resolve,omitempty"`
	// Error is set when the target never got as far as a backend.
	Error    string             `json:"error,omitempty"`
	Stats    map[string]float64 `json:"stats,omitempty"`
	Backends []backendData      `json:"backends"`
}

// backendData carries every metric the probe recorded for one address.
type backendData struct {
	// live is set once probe_up was seen; stale totals alone must not make a row.
	live bool

	Backend string `json:"backend,omitempty"`
	Family  string `json:"family,omitempty"`
	Up      bool   `json:"up"`
	Reason  string `json:"reason,omitempty"`
	// Error is the message behind the failure; Reason is only its category.
	Error   string  `json:"error,omitempty"`
	Latency float64 `json:"latency,omitempty"`
	Age     float64 `json:"age,omitempty"`

	Phases   map[string]float64  `json:"phases,omitempty"`
	Gauges   map[string]float64  `json:"gauges,omitempty"`
	Counters map[string]float64  `json:"counters,omitempty"`
	Info     map[string]string   `json:"info,omitempty"`
	Hist     map[string]histData `json:"hist,omitempty"`
}

// histData is a histogram summarised without its buckets; a bucketless one
// has no percentiles to give.
type histData struct {
	Count uint64  `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50,omitempty"`
	P95   float64 `json:"p95,omitempty"`
}

// Series the page reads by name, each driving a field of its own.
var (
	mUp      = probe.SeriesUp
	mLast    = probe.TimeProbe.Last()
	mPhase   = probe.TimePhase.Last()
	mResolve = probe.TimeResolve.Last()
)

func (s *statusPage) state() stateView {
	a := s.app
	a.RefreshSelf()
	samples := a.Store.Snapshot()
	status := a.Status()

	view := stateView{
		Node:       orDash(status.Node),
		Version:    app.Version,
		Uptime:     status.Uptime,
		StaleAfter: a.Cfg.Probing.StaleAfter.String(),
		Time:       time.Now().UnixMilli(),
		Series:     status.Series,
		Probes:     []probeData{},
	}

	// Grouped by metric name, so no pointer here may alias a growing slice.
	type key struct{ probe, target, backend string }
	backends := map[key]*backendData{}
	targets := map[string]*targetData{}
	var backendOrder []key
	var targetOrder []string

	for _, sm := range samples {
		name := sm.Labels.Get("probe")
		target := sm.Labels.Get("target")
		// The agent's own metrics carry a probe label but no target.
		if name == "" || target == "" {
			continue
		}
		tk := name + "\x00" + target
		td, ok := targets[tk]
		if !ok {
			td = &targetData{Target: orDash(target), probe: name, Backends: []backendData{}}
			targets[tk] = td
			targetOrder = append(targetOrder, tk)
		}
		if probe.TargetLevel(sm.Name) {
			absorbTarget(td, sm)
			continue
		}
		k := key{name, target, sm.Labels.Get("backend")}
		bd, ok := backends[k]
		if !ok {
			bd = &backendData{Backend: k.backend, Family: sm.Labels.Get("family")}
			backends[k] = bd
			backendOrder = append(backendOrder, k)
		}
		absorb(bd, sm)
	}

	failures := map[string]map[string]probe.Failure{}
	for _, r := range a.Runners() {
		failures[r.Name] = r.Failures()
	}

	for _, k := range backendOrder {
		td := targets[k.probe+"\x00"+k.target]
		// A probe result with no address belongs to the target, not to a backend.
		if k.backend == "" && td.counted && td.declared == 0 {
			continue
		}
		bd := backends[k]
		if !bd.live {
			continue
		}
		if !bd.Up {
			// The runner, not the counters, knows which failure was the last one.
			why := failures[k.probe][k.target+"|"+k.backend]
			bd.Reason, bd.Error = string(why.Reason), why.Message
		}
		td.Backends = append(td.Backends, *bd)
	}

	byProbe := make(map[string]*probeData, len(status.Probes))
	for _, p := range status.Probes {
		// An empty list marshals as [], never null.
		byProbe[p.Name] = &probeData{
			Name: p.Name, Type: p.Type, Interval: p.Interval,
			Sleeping: p.Sleeping, Negative: p.Negative,
			Targets: []targetData{},
		}
	}

	for _, tk := range targetOrder {
		td := targets[tk]
		pd, ok := byProbe[td.probe]
		if !ok {
			continue // series of a removed probe
		}
		slices.SortFunc(td.Backends, func(a, b backendData) int {
			return strings.Compare(a.Backend, b.Backend)
		})
		td.Total = len(td.Backends)
		for _, b := range td.Backends {
			if b.Up {
				td.Up++
				view.Up++
			} else {
				view.Down++
			}
		}
		// A target with no backends did not resolve, which counts as down.
		if len(td.Backends) == 0 {
			if !td.counted {
				continue // stale totals of a gone target
			}
			td.Error = failures[td.probe][td.Target+"|resolve"].Message
			view.Down++
		}
		pd.Targets = append(pd.Targets, *td)
	}

	for _, p := range status.Probes {
		pd := byProbe[p.Name]
		slices.SortStableFunc(pd.Targets, func(a, b targetData) int {
			if ab, bb := a.Up < a.Total, b.Up < b.Total; ab != bb {
				if ab {
					return -1
				}
				return 1
			}
			return strings.Compare(a.Target, b.Target)
		})
		view.Probes = append(view.Probes, *pd)
	}
	return view
}

func absorbTarget(td *targetData, sm metrics.Sample) {
	switch v := sm.Value.(type) {
	case metrics.Gauge:
		switch sm.Name {
		case probe.SeriesBackends:
			td.declared, td.counted = int(v), true
			return
		case mResolve:
			td.Resolve = float64(v)
			return
		}
		set(&td.Stats, sm.Name, float64(v))
	case metrics.Counter:
		set(&td.Stats, sm.Name, float64(v))
	case *metrics.Dist:
		if sm.Name == probe.TimeResolve.Hist() {
			set(&td.Stats, "resolve_mean", v.Mean())
			set(&td.Stats, "resolve_p95", v.Quantile(0.95))
		}
	}
}

// absorb files one sample under the heading the page reads it from.
func absorb(bd *backendData, sm metrics.Sample) {
	switch v := sm.Value.(type) {
	case metrics.Gauge:
		switch sm.Name {
		case mUp:
			bd.Up, bd.live = v == 1, true
		case mLast:
			bd.Latency = float64(v)
		case mPhase:
			set(&bd.Phases, sm.Labels.Get("phase"), float64(v))
		case "tls_cert_expiry_days":
			bd.Age = float64(v)
			set(&bd.Gauges, sm.Name, float64(v))
		default:
			set(&bd.Gauges, trimLast(sm.Name), float64(v))
		}
	case metrics.Counter:
		key := sm.Name
		if r := sm.Labels.Get("reason"); r != "" {
			key += " (" + r + ")"
		}
		set(&bd.Counters, key, float64(v))
	case metrics.Info:
		set(&bd.Info, sm.Name, string(v))
	case *metrics.Dist:
		if bd.Hist == nil {
			bd.Hist = map[string]histData{}
		}
		name := strings.TrimSuffix(sm.Name, "_duration_seconds")
		if p := sm.Labels.Get("phase"); p != "" {
			name += ":" + p
		}
		if r := sm.Labels.Get("resolver"); r != "" {
			name += ":" + r
		}
		h := histData{Count: v.Count, Mean: v.Mean()}
		if v.HasBuckets() {
			h.P50, h.P95 = v.Quantile(0.5), v.Quantile(0.95)
		}
		bd.Hist[name] = h
	}
}

// trimLast drops the _last_duration_seconds suffix from a metric name.
func trimLast(name string) string {
	return strings.TrimSuffix(name, "_last_duration_seconds")
}

func set[T any](m *map[string]T, k string, v T) {
	if k == "" {
		return
	}
	if *m == nil {
		*m = map[string]T{}
	}
	(*m)[k] = v
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
