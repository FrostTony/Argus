package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const validProbes = `
probes:
  - name: site
    type: http
    targets: ["https://example.test/"]
    http: {}
`

func TestUnknownCommandExplainsItself(t *testing.T) {
	err := run([]string{"frobnicate"})
	if err == nil {
		t.Fatal("an unknown command must be an error")
	}
	if !strings.Contains(err.Error(), "frobnicate") || !strings.Contains(err.Error(), "argus run") {
		t.Fatalf("the message names neither the command nor the alternatives: %v", err)
	}
}

func TestProbesFlagIsRequired(t *testing.T) {
	if _, err := parseFlags("run", nil); err == nil {
		t.Fatal("want an error without -probes")
	}
	f, err := parseFlags("run", []string{"-probes", "a.yaml", "-probes", "b.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.probes) != 2 {
		t.Fatalf("-probes is repeatable: %v", f.probes)
	}
}

func TestLogFlagsOverrideTheFile(t *testing.T) {
	dir := t.TempDir()
	server := writeFile(t, dir, "argus.yaml", "logging: {level: error, format: json}\n")
	probes := writeFile(t, dir, "probes.yaml", validProbes)

	cfg, _, _, err := load(&flags{
		server: server,
		probes: multiFlag{probes},
		level:  "debug",
		format: "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Level != "debug" || cfg.Logging.Format != "text" {
		t.Fatalf("logging = %+v, want the flags", cfg.Logging)
	}
}

func TestValidateBuildsProbesThisNodeSkips(t *testing.T) {
	probes := parseProbes(t, `
probes:
  - name: here
    type: http
    targets: ["https://example.test/"]
    http: {}
  - name: elsewhere
    type: http
    run_on: other-node
    targets: ["https://example.test/"]
    http: {nonsense: true}
`)
	st := app.Status{Probes: []app.ProbeStatus{{Name: "here"}}}

	if _, err := checkIdle(probes, "this-node", st); err == nil {
		t.Fatal("a broken probe meant for another node was called valid")
	} else if !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("the message does not name the bad field: %v", err)
	}
}

func TestIdleProbesAreNamedWithAReason(t *testing.T) {
	probes := parseProbes(t, `
probes:
  - name: off
    type: http
    disabled: true
    targets: ["https://example.test/"]
    http: {}
  - name: theirs
    type: http
    run_on: other-node
    targets: ["https://example.test/"]
    http: {}
`)
	lines, err := checkIdle(probes, "this-node", app.Status{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("listed %d idle probes, want 2: %v", len(lines), lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "disabled") {
		t.Errorf("the disabled probe has no reason: %s", joined)
	}
	if !strings.Contains(joined, "other-node") || !strings.Contains(joined, "this-node") {
		t.Errorf("the run_on reason names neither side: %s", joined)
	}
}

func parseProbes(t *testing.T, body string) config.Probes {
	t.Helper()
	p, err := config.ParseProbes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPersisterNeedsASingleFile(t *testing.T) {
	dir := t.TempDir()
	one := writeFile(t, dir, "checks.yaml", validProbes)
	two := writeFile(t, dir, "more.yaml", validProbes)

	if persister(&flags{probes: multiFlag{one}}) == nil {
		t.Error("a single file is a destination, and was refused")
	}
	if persister(&flags{probes: multiFlag{dir}}) != nil {
		t.Error("a directory was accepted as a destination")
	}
	if persister(&flags{probes: multiFlag{one, two}}) != nil {
		t.Error("two files were accepted as one destination")
	}
	if persister(&flags{probes: multiFlag{filepath.Join(dir, "absent.yaml")}}) != nil {
		t.Error("a path that does not exist was accepted as a destination")
	}
}

func TestWhatThePersisterWritesLoadsAgain(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "checks.yaml", validProbes)

	save := persister(&flags{probes: multiFlag{path}})
	if save == nil {
		t.Fatal("no destination")
	}
	changed := []byte(`
probes:
  - name: site
    type: http
    targets: ["https://example.test/"]
    http: {}
  - name: added
    type: tcp
    targets: ["db.example.test:5432"]
    tcp: {}
`)
	if err := save(changed); err != nil {
		t.Fatal(err)
	}

	_, probes, _, err := load(&flags{probes: multiFlag{path}})
	if err != nil {
		t.Fatalf("the node cannot start from what it saved: %v", err)
	}
	list, err := probes.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[1].Name != "added" {
		t.Fatalf("the saved configuration lost the change: %+v", list)
	}
}

func TestWatchFilesReloadsOnAChange(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "checks.yaml", validProbes)

	reloads := make(chan struct{}, 4)
	reload := func() error {
		reloads <- struct{}{}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go watchFiles(ctx, []string{dir}, 10*time.Millisecond, reload, discard())

	// An unchanged directory must not reload.
	select {
	case <-reloads:
		t.Fatal("an unchanged directory was reloaded")
	case <-time.After(60 * time.Millisecond):
	}

	if err := os.WriteFile(path, []byte(validProbes+"\n# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloads:
	case <-ctx.Done():
		t.Fatal("an edited file was never picked up")
	}

	// One change reloads once, not on every tick after.
	select {
	case <-reloads:
		t.Fatal("one change reloaded twice")
	case <-time.After(120 * time.Millisecond):
	}
}

func TestWatchFilesDoesNotRetryARejectedConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "checks.yaml", validProbes)

	var attempts atomic.Int64
	reload := func() error {
		attempts.Add(1)
		return errors.New("nope")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go watchFiles(ctx, []string{dir}, 10*time.Millisecond, reload, discard())
	time.Sleep(50 * time.Millisecond) // let it take its baseline

	if err := os.WriteFile(path, []byte(validProbes+"\n# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("a rejected configuration was retried %d times", got)
	}
}
