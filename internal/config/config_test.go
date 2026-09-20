package config

import (
	"gopkg.in/yaml.v3"

	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func loadProbes(t *testing.T, yaml string) Probes {
	t.Helper()
	var p Probes
	if err := decodeStrictBytes([]byte(yaml), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func TestProbeInheritance(t *testing.T) {
	p := loadProbes(t, `
defaults:
  interval: 30s
  timeout: 5s
  labels: {env: prod}

templates:
  web:
    type: http
    labels: {tier: front}
    http:
      method: GET

probes:
  - name: site
    template: web
    labels: {project: demo}
    targets: ["https://example.com/"]
`)
	list, err := p.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := list[0]
	if got.Type != "http" {
		t.Fatalf("type: %q", got.Type)
	}
	if got.Interval.D() != 30*time.Second {
		t.Fatalf("interval: %s", got.Interval)
	}
	// Labels accumulate across all three levels.
	for k, want := range map[string]string{"env": "prod", "tier": "front", "project": "demo"} {
		if got.Labels[k] != want {
			t.Fatalf("label %s=%q, want %q", k, got.Labels[k], want)
		}
	}
	var opts struct {
		Method string `yaml:"method"`
	}
	if err := got.DecodeOptions(&opts); err != nil || opts.Method != "GET" {
		t.Fatalf("prober options not inherited: %v %+v", err, opts)
	}
}

func TestProbeRejectsTwoOptionBlocks(t *testing.T) {
	var p Probes
	err := decodeStrictBytes([]byte(`
probes:
  - name: x
    targets: ["a"]
    http: {}
    tcp: {}
`), &p)
	if err == nil || !strings.Contains(err.Error(), "one parameter block") {
		t.Fatalf("want an error about two blocks, got: %v", err)
	}
}

func TestProbeRejectsUnknownField(t *testing.T) {
	var p Probes
	err := decodeStrictBytes([]byte(`
probes:
  - name: x
    type: http
    targets: ["a"]
    http:
      methd: GET
`), &p)
	if err != nil {
		t.Fatalf("probe decoding itself must not fail: %v", err)
	}
	// A typo in a prober parameter is caught when the block is decoded.
	if err := p.List[0].DecodeOptions(&struct {
		Method string `yaml:"method"`
	}{}); err == nil {
		t.Fatal("the methd typo went unnoticed")
	}
}

func TestDuplicateProbeName(t *testing.T) {
	p := loadProbes(t, `
probes:
  - {name: x, type: http, targets: ["https://a/"]}
  - {name: x, type: http, targets: ["https://b/"]}
`)
	if _, err := p.Resolve(); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("duplicate name not caught: %v", err)
	}
}

func TestTimeoutGreaterThanInterval(t *testing.T) {
	p := loadProbes(t, `
probes:
  - {name: x, type: http, interval: 5s, timeout: 10s, targets: ["https://a/"]}
`)
	if _, err := p.Resolve(); err == nil || !strings.Contains(err.Error(), "exceeds interval") {
		t.Fatalf("timeout > interval not caught: %v", err)
	}
}

func TestTargetShorthand(t *testing.T) {
	cases := []struct {
		in       string
		host     string
		port     int
		wantsURL bool
	}{
		{"https://google.com/api?x=1", "google.com", 0, true},
		{"https://google.com:8443/", "google.com", 8443, true},
		{"db.local:5432", "db.local", 5432, false},
		{"example.com", "example.com", 0, false},
		{"[2001:db8::1]:443", "2001:db8::1", 443, false},
	}
	for _, c := range cases {
		var tg Target
		if err := tg.parseShorthand(c.in, 1); err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if tg.Host != c.host || tg.Port != c.port || (tg.URL != "") != c.wantsURL {
			t.Fatalf("%s -> host=%q port=%d url=%q", c.in, tg.Host, tg.Port, tg.URL)
		}
	}
}

func TestDurationAcceptsBareSeconds(t *testing.T) {
	var s struct {
		D Duration `yaml:"d"`
	}
	if err := decodeStrictBytes([]byte("d: 60"), &s); err != nil {
		t.Fatal(err)
	}
	if s.D.D() != time.Minute {
		t.Fatalf("60 -> %s, want one minute", s.D)
	}
}

func TestExpandEnvLeavesRegexAlone(t *testing.T) {
	t.Setenv("ARGUS_TEST_TOKEN", "s3cret")
	got := string(expandEnv([]byte(`a: ${ARGUS_TEST_TOKEN}` + "\n" + `b: "^\d+$"`)))
	if !strings.Contains(got, "s3cret") {
		t.Fatalf("variable not substituted: %s", got)
	}
	if !strings.Contains(got, `^\d+$`) {
		t.Fatalf("regex was mangled: %s", got)
	}
}

// A default of true or non-zero survives a file that does not mention it.
func TestServerDefaultsSurvivePartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "argus.yaml")
	if err := os.WriteFile(path, []byte("node:\n  name: n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Probing.PerBackend {
		t.Fatal("probing.per_backend lost its default")
	}
	if !cfg.Resolver.StableOrder {
		t.Fatal("resolver.stable_order lost its default")
	}
	if cfg.Probing.Jitter != 0.1 || cfg.HTTP.Listen != ":6767" {
		t.Fatalf("defaults lost: %+v", cfg.Probing)
	}
}

// An explicit false must still win over a default of true.
func TestExplicitFalseOverridesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "argus.yaml")
	if err := os.WriteFile(path, []byte("probing:\n  per_backend: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Probing.PerBackend {
		t.Fatal("an explicit false was ignored")
	}
}

