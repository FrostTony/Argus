package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/logging"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// Server is the node's own configuration: identity, resolution, scheduling and
// where metrics go.
type Server struct {
	Node      Node       `yaml:"node"`
	HTTP      HTTPServer `yaml:"http"`
	Resolver  Resolver   `yaml:"resolver"`
	Probing   Probing    `yaml:"probing"`
	Surfacers []Surfacer `yaml:"surfacers"`
	Logging   Logging    `yaml:"logging"`
}

// Node labels reach every metric prefixed with node_, so they never collide
// with a target's own labels.
type Node struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels"`
}

type HTTPServer struct {
	Listen      string     `yaml:"listen"`
	MetricsPath string     `yaml:"metrics_path"`
	Timeout     Duration   `yaml:"timeout"`
	TLS         *ServerTLS `yaml:"tls"`
	API         API        `yaml:"api"`
}

// API is the check-configuration endpoint; reads and writes authorise separately.
type API struct {
	Enabled bool `yaml:"enabled"`
	// ProbeEndpoint serves /probe, one check on demand as a Prometheus
	// exposition. On by default, and authorised like /api/check.
	ProbeEndpoint bool `yaml:"probe_endpoint"`
	// ProbeAliases adds blackbox_exporter's names for the series it and Argus
	// both measure, on /probe only. On by default: the endpoint exists to be
	// scraped by a configuration written for blackbox.
	ProbeAliases bool `yaml:"probe_aliases"`
	// Token authorises both reads and writes unless ReadToken is set.
	Token string `yaml:"token"`
	// ReadToken, when set, authorises reads only.
	ReadToken string `yaml:"read_token"`
	// ReadAuth and WriteAuth are "token" or "none".
	ReadAuth  string `yaml:"read_auth"`
	WriteAuth string `yaml:"write_auth"`
}

const (
	AuthToken = "token"
	AuthNone  = "none"
)

// ReadTokens lists the tokens that authorise a read.
func (a API) ReadTokens() []string {
	if a.ReadAuth == AuthNone {
		return nil
	}
	if a.ReadToken != "" {
		// The main token authorises reads as well.
		return []string{a.ReadToken, a.Token}
	}
	return []string{a.Token}
}

// WriteTokens lists the tokens that authorise a change; never the read token.
func (a API) WriteTokens() []string {
	if a.WriteAuth == AuthNone {
		return nil
	}
	return []string{a.Token}
}

// ServerTLS serves /metrics and the API over TLS.
type ServerTLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ClientCAFile requires a client certificate signed by this CA.
	ClientCAFile string `yaml:"client_ca_file"`
}

type Resolver struct {
	// Mode: system uses the OS resolver; custom queries the listed servers and
	// so yields TTL and rcode.
	Mode        string   `yaml:"mode"`
	Servers     []string `yaml:"servers"`
	IPFamily    string   `yaml:"ip_family"` // ipv4 | ipv6 | both
	MaxBackends int      `yaml:"max_backends"`
	Timeout     Duration `yaml:"timeout"`
	// CacheTTL: 0 honours the TTL from the answer, >0 pins a fixed lifetime.
	CacheTTL Duration `yaml:"cache_ttl"`
	MinTTL   Duration `yaml:"min_ttl"`
	MaxTTL   Duration `yaml:"max_ttl"`
	// StableOrder sorts addresses so DNS rotation does not rename backends.
	StableOrder bool `yaml:"stable_order"`
}

type Probing struct {
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
	// Jitter is a fraction of the interval.
	Jitter float64 `yaml:"jitter"`
	// MaxConcurrent caps simultaneous backend requests across the node.
	MaxConcurrent int `yaml:"max_concurrent"`
	// PerBackend fans probes out across all of a target's addresses.
	PerBackend bool `yaml:"per_backend"`
	// StaleAfter is how long a series survives without an update.
	StaleAfter Duration `yaml:"stale_after"`
	// MaxSeries caps the metric store; new series past it are dropped and counted.
	MaxSeries      int       `yaml:"max_series"`
	LatencyBuckets []float64 `yaml:"latency_buckets"`
	// WatchConfig re-reads the check files on this interval and reloads on a
	// change. Zero, the default, leaves SIGHUP and the API as the only ways in.
	WatchConfig Duration `yaml:"watch_config"`
}

