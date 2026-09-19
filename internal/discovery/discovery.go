// Package discovery turns configured target sources into a probe.TargetSource.
// A dynamic source refreshes in the background and keeps the last good list.
package discovery

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

const defaultRefresh = 5 * time.Minute

// Loader fetches the current target list from one source.
type Loader interface {
	Load(ctx context.Context) ([]probe.Target, error)
	Describe() string
}

// Build picks the source described by the configuration.
func Build(name string, t config.Targets, log *slog.Logger) (probe.TargetSource, error) {
	if len(t.Static) > 0 {
		targets, err := Convert(t.Static)
		if err != nil {
			return nil, err
		}
		return probe.StaticTargets(targets), nil
	}

	var loader Loader
	switch {
	case t.File != nil:
		if t.File.Path == "" {
			return nil, fmt.Errorf("targets.file.path is required")
		}
		loader = &fileLoader{path: t.File.Path}
	case t.HTTP != nil:
		if t.HTTP.URL == "" {
			return nil, fmt.Errorf("targets.http.url is required")
		}
		loader = &httpLoader{
			url:     t.HTTP.URL,
			headers: t.HTTP.Headers,
			client:  &http.Client{Timeout: positive(t.HTTP.Timeout.D(), 10*time.Second)},
		}
	default:
		return nil, fmt.Errorf("targets are empty")
	}

	d := &Dynamic{
		probe:   name,
		loader:  loader,
		refresh: positive(t.Refresh.D(), defaultRefresh),
		log:     log,
	}
	// Load once up front so a broken source fails validation, not the first tick.
	if err := d.Refresh(context.Background()); err != nil {
		return nil, fmt.Errorf("targets %s: %w", loader.Describe(), err)
	}
	return d, nil
}

// Dynamic serves the last successfully loaded list.
type Dynamic struct {
	probe   string
	loader  Loader
	refresh time.Duration
	log     *slog.Logger

	current atomic.Pointer[[]probe.Target]
	fails   atomic.Int64
	loaded  atomic.Int64 // unix nanos of the last successful load
}

func (d *Dynamic) Targets() []probe.Target {
	if p := d.current.Load(); p != nil {
		return *p
	}
	return nil
}

func (d *Dynamic) Describe() string { return d.loader.Describe() }

// Failures counts refreshes that did not produce a list, since start.
func (d *Dynamic) Failures() int64 { return d.fails.Load() }

// Age is how long the served list has been standing.
func (d *Dynamic) Age() time.Duration {
	at := d.loaded.Load()
	if at == 0 {
		return 0
	}
	return time.Since(time.Unix(0, at))
}

// Start refreshes until the context is cancelled.
func (d *Dynamic) Start(ctx context.Context) {
	t := time.NewTicker(d.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.Refresh(ctx); err != nil {
				d.log.Warn("target discovery failed",
					"probe", d.probe, "source", d.loader.Describe(), "err", err)
			}
		}
	}
}

// Refresh loads the list once. A failure leaves the previous list in place.
func (d *Dynamic) Refresh(ctx context.Context) error {
	targets, err := d.loader.Load(ctx)
	if err != nil {
		d.fails.Add(1)
		return err
	}
	// An empty answer is not an error, but it leaves the probe with no targets.
	if len(targets) == 0 && len(d.Targets()) > 0 {
		d.log.Warn("target discovery returned an empty list; the probe now has no targets",
			"probe", d.probe, "source", d.loader.Describe())
	}
	d.current.Store(&targets)
	d.loaded.Store(time.Now().UnixNano())
	return nil
}

// Convert maps configured targets to runtime ones.
func Convert(in []config.Target) ([]probe.Target, error) {
	out := make([]probe.Target, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, t := range in {
		labels, err := labelsOf(t.Labels)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", cmp.Or(t.Name, t.URL, t.Host), err)
		}
		tt := probe.Target{Name: t.Name, Host: t.Host, Port: t.Port, Labels: labels}
		if t.URL != "" {
			u, err := url.Parse(t.URL)
			if err != nil {
				return nil, fmt.Errorf("target %q: %w", t.Name, err)
			}
			tt.URL = u
			if tt.Host == "" {
				tt.Host = u.Hostname()
			}
		}
		if tt.Name == "" {
			tt.Name = cmp.Or(t.URL, t.Host)
		}
		if tt.Host == "" {
			return nil, fmt.Errorf("target %q has no host", tt.Name)
		}
		if seen[tt.Name] {
			return nil, fmt.Errorf("target %q is listed twice", tt.Name)
		}
		seen[tt.Name] = true
		out = append(out, tt)
	}
	return out, nil
}

type fileLoader struct{ path string }

func (f *fileLoader) Describe() string { return "file:" + f.path }

func (f *fileLoader) Load(context.Context) ([]probe.Target, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, err
	}
	var raw []config.Target
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return Convert(raw)
}

type httpLoader struct {
	url     string
	headers map[string]string
	client  *http.Client
}

func (h *httpLoader) Describe() string { return "http:" + h.url }

func (h *httpLoader) Load(ctx context.Context) ([]probe.Target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var raw []config.Target
	// A discovery endpoint accepts the same YAML, JSON and shorthand as the file.
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return Convert(raw)
}

// positive is cmp.Or for durations, where a negative value is not "unset".
func positive(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// labelsOf builds a target's own labels, rejecting names the exposition reserves.
func labelsOf(m map[string]string) (metrics.Labels, error) {
	l := metrics.Labels{}
	for k, v := range m {
		l = l.With(k, v)
	}
	if err := metrics.ValidateLabels(l); err != nil {
		return nil, err
	}
	return l, nil
}
