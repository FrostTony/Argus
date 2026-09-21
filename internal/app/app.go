// Package app wires configuration into running probes.
package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/discovery"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
	"github.com/tonyamdfrost-cmd/Argus/internal/surfacer"
)

// App owns what outlives a configuration: the store, the sinks, the resolver.
type App struct {
	Cfg   config.Server
	Store *metrics.Store
	Log   *slog.Logger
	Sink  metrics.Sink

	resolver  resolve.Resolver
	sem       chan struct{}
	buckets   metrics.Buckets
	closers   []func() error
	reopeners []func() error

	// applyMu serialises reloads end to end.
	applyMu sync.Mutex

	mu     sync.Mutex
	probes map[string]*running
	order  []string
	source config.Probes
	// meta is where the running configuration came from; the API and the
	// loader set it, /status reports it.
	meta    ConfigMeta
	rootCtx context.Context
	live    sync.WaitGroup
	// stopped is the shutdown barrier: nothing new may register with live.
	stopped bool
	alive   atomic.Int64

	reloads  atomic.Int64
	reloadNG atomic.Int64
}

type running struct {
	runner *probe.Runner
	// fingerprint and files decide whether a reload leaves the probe alone.
	fingerprint string
	files       string
	cancel      context.CancelFunc
	done        chan struct{}
}

// Build assembles a node from its two configurations; nothing runs until Start.
func Build(server config.Server, probes config.Probes, log *slog.Logger) (*App, error) {
	a := &App{
		Cfg:     server,
		Log:     log,
		Store:   metrics.NewStore(server.Probing.StaleAfter.D(), server.Probing.MaxSeries),
		sem:     make(chan struct{}, max(1, server.Probing.MaxConcurrent)),
		buckets: metrics.Buckets(server.Probing.LatencyBuckets),
		probes:  map[string]*running{},
	}
	if len(a.buckets) == 0 {
		a.buckets = metrics.DefaultLatencyBuckets
	}
	if err := a.buildSinks(server); err != nil {
		return nil, err
	}
	a.resolver = buildResolver(server.Resolver)

	if err := a.apply(probes); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) buildSinks(server config.Server) error {
	sinks := metrics.Fanout{a.Store}
	for _, sf := range server.Surfacers {
		switch sf.Type {
		case "prometheus":
			// Reads the store directly; no sink of its own.
		case "file":
			f, err := surfacer.NewFile(sf.File.Path)
			if err != nil {
				return err
			}
			sinks = append(sinks, f)
			a.closers = append(a.closers, f.Close)
			a.reopeners = append(a.reopeners, f.Reopen)
		case "remote_write":
			rw := surfacer.NewRemoteWrite(surfacer.RemoteWriteOptions{
				URL:          sf.RemoteWrite.URL,
				Interval:     sf.RemoteWrite.Interval.D(),
				Timeout:      sf.RemoteWrite.Timeout.D(),
				Headers:      sf.RemoteWrite.Headers,
				User:         sf.RemoteWrite.BasicAuthUser,
				Password:     sf.RemoteWrite.BasicAuthPass,
				MaxBatchSize: sf.RemoteWrite.MaxBatchSize,
				MaxRetries:   sf.RemoteWrite.MaxRetries,
				Prefix:       server.Exposition().Prefix,
				Store:        a.Store,
				Log:          a.Log,
			})
			sinks = append(sinks, rw)
			a.closers = append(a.closers, rw.Close)
		}
	}
	a.Sink = sinks
	return nil
}

func buildResolver(c config.Resolver) resolve.Resolver {
	var inner resolve.Resolver
	if c.Mode == "custom" {
		inner = resolve.NewDNS(c.Servers, c.Timeout.D(), c.StableOrder)
	} else {
		inner = &resolve.System{Timeout: c.Timeout.D(), Order: c.StableOrder}
	}
	return resolve.NewCache(inner, c.CacheTTL.D(), c.MinTTL.D(), c.MaxTTL.D())
}

// Reload swaps in a new check configuration, touching only what changed.
func (a *App) Reload(probes config.Probes) error {
	err := a.apply(probes)
	if err != nil {
		a.reloadNG.Add(1)
		return err
	}
	a.reloads.Add(1)
	return nil
}

