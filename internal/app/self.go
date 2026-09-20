package app

import (
	"context"
	"runtime"
	"runtime/debug"
	rtmetrics "runtime/metrics"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/discovery"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// Version is the release this build is of; -ldflags stamps a tag over it.
var Version = "0.0.2"

// Revision is the commit this binary was built from.
var Revision = revision()

func revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key != "vcs.revision" {
			continue
		}
		// Short: this ends up in a metric label and a log field.
		if len(s.Value) > 12 {
			return s.Value[:12]
		}
		return s.Value
	}
	return ""
}

// VersionString is the release, plus the commit when there is one.
func VersionString() string {
	if Revision == "" {
		return Version
	}
	return Version + "+" + Revision
}

// selfRecorder puts every series it records under metrics.SelfPrefix. It is
// not the Recorder embedded, so that a self metric cannot be written
// unprefixed by reaching past it.
type selfRecorder struct{ rec *metrics.Recorder }

func (s selfRecorder) Gauge(name string, v float64, extra ...string) {
	s.rec.Gauge(metrics.SelfPrefix+name, v, extra...)
}

func (s selfRecorder) Info(name, text string, extra ...string) {
	s.rec.Info(metrics.SelfPrefix+name, text, extra...)
}

var started = time.Now()

const selfInterval = 15 * time.Second

// reportSelf publishes the agent's own state on a ticker.
func (a *App) reportSelf(ctx context.Context) {
	t := time.NewTicker(selfInterval)
	defer t.Stop()
	a.writeSelf(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.writeSelf(ctx)
		}
	}
}

func (a *App) writeSelf(ctx context.Context) {
	a.Sink.Write(ctx, a.selfBatch())
}

// RefreshSelf updates the agent's own metrics in the store, once per scrape.
func (a *App) RefreshSelf() {
	a.Store.Write(context.Background(), a.selfBatch())
}

func (a *App) selfBatch() metrics.Batch {
	rec := metrics.NewRecorder(a.nodeLabels())
	a.collectSelf(rec)
	b := rec.Batch(time.Now())
	b.Every = selfInterval
	return b
}

func (a *App) collectSelf(r *metrics.Recorder) {
	rec := selfRecorder{r}
	a.mu.Lock()
	probes := len(a.probes)
	a.mu.Unlock()

	if Revision != "" {
		rec.Info("build_info", Version, "go_version", runtime.Version(), "revision", Revision)
	} else {
		rec.Info("build_info", Version, "go_version", runtime.Version())
	}
	rec.Gauge("uptime_seconds", time.Since(started).Seconds())
	rec.Gauge("goroutines", float64(runtime.NumGoroutine()))
	rec.Gauge("memory_bytes", float64(heapBytes()))
	rec.Gauge("probes_configured", float64(probes))
	rec.Gauge("series", float64(a.Store.Len()))
	rec.Gauge("series_dropped", float64(a.Store.Dropped()))
	rec.Gauge("config_reloads", float64(a.reloads.Load()))
	rec.Gauge("config_reload_failures", float64(a.reloadNG.Load()))
	// Pinned at the limit means probes queue and their timings include the wait.
	rec.Gauge("probe_slots_used", float64(len(a.sem)))
	rec.Gauge("probe_slots_total", float64(cap(a.sem)))

	for _, r := range a.Runners() {
		rec.Gauge("probe_targets", float64(len(r.Source.Targets())), "probe", r.Name)
		// A source that stopped answering keeps serving its last good list.
		if d, ok := r.Source.(*discovery.Dynamic); ok {
			rec.Gauge("discovery_failures", float64(d.Failures()), "probe", r.Name, "source", d.Describe())
			rec.Gauge("discovery_age_seconds", d.Age().Seconds(), "probe", r.Name, "source", d.Describe())
		}
		rec.Gauge("probe_runs_skipped", float64(r.Skipped()), "probe", r.Name)
		rec.Gauge("probe_panics", float64(r.Panics()), "probe", r.Name)
		rec.Gauge("probe_backends_down", float64(r.Down()), "probe", r.Name)
		rec.Gauge("probe_runs_paused", float64(r.Paused()), "probe", r.Name)
		rec.Gauge("probe_sleeping", metrics.Bool(r.Sleeping()), "probe", r.Name)
	}
}

// heapBytes reads the live heap without stopping the world, as ReadMemStats
// would.
func heapBytes() uint64 {
	sample := []rtmetrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	rtmetrics.Read(sample)
	if sample[0].Value.Kind() != rtmetrics.KindUint64 {
		return 0
	}
	return sample[0].Value.Uint64()
}
