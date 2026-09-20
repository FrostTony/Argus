package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/discovery"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// counting is a prober that records how often it ran.
type counting struct{ runs atomic.Int64 }

var probers sync.Map // probe name -> *counting

func init() {
	probe.Register("counting", func(pc config.Probe) (probe.Prober, error) {
		p := &counting{}
		probers.Store(pc.Name, p)
		return p, nil
	})
}

func (c *counting) Probe(context.Context, probe.Request, *metrics.Recorder) probe.Result {
	c.runs.Add(1)
	return probe.Result{}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func probesFrom(t *testing.T, body string) config.Probes {
	t.Helper()
	var p config.Probes
	dec := yaml.NewDecoder(strings.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func testServer() config.Server {
	s := config.DefaultServer()
	s.Probing.Interval = config.Duration(time.Hour)
	s.Probing.Timeout = config.Duration(time.Second)
	s.Probing.Jitter = 0
	s.Logging.Level = "error"
	return s
}

const twoProbes = `
probes:
  - {name: a, type: counting, targets: ["1.1.1.1"], counting: {}}
  - {name: b, type: counting, targets: ["2.2.2.2"], counting: {}}
`

// A kept runner keeps its ticker; a rebuilt one would restart the schedule.
func TestReloadKeepsUnchangedProbes(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, twoProbes), discard())
	if err != nil {
		t.Fatal(err)
	}
	before := byName(a.Runners())

	const changed = `
probes:
  - {name: a, type: counting, targets: ["1.1.1.1"], counting: {}}
  - {name: b, type: counting, interval: 30s, targets: ["2.2.2.2"], counting: {}}
  - {name: c, type: counting, targets: ["3.3.3.3"], counting: {}}
`
	if err := a.Reload(probesFrom(t, changed)); err != nil {
		t.Fatal(err)
	}
	after := byName(a.Runners())

	if after["a"] != before["a"] {
		t.Error("unchanged probe a was rebuilt")
	}
	if after["b"] == before["b"] {
		t.Error("probe b changed its interval but was not rebuilt")
	}
	if after["c"] == nil {
		t.Error("new probe c was not started")
	}
}

// Series of a removed probe go at once, not at stale_after.
func TestReloadDropsRemovedProbeSeries(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, twoProbes), discard())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.RunOnce(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !hasProbe(a.Store.Snapshot(), "b") {
		t.Fatal("probe b never produced metrics")
	}

	const onlyA = `
probes:
  - {name: a, type: counting, targets: ["1.1.1.1"], counting: {}}
`
	if err := a.Reload(probesFrom(t, onlyA)); err != nil {
		t.Fatal(err)
	}
	if hasProbe(a.Store.Snapshot(), "b") {
		t.Error("series of the removed probe b are still exposed")
	}
	if !hasProbe(a.Store.Snapshot(), "a") {
		t.Error("series of the surviving probe a were dropped")
	}
}

// A rejected configuration leaves the running one untouched.
func TestReloadRejectsBrokenConfig(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, twoProbes), discard())
	if err != nil {
		t.Fatal(err)
	}
	before := byName(a.Runners())

	err = a.Reload(probesFrom(t, `
probes:
  - {name: a, type: nosuchprober, targets: ["1.1.1.1"]}
`))
	if err == nil {
		t.Fatal("an unknown prober must fail the reload")
	}
	if len(a.Runners()) != len(before) {
		t.Fatalf("running probes changed after a failed reload: %d", len(a.Runners()))
	}
}

func byName(runners []*probe.Runner) map[string]*probe.Runner {
	m := make(map[string]*probe.Runner, len(runners))
	for _, r := range runners {
		m[r.Name] = r
	}
	return m
}

func hasProbe(samples []metrics.Sample, name string) bool {
	for _, s := range samples {
		if s.Labels.Get("probe") == name {
			return true
		}
	}
	return false
}