// apply installs a probe set, failing before any running probe is touched.
func (a *App) apply(probes config.Probes) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	list, err := probes.Resolve()
	if err != nil {
		return err
	}

	a.mu.Lock()
	current := make(map[string]*running, len(a.probes))
	for k, v := range a.probes {
		current[k] = v
	}
	nodeLabels := a.nodeLabels()
	a.mu.Unlock()

	next := make(map[string]*running, len(list))
	order := make([]string, 0, len(list))

	for _, pc := range list {
		if config.Enabled(pc.Disabled, false) {
			continue
		}
		if !a.runsHere(pc) {
			continue
		}
		fp, err := fingerprint(pc)
		if err != nil {
			return err
		}
		// An identical configuration keeps the runner, and with it its ticker.
		if old, ok := current[pc.Name]; ok && old.fingerprint == fp && old.files == digest(old.runner.Prober) {
			next[pc.Name] = old
			order = append(order, pc.Name)
			continue
		}
		r, err := a.buildRunner(pc, nodeLabels)
		if err != nil {
			return err
		}
		if old, ok := current[pc.Name]; ok {
			r.Inherit(old.runner)
		}
		next[pc.Name] = &running{runner: r, fingerprint: fp, files: digest(r.Prober)}
		order = append(order, pc.Name)
	}
	if len(next) == 0 {
		// Legal: a node that has just been installed, or one whose last domain
		// was taken away, has nothing to check. Loud, because silence here
		// looks exactly like health.
		a.Log.Warn("no probe runs on this node: the configuration is empty, disabled or meant for other nodes")
	}

	a.mu.Lock()
	a.probes, a.order, a.source = next, order, probes
	var starting, stopping []*running
	for _, name := range order {
		if r := next[name]; r.done == nil {
			starting = append(starting, r)
		}
	}
	var gone []string
	for name, old := range current {
		if next[name] != old {
			stopping = append(stopping, old)
			if _, kept := next[name]; !kept {
				gone = append(gone, name)
			}
		}
	}
	if a.rootCtx != nil {
		for _, r := range starting {
			a.startLocked(r)
		}
	}
	a.mu.Unlock()

	// Replaced probes stop only after their successors are up.
	for _, r := range stopping {
		if r.cancel != nil {
			r.cancel()
		}
	}
	// A probe that left the configuration leaves /metrics with it.
	for _, name := range gone {
		a.Store.Drop(func(s metrics.Sample) bool { return s.Labels.Get("probe") == name })
	}
	if len(starting) > 0 || len(stopping) > 0 {
		a.Log.Info("configuration applied",
			"probes", len(next), "started", len(starting), "stopped", len(stopping), "removed", len(gone))
	}
	return nil
}