// Surfacer activates the block named by Type.
type Surfacer struct {
	Type        string               `yaml:"type"`
	Prometheus  *PrometheusSurfacer  `yaml:"prometheus"`
	File        *FileSurfacer        `yaml:"file"`
	RemoteWrite *RemoteWriteSurfacer `yaml:"remote_write"`
}

type PrometheusSurfacer struct {
	Prefix            string `yaml:"prefix"`
	IncludeTimestamps bool   `yaml:"include_timestamps"`
}

type FileSurfacer struct {
	// Path is a file, or "stdout"/"stderr".
	Path string `yaml:"path"`
}

type RemoteWriteSurfacer struct {
	URL           string            `yaml:"url"`
	Interval      Duration          `yaml:"interval"`
	Timeout       Duration          `yaml:"timeout"`
	Headers       map[string]string `yaml:"headers"`
	BasicAuthUser string            `yaml:"basic_auth_user"`
	BasicAuthPass string            `yaml:"basic_auth_pass"`
	MaxBatchSize  int               `yaml:"max_batch_size"`
	MaxRetries    int               `yaml:"max_retries"`
}

type Logging struct {
	Level string `yaml:"level"` // debug | info | warn | error
	// Format: json by default, text for a terminal.
	Format string `yaml:"format"`
	// Source adds the file and line that logged the record.
	Source bool `yaml:"source"`
}

// DefaultServer is what Argus runs with when no file is given.
func DefaultServer() Server {
	return Server{
		HTTP: HTTPServer{
			Listen:      ":6767",
			MetricsPath: "/metrics",
			Timeout:     Duration(30 * time.Second),
			API:         API{ReadAuth: AuthToken, WriteAuth: AuthToken, ProbeEndpoint: true, ProbeAliases: true},
		},
		Resolver: Resolver{
			Mode:        "system",
			IPFamily:    "both",
			MaxBackends: 8,
			Timeout:     Duration(3 * time.Second),
			MinTTL:      Duration(5 * time.Second),
			MaxTTL:      Duration(5 * time.Minute),
			StableOrder: true,
		},
		Probing: Probing{
			Interval:      Duration(60 * time.Second),
			Timeout:       Duration(10 * time.Second),
			Jitter:        0.1,
			MaxConcurrent: 256,
			PerBackend:    true,
			StaleAfter:    Duration(10 * time.Minute),
			MaxSeries:     500_000,
		},
		Surfacers: []Surfacer{{Type: "prometheus"}},
		Logging:   Logging{Level: "info", Format: logging.FormatJSON},
	}
}

var validAuth = map[string]bool{AuthToken: true, AuthNone: true}

func (a API) validate() error {
	if !a.Enabled {
		return nil
	}
	for field, mode := range map[string]string{"read_auth": a.ReadAuth, "write_auth": a.WriteAuth} {
		if !validAuth[mode] {
			return fmt.Errorf("http.api.%s: want token|none, got %q", field, mode)
		}
	}
	if a.WriteAuth == AuthToken && a.Token == "" {
		return fmt.Errorf("http.api.write_auth=token requires http.api.token")
	}
	if a.ReadAuth == AuthToken && a.Token == "" && a.ReadToken == "" {
		return fmt.Errorf("http.api.read_auth=token requires http.api.token or http.api.read_token")
	}
	return nil
}

var (
	validFamilies  = map[string]bool{"ipv4": true, "ipv6": true, "both": true}
	validModes     = map[string]bool{"system": true, "custom": true}
	validSurfacers = map[string]bool{"prometheus": true, "file": true, "remote_write": true}
)

