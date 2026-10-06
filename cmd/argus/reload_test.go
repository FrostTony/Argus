package main

import (
	"path/filepath"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

// Waiting empty for a first push is for a node starting up. A running node
// whose files break keeps what it runs, rather than dropping every probe.
func TestBrokenFilesFailTheReload(t *testing.T) {
	dir := t.TempDir()
	files := writeFile(t, dir, "checks.yaml", validProbes)
	f := &flags{probes: multiFlag{files}, state: filepath.Join(dir, "state.yaml")}

	ck, err := loadChecks(f, discard(), true)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(config.DefaultServer(), ck.probes, discard())
	if err != nil {
		t.Fatal(err)
	}
	a.SetConfigMeta(ck.meta)
	writeFile(t, dir, "checks.yaml", validProbes+"    intervl: 5s\n")

	if err := reloader(f, a, discard())(); err == nil {
		t.Fatal("a reload from a broken file succeeded")
	}
	if n := len(a.Runners()); n != 1 {
		t.Fatalf("%d probe(s) left running", n)
	}
	if got := a.ConfigMeta().Source; got != app.SourceFile {
		t.Fatalf("the failed reload recorded source %q", got)
	}
	// At startup the same files leave the node waiting for a push.
	if ck, err := loadChecks(f, discard(), true); err != nil || ck.meta.Source != app.SourceNone {
		t.Fatalf("source=%q err=%v", ck.meta.Source, err)
	}
}

func TestBrokenStateFailsTheReload(t *testing.T) {
	dir := t.TempDir()
	state := writeFile(t, dir, "state.yaml", validProbes+"    intervl: 5s\n")
	if _, err := loadChecks(&flags{state: state}, discard(), false); err == nil {
		t.Fatal("a reload from a broken state file and no files succeeded")
	}
}