func fingerprint(pc config.Probe) (string, error) {
	data, err := yaml.Marshal(pc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// digest is the state of the files a prober read, so a rotation rebuilds it.
func digest(p probe.Prober) string {
	fb, ok := p.(probe.FileBacked)
	if !ok {
		return ""
	}
	sum := sha256.New()
	for _, path := range fb.Files() {
		data, err := os.ReadFile(path)
		fmt.Fprintf(sum, "%s\x00%d\x00%v\x00", path, len(data), err)
		sum.Write(data)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func (a *App) buildRunner(pc config.Probe, nodeLabels metrics.Labels) (*probe.Runner, error) {
	prober, err := probe.New(pc)
	if err != nil {
		return nil, err
	}
	source, err := discovery.Build(pc.Name, pc.Targets, a.Log)
	if err != nil {
		return nil, fmt.Errorf("probe %q: %w", pc.Name, err)
	}
	buckets := a.buckets
	if len(pc.LatencyBuckets) > 0 {
		buckets = metrics.Buckets(pc.LatencyBuckets)
	}
	var phaseBuckets metrics.Buckets
	if config.Enabled(pc.PhaseHistograms, a.Cfg.Probing.PhaseHistograms) {
		phaseBuckets = buckets
	}
	labels := nodeLabels.Merge(labelsOf(pc.Labels)).With("probe_type", pc.Type)
	if err := metrics.ValidateLabels(labels.With("probe", pc.Name)); err != nil {
		return nil, fmt.Errorf("probe %q: %w", pc.Name, err)
	}
	// Compiled per runner: the parsed configuration stays pure data.
	schedule, err := pc.Schedule.Compile()
	if err != nil {
		return nil, fmt.Errorf("probe %q: %w", pc.Name, err)
	}
	family := resolve.Family(cmp.Or(pc.IPVersion, a.Cfg.Resolver.IPFamily))
	var sourceIP netip.Addr
	if pc.SourceIP != "" {
		if sourceIP, err = netip.ParseAddr(pc.SourceIP); err != nil {
			return nil, fmt.Errorf("probe %q: source_ip: %w", pc.Name, err)
		}
	}
	// Checked on the values the probe runs with: either may be inherited.
	interval := cmp.Or(pc.Interval.D(), a.Cfg.Probing.Interval.D())
	timeout := cmp.Or(pc.Timeout.D(), a.Cfg.Probing.Timeout.D())
	if err := config.CheckTiming(interval, timeout, pc.RequestsPerProbe); err != nil {
		return nil, fmt.Errorf("probe %q: %w", pc.Name, err)
	}

	return &probe.Runner{
		Name:         pc.Name,
		Kind:         pc.Type,
		Prober:       prober,
		Source:       source,
		Interval:     interval,
		Timeout:      timeout,
		Labels:       labels,
		Buckets:      buckets,
		PhaseBuckets: phaseBuckets,
		Resolver:     a.resolver,
		Family:       family,
		Fallback:     config.Enabled(pc.IPFallback, false),
		SourceIP:     sourceIP,
		Schedule:     schedule,
		Negative:     config.Enabled(pc.NegativeTest, false),
		Requests:     pc.RequestsPerProbe,
		Hostname:     pc.Hostname,
		MaxBackends:  a.Cfg.Resolver.MaxBackends,
		PerBackend:   config.Enabled(pc.PerBackend, a.Cfg.Probing.PerBackend),
		Sem:          a.sem,
		Log:          a.Log,
	}, nil
}

// runsHere matches run_on against this node's name; an unnamed node runs all.
func (a *App) runsHere(pc config.Probe) bool {
	if pc.RunOn == "" || a.Cfg.Node.Name == "" {
		return true
	}
	re, err := regexp.Compile(pc.RunOn)
	if err != nil {
		return true // a broken pattern must not hide a probe
	}
	return re.MatchString(a.Cfg.Node.Name)
}

func (a *App) nodeLabels() metrics.Labels {
	l := metrics.Labels{}
	if a.Cfg.Node.Name != "" {
		l = l.With("node", a.Cfg.Node.Name)
	}
	for k, v := range a.Cfg.Node.Labels {
		l = l.With("node_"+k, v)
	}
	return l
}

func labelsOf(m map[string]string) metrics.Labels {
	l := metrics.Labels{}
	for k, v := range m {
		l = l.With(k, v)
	}
	return l
}

// Start runs every probe and blocks until the context is cancelled.
func (a *App) Start(ctx context.Context) {
	a.mu.Lock()
	a.rootCtx = ctx
	for _, name := range a.order {
		a.startLocked(a.probes[name])
	}
	a.mu.Unlock()

	a.live.Add(2)
	go func() { defer a.live.Done(); a.sweep(ctx) }()
	go func() { defer a.live.Done(); a.reportSelf(ctx) }()
	<-ctx.Done()

	a.mu.Lock()
	a.stopped = true
	a.mu.Unlock()

	a.wait()
}

func (a *App) startLocked(r *running) {
	if a.stopped {
		return
	}
	ctx, cancel := context.WithCancel(a.rootCtx)
	r.cancel = cancel
	r.done = make(chan struct{})
	a.live.Add(1)
	a.alive.Add(1)

	go func() {
		defer a.live.Done()
		defer a.alive.Add(-1)
		defer close(r.done)
		var wg sync.WaitGroup
		if d, ok := r.runner.Source.(*discovery.Dynamic); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.Start(ctx)
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runner.Start(ctx, a.Sink, a.Cfg.Probing.Jitter)
		}()
		wg.Wait()
	}()
}

// wait blocks until every goroutine that writes metrics has stopped.
func (a *App) wait() { a.live.Wait() }

// Running reports how many probes are still going, draining ones included.
func (a *App) Running() int64 { return a.alive.Load() }

// Runners is the probes this node currently runs, in configuration order.
func (a *App) Runners() []*probe.Runner {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*probe.Runner, 0, len(a.order))
	for _, name := range a.order {
		out = append(out, a.probes[name].runner)
	}
	return out
}

// Probes is the check configuration as loaded, before inheritance is applied.
func (a *App) Probes() config.Probes {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.source
}

// SetConfigMeta records where the configuration now running came from. It is
// set after a successful apply, so a rejected push leaves the old value alone.
func (a *App) SetConfigMeta(source, hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.meta = ConfigMeta{Source: source, Hash: hash, AppliedAt: time.Now()}
}

// ConfigMeta is what the node says about its current configuration.
func (a *App) ConfigMeta() ConfigMeta {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meta
}

// RunOnce runs one probe or all of them once, under the overlap guard.
func (a *App) RunOnce(ctx context.Context, name string) error {
	found, busy := false, 0
	for _, r := range a.Runners() {
		if name != "" && r.Name != name {
			continue
		}
		found = true
		if !r.RunNow(ctx, a.Sink) {
			busy++
		}
	}
	switch {
	case !found:
		return fmt.Errorf("probe %q not found", name)
	case busy > 0:
		return fmt.Errorf("%w: %d probe(s) already running", ErrBusy, busy)
	}
	return nil
}

// ErrBusy says a run was already in progress, so this one was not started.
var ErrBusy = errors.New("a run is already in progress")

func (a *App) sweep(ctx context.Context) {
	interval := a.Cfg.Probing.StaleAfter.D()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := a.Store.Sweep(); n > 0 {
				a.Log.Debug("swept stale series", "count", n)
			}
		}
	}
}

// Reopen re-opens file sinks, for logrotate.
func (a *App) Reopen() error {
	var first error
	for _, r := range a.reopeners {
		if err := r(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Close shuts the sinks down; call it after Start has returned.
func (a *App) Close() error {
	var first error
	for _, c := range a.closers {
		if err := c(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