// reservedPaths are the paths the node already serves, the API's included:
// net/http panics while registering a metrics_path that collides with one.
var reservedPaths = map[string]bool{
	"/": true, "/healthz": true, "/status": true, "/status.json": true, "/status/data": true,
	"/api/config": true, "/api/reload": true, "/api/probes": true, "/api/check": true,
	"/probe": true, "/logo.png": true,
}

// Exposition returns the settings /metrics and remote-write render with.
func (s *Server) Exposition() PrometheusSurfacer {
	// Empty by default: probe_success, probe_duration_seconds and the rest are
	// the names blackbox_exporter dashboards and rules already reference. The
	// agent's own series carry app.SelfPrefix in their name instead.
	var out PrometheusSurfacer
	for _, sf := range s.Surfacers {
		if sf.Type == "prometheus" && sf.Prometheus != nil {
			if sf.Prometheus.Prefix != "" {
				out.Prefix = sf.Prometheus.Prefix
			}
			out.IncludeTimestamps = sf.Prometheus.IncludeTimestamps
		}
	}
	return out
}

func (s *Server) Validate() error {
	if !strings.HasPrefix(s.HTTP.MetricsPath, "/") {
		return fmt.Errorf("http.metrics_path: want a path beginning with /, got %q", s.HTTP.MetricsPath)
	}
	if reservedPaths[s.HTTP.MetricsPath] {
		return fmt.Errorf("http.metrics_path: %q is already served by this node", s.HTTP.MetricsPath)
	}
	if !validModes[s.Resolver.Mode] {
		return fmt.Errorf("resolver.mode: want system|custom, got %q", s.Resolver.Mode)
	}
	if s.Resolver.Mode == "custom" && len(s.Resolver.Servers) == 0 {
		return fmt.Errorf("resolver.mode=custom requires resolver.servers")
	}
	if !validFamilies[s.Resolver.IPFamily] {
		return fmt.Errorf("resolver.ip_family: want ipv4|ipv6|both, got %q", s.Resolver.IPFamily)
	}
	if s.Probing.Jitter < 0 || s.Probing.Jitter >= 1 {
		return fmt.Errorf("probing.jitter: want a fraction in [0,1), got %v", s.Probing.Jitter)
	}
	if s.Probing.Interval <= 0 {
		return fmt.Errorf("probing.interval: want a positive duration, got %s", s.Probing.Interval)
	}
	if s.Probing.Timeout <= 0 {
		return fmt.Errorf("probing.timeout: want a positive duration, got %s", s.Probing.Timeout)
	}
	// A timeout above the interval queues runs on top of each other.
	if s.Probing.Timeout > s.Probing.Interval {
		return fmt.Errorf("probing.timeout (%s) exceeds probing.interval (%s)", s.Probing.Timeout, s.Probing.Interval)
	}
	if s.HTTP.TLS != nil && (s.HTTP.TLS.CertFile == "" || s.HTTP.TLS.KeyFile == "") {
		return fmt.Errorf("http.tls: cert_file and key_file are both required")
	}
	if err := s.HTTP.API.validate(); err != nil {
		return err
	}
	if _, err := logging.ParseLevel(s.Logging.Level); err != nil {
		return err
	}
	if !logging.ValidFormat(s.Logging.Format) {
		return fmt.Errorf("logging.format: want json|text, got %q", s.Logging.Format)
	}
	if err := metrics.Buckets(s.Probing.LatencyBuckets).Valid(); err != nil {
		return fmt.Errorf("probing.latency_buckets: %w", err)
	}
	for i, sf := range s.Surfacers {
		if !validSurfacers[sf.Type] {
			return fmt.Errorf("surfacers[%d].type: unknown type %q", i, sf.Type)
		}
		if sf.Type == "remote_write" && (sf.RemoteWrite == nil || sf.RemoteWrite.URL == "") {
			return fmt.Errorf("surfacers[%d]: remote_write.url is required", i)
		}
		if sf.Type == "file" && (sf.File == nil || sf.File.Path == "") {
			return fmt.Errorf("surfacers[%d]: file.path is required", i)
		}
	}
	return nil
}