func TestConcurrentReloadsLeaveConsistentState(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, twoProbes), discard())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Start(ctx)
	time.Sleep(20 * time.Millisecond)

	configs := []string{
		"probes:\n  - {name: a, type: counting, targets: [\"1.1.1.1\"], counting: {}}\n  - {name: x, type: counting, targets: [\"3.3.3.3\"], counting: {}}\n",
		"probes:\n  - {name: a, type: counting, targets: [\"1.1.1.1\"], counting: {}}\n  - {name: y, type: counting, targets: [\"4.4.4.4\"], counting: {}}\n",
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := a.Reload(probesFrom(t, configs[i%2])); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	// Whichever config won, the node must be running exactly that one.
	names := map[string]bool{}
	for _, r := range a.Runners() {
		names[r.Name] = true
	}
	if len(names) != 2 || !names["a"] {
		t.Fatalf("probes after concurrent reloads: %v", names)
	}
	if names["x"] == names["y"] {
		t.Fatalf("both losing and winning configs are running: %v", names)
	}
	// Replaced probes stop asynchronously, so give them a moment to drain.
	deadline := time.Now().Add(2 * time.Second)
	for a.Running() > 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := a.Running(); got != 2 {
		t.Fatalf("%d probes still running, want 2", got)
	}
}

// A frozen source keeps serving its last good list, so only the failure count
// and the age of that list report it.
func TestDiscoveryHealthIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.yaml")
	if err := os.WriteFile(path, []byte("- 1.1.1.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Build(testServer(), probesFrom(t, `
probes:
  - name: a
    type: counting
    counting: {}
    targets: {file: {path: `+path+`}}
`), discard())
	if err != nil {
		t.Fatal(err)
	}
	src := a.Runners()[0].Source.(*discovery.Dynamic)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := src.Refresh(context.Background()); err == nil {
		t.Fatal("a missing file must fail the refresh")
	}

	rec := metrics.NewRecorder(metrics.Labels{})
	a.collectSelf(rec)
	fails, ok := selfGauge(rec.Samples(), "argus_discovery_failures")
	if !ok {
		t.Fatal("discovery_failures is never emitted, so a dead source is invisible")
	}
	if fails != 1 {
		t.Errorf("discovery_failures = %v, want 1", fails)
	}
	if _, ok := selfGauge(rec.Samples(), "argus_discovery_age_seconds"); !ok {
		t.Error("discovery_age_seconds is missing")
	}
}

func selfGauge(samples []metrics.Sample, name string) (float64, bool) {
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		if g, ok := s.Value.(metrics.Gauge); ok {
			return float64(g), true
		}
	}
	return 0, false
}

func TestVersionIsAReleaseNumber(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(Version) {
		t.Fatalf("Version = %q, want a release number", Version)
	}
}

func TestVersionStringCarriesTheCommit(t *testing.T) {
	version, revision := Version, Revision
	t.Cleanup(func() { Version, Revision = version, revision })

	Version, Revision = "1.2.3", "abc123def456"
	if got := VersionString(); got != "1.2.3+abc123def456" {
		t.Errorf("VersionString() = %q", got)
	}
	// Without a revision the string is the release alone.
	Revision = ""
	if got := VersionString(); got != "1.2.3" {
		t.Errorf("VersionString() without a revision = %q", got)
	}
}

// The revision label appears only when there is a revision.
func TestBuildInfoCarriesTheVersion(t *testing.T) {
	a, err := Build(testServer(), probesFrom(t, twoProbes), discard())
	if err != nil {
		t.Fatal(err)
	}
	version, revision := Version, Revision
	t.Cleanup(func() { Version, Revision = version, revision })

	for _, rev := range []string{"abc123def456", ""} {
		Version, Revision = "1.2.3", rev
		rec := metrics.NewRecorder(metrics.Labels{})
		a.collectSelf(rec)

		var found bool
		for _, s := range rec.Samples() {
			if s.Name != "argus_build_info" {
				continue
			}
			found = true
			if got := string(s.Value.(metrics.Info)); got != "1.2.3" {
				t.Errorf("build_info = %q, want the version", got)
			}
			if got := s.Labels.Get("revision"); got != rev {
				t.Errorf("revision label = %q, want %q", got, rev)
			}
		}
		if !found {
			t.Fatal("build_info is not reported")
		}
	}
}
