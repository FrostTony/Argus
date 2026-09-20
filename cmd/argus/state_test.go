package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

const pushedProbes = `
probes:
  - name: pushed
    type: http
    targets: ["https://pushed.test/"]
    http: {}
`

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTheStateFileWinsOverTheFiles(t *testing.T) {
	dir := t.TempDir()
	files := writeFile(t, dir, "checks.yaml", validProbes)
	state := writeFile(t, dir, "pushed.yaml", pushedProbes)

	ck, err := loadChecks(&flags{probes: multiFlag{files}, state: state}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	list, err := ck.probes.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "pushed" {
		t.Fatalf("the node came back with the files, not with what it was pushed: %+v", list)
	}
	if ck.source != app.SourceAPI {
		t.Fatalf("source is %q, want api", ck.source)
	}
	if want := config.Hash([]byte(pushedProbes)); ck.hash != want {
		t.Fatalf("hash is %q, want the sha256 of the saved bytes", ck.hash)
	}
}

func TestAnUnusableStateFileFallsBackToTheFiles(t *testing.T) {
	dir := t.TempDir()
	files := writeFile(t, dir, "checks.yaml", validProbes)
	state := writeFile(t, dir, "pushed.yaml", "probes: [{name: x, type: nosuchkind}]\n")

	ck, err := loadChecks(&flags{probes: multiFlag{files}, state: state}, quietLog())
	if err != nil {
		t.Fatalf("a broken state file must not stop the node: %v", err)
	}
	if ck.source != app.SourceFile {
		t.Fatalf("source is %q, want file", ck.source)
	}
	// Left on disk: it is the evidence of what the node was asked to run.
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("the state file was removed: %v", err)
	}
}

func TestANodeWithOnlyAStateFileStartsEmpty(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state", "pushed.yaml")

	ck, err := loadChecks(&flags{state: state}, quietLog())
	if err != nil {
		t.Fatalf("a node waiting for its first push must still start: %v", err)
	}
	if ck.source != app.SourceNone {
		t.Fatalf("source is %q, want none", ck.source)
	}
	if len(ck.probes.List) != 0 {
		t.Fatalf("it checks %d probe(s) out of nowhere", len(ck.probes.List))
	}
}

func TestThePersisterWritesTheStateFileAndItsDirectory(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state", "pushed.yaml")

	save := persister(&flags{state: state})
	if save == nil {
		t.Fatal("a state file is a destination, and was refused")
	}
	if err := save([]byte(pushedProbes)); err != nil {
		t.Fatal(err)
	}

	ck, err := loadChecks(&flags{state: state}, quietLog())
	if err != nil {
		t.Fatalf("the node cannot start from what it saved: %v", err)
	}
	list, err := ck.probes.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "pushed" {
		t.Fatalf("the saved configuration is not the one that was pushed: %+v", list)
	}
	// A configuration may carry tokens, so the file it lands in is private.
	info, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("state file mode is %v, want 0600", mode)
	}
}

func TestValidateRefusesAConfigurationThisNodeWouldIgnore(t *testing.T) {
	dir := t.TempDir()
	server := writeFile(t, dir, "argus.yaml", "node: {name: node-1}\n")
	probes := writeFile(t, dir, "checks.yaml", `
probes:
  - name: elsewhere
    type: http
    targets: ["https://example.test/"]
    run_on: "node-2"
    http: {}
`)
	err := cmdValidate([]string{"-config", server, "-probes", probes})
	if err == nil {
		t.Fatal("a configuration with nothing to run here passed validation")
	}
	if !strings.Contains(err.Error(), "node-1") {
		t.Fatalf("the message does not name the node: %v", err)
	}
}

func TestMissingCheckFilesAreNotFatalWithAStateFile(t *testing.T) {
	dir := t.TempDir()
	// The node is driven by the API and the probes directory was never filled:
	// starting empty beats a crash loop until the first push arrives.
	ck, err := loadChecks(&flags{
		probes: multiFlag{filepath.Join(dir, "probes.d")},
		state:  filepath.Join(dir, "pushed.yaml"),
	}, quietLog())
	if err != nil {
		t.Fatalf("a node waiting for its first push must still start: %v", err)
	}
	if ck.source != app.SourceNone {
		t.Fatalf("source is %q, want none", ck.source)
	}
	if _, err := loadChecks(&flags{probes: multiFlag{filepath.Join(dir, "probes.d")}}, quietLog()); err == nil {
		t.Fatal("without a state file, unreadable check files are still an error")
	}
}