// A surfacer list in the file replaces the default one instead of appending.
func TestSurfacersReplaceDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "argus.yaml")
	body := "surfacers:\n  - type: file\n    file:\n      path: stdout\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Surfacers) != 1 || cfg.Surfacers[0].Type != "file" {
		t.Fatalf("surfacers: %+v", cfg.Surfacers)
	}
}

func TestValidateRejectsConfigsThatCrashAtRuntime(t *testing.T) {
	cases := map[string]func(*Server){
		"metrics path with no slash":   func(c *Server) { c.HTTP.MetricsPath = "metrics" },
		"empty metrics path":           func(c *Server) { c.HTTP.MetricsPath = "" },
		"metrics path over status":     func(c *Server) { c.HTTP.MetricsPath = "/status" },
		"metrics path over the root":   func(c *Server) { c.HTTP.MetricsPath = "/" },
		"metrics path over healthz":    func(c *Server) { c.HTTP.MetricsPath = "/healthz" },
		"zero probing interval":        func(c *Server) { c.Probing.Interval = 0; c.Probing.Timeout = 0 },
		"zero probing timeout":         func(c *Server) { c.Probing.Timeout = 0 },
		"negative probing interval":    func(c *Server) { c.Probing.Interval = Duration(-time.Second) },
		"unknown surfacer":             func(c *Server) { c.Surfacers = []Surfacer{{Type: "carrier-pigeon"}} },
		"remote_write without a url":   func(c *Server) { c.Surfacers = []Surfacer{{Type: "remote_write"}} },
		"jitter of one whole interval": func(c *Server) { c.Probing.Jitter = 1 },
	}
	for name, tune := range cases {
		cfg := DefaultServer()
		tune(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestValidateAcceptsTheDefaults(t *testing.T) {
	cfg := DefaultServer()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
}

func TestInheritedFlagsCanBeTurnedOff(t *testing.T) {
	src := `
defaults:
  disabled: true
  negative_test: true
  ip_fallback: true
  per_backend: true
probes:
  - name: on
    type: noop
    targets: ["1.1.1.1"]
    disabled: false
    negative_test: false
    ip_fallback: false
    per_backend: false
  - name: inherits
    type: noop
    targets: ["1.1.1.1"]
`
	var probes Probes
	if err := yaml.Unmarshal([]byte(src), &probes); err != nil {
		t.Fatal(err)
	}
	list, err := probes.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		want := p.Name == "inherits"
		for name, got := range map[string]*bool{
			"disabled": p.Disabled, "negative_test": p.NegativeTest,
			"ip_fallback": p.IPFallback, "per_backend": p.PerBackend,
		} {
			if Enabled(got, false) != want {
				t.Errorf("probe %q: %s = %v, want %v", p.Name, name, Enabled(got, false), want)
			}
		}
	}
}

func TestNegativeDiscoveryDurationsAreRejected(t *testing.T) {
	cases := map[string]string{
		"targets.refresh": `
probes:
  - name: a
    type: http
    http: {}
    targets: {refresh: -1s, file: {path: /tmp/t.yaml}}
`,
		"targets.http.timeout": `
probes:
  - name: a
    type: http
    http: {}
    targets: {http: {url: "http://example.test/", timeout: -1s}}
`,
	}
	for field, body := range cases {
		p := loadProbes(t, body)
		if _, err := p.Resolve(); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: want a rejection naming the field, got %v", field, err)
		}
	}
}

// net/http panics while registering a metrics_path that collides with a
// path the node already serves.
func TestReservedMetricsPathsIncludeTheAPI(t *testing.T) {
	for _, path := range []string{"/", "/healthz", "/status", "/status.json", "/status/data",
		"/api/config", "/api/reload", "/api/probes", "/api/check"} {
		s := DefaultServer()
		s.HTTP.API.Enabled = true
		s.HTTP.API.Token = "tok"
		s.HTTP.MetricsPath = path
		if err := s.Validate(); err == nil {
			t.Errorf("metrics_path %q was accepted; it collides with a path the node serves", path)
		}
	}
}

// Probe series carry blackbox names, so the exposition adds nothing by default.
func TestExpositionPrefixIsEmptyByDefault(t *testing.T) {
	def := DefaultServer()
	if got := def.Exposition().Prefix; got != "" {
		t.Errorf("default prefix = %q, want empty: probe_success has to be named probe_success", got)
	}

	s := DefaultServer()
	s.Surfacers = []Surfacer{{Type: "prometheus", Prometheus: &PrometheusSurfacer{Prefix: "argus_"}}}
	if got := s.Exposition().Prefix; got != "argus_" {
		t.Errorf("configured prefix = %q, want argus_", got)
	}
}

// A unix probe's target is a socket path, which has no port and nothing to
// resolve; the shorthand parser must not read it as host:port.
func TestSocketPathIsAValidTarget(t *testing.T) {
	var tg Target
	if err := tg.Parse("/var/run/docker.sock"); err != nil {
		t.Fatalf("a socket path was rejected as a target: %v", err)
	}
	if tg.Host != "/var/run/docker.sock" || tg.Port != 0 {
		t.Errorf("parsed as host %q port %d, want the whole path and no port", tg.Host, tg.Port)
	}
}

// probeFields is derived from the struct, so a field that yields no yaml name
// would be silently swallowed as somebody's prober block instead.
func TestEveryProbeFieldIsNamed(t *testing.T) {
	typ := reflect.TypeFor[Probe]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		switch {
		case f.Name == "Options":
			continue // the prober's own block, held as a raw node
		case f.Anonymous:
			t.Errorf("%s is embedded, so its keys would be read as a prober block", f.Name)
		case name == "" || name == "-":
			t.Errorf("%s has no yaml name, so its key would be read as a prober block", f.Name)
		case !probeFields[name]:
			t.Errorf("%s is not in probeFields", name)
		}
	}
}
