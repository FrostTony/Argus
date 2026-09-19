package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Sorted across both extensions at once, not one extension after the other.
func TestDirectoryFilesAreReadInSortedOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "10-base.yml", "defaults: {interval: 10s}\nprobes: [{name: a, type: http, targets: [\"a.example\"], http: {}}]\n")
	write(t, dir, "90-tuning.yaml", "defaults: {interval: 90s}\n")

	probes, err := LoadProbes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := probes.Defaults.Interval.D(); got != 90*time.Second {
		t.Fatalf("interval = %s, want the later file's 90s", got)
	}
}

func TestTemplateRedefinedAcrossFilesIsRejected(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "templates: {web: {type: http, http: {}}}\n")
	write(t, dir, "b.yaml", "templates: {web: {type: tcp, tcp: {}}}\n")

	if _, err := LoadProbes(dir); err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("want the duplicate template named, got %v", err)
	}
}

func TestLoadProbesNeedsAtLeastOneFile(t *testing.T) {
	if _, err := LoadProbes(t.TempDir()); err == nil {
		t.Fatal("an empty directory must be an error, not an empty configuration")
	}
	if _, err := LoadProbes(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func TestUnknownFieldInAFileIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "p.yaml", "probes: [{name: a, type: http, targest: [\"a.example\"], http: {}}]\n")
	_, err := LoadProbes(path)
	if err == nil || !strings.Contains(err.Error(), "targest") {
		t.Fatalf("want the unknown field named, got %v", err)
	}
	if !strings.Contains(err.Error(), "p.yaml") {
		t.Errorf("the message does not say which file: %v", err)
	}
}

// Substitution happens before the YAML is parsed, so a token may sit anywhere.
func TestFilesExpandEnvironmentReferences(t *testing.T) {
	t.Setenv("ARGUS_TEST_TOKEN", "s3cret")
	dir := t.TempDir()
	path := write(t, dir, "server.yaml", "http:\n  api:\n    enabled: true\n    token: ${ARGUS_TEST_TOKEN}\n")

	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.API.Token != "s3cret" {
		t.Fatalf("token = %q", cfg.HTTP.API.Token)
	}
}

func TestLoadServerWithoutAFileValidates(t *testing.T) {
	cfg, err := LoadServer("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen == "" || cfg.Probing.Interval.D() <= 0 {
		t.Fatalf("defaults are incomplete: %+v", cfg.HTTP)
	}
}

func TestServerFileErrorsNameTheFile(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "argus.yaml", "http: {metrics_path: metrics}\n")
	_, err := LoadServer(path)
	if err == nil || !strings.Contains(err.Error(), "argus.yaml") {
		t.Fatalf("want the file named, got %v", err)
	}
}

// Templates and defaults survive the round trip; they are not resolved away.
func TestSaveProbesRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checks.yaml")
	original := `
defaults:
  interval: 30s
  labels: {env: prod}
templates:
  web:
    type: http
    timeout: 5s
    http: {validators: [{name: status, status: "200-399"}]}
probes:
  - name: site
    template: web
    targets: ["https://example.test/", "db.example.test:5432"]
  - name: gone
    template: web
    disabled: true
    targets: ["https://old.example.test/"]
`
	loaded, err := ParseProbes([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveProbes(path, []byte(original)); err != nil {
		t.Fatal(err)
	}

	again, err := LoadProbes(path)
	if err != nil {
		t.Fatalf("what was written cannot be read back: %v", err)
	}
	want, err := loaded.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d probes survived the round trip, want %d", len(got), len(want))
	}
	// Compared as YAML: a yaml.Node carries the line it was read from.
	for i := range want {
		if a, b := asYAML(t, got[i]), asYAML(t, want[i]); a != b {
			t.Errorf("probe %q changed:\n got %s\nwant %s", want[i].Name, a, b)
		}
	}
}

func asYAML(t *testing.T, v any) string {
	t.Helper()
	out, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestSaveProbesLeavesTheFileAloneWhenItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory anyway")
	}
	dir := t.TempDir()
	path := write(t, dir, "checks.yaml", "probes: [{name: a, type: http, targets: [\"a.example\"], http: {}}]\n")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	next := []byte(`probes: [{name: b, type: http, targets: ["b.example"], http: {}}]`)
	if err := SaveProbes(path, next); err == nil {
		t.Fatal("a write into a read-only directory reported success")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "a.example") {
		t.Fatalf("the original file did not survive the failed write:\n%s", body)
	}
}

func TestFingerprintFollowsTheContent(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "checks.yaml", validProbesYAML)

	first, err := Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}

	// A touch is not a change.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if again, _ := Fingerprint(dir); again != first {
		t.Error("the fingerprint moved when only the modification time did")
	}

	// An edit is.
	write(t, dir, "checks.yaml", validProbesYAML+"\n# edited\n")
	if edited, _ := Fingerprint(dir); edited == first {
		t.Error("the fingerprint did not move when the file changed")
	}

	// So is a probe arriving as a new file in a watched directory.
	before, _ := Fingerprint(dir)
	write(t, dir, "more.yaml", "probes: [{name: b, type: http, targets: [\"b.example\"], http: {}}]\n")
	if after, _ := Fingerprint(dir); after == before {
		t.Error("the fingerprint did not move when a file was added")
	}
}

const validProbesYAML = "probes: [{name: a, type: http, targets: [\"a.example\"], http: {}}]\n"

func TestAPIAndFileAgreeOnEnv(t *testing.T) {
	t.Setenv("ARGUS_TEST_HOST", "db.internal:5432")
	doc := []byte("probes:\n  - name: a\n    type: tcp\n    targets: [\"${ARGUS_TEST_HOST}\"]\n")

	viaAPI, err := ParseProbes(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "probes.yaml")
	if err := SaveProbes(path, doc); err != nil {
		t.Fatal(err)
	}
	viaFile, err := LoadProbes(path)
	if err != nil {
		t.Fatal(err)
	}
	if a, f := viaAPI.List[0].Targets.Static[0].Host, viaFile.List[0].Targets.Static[0].Host; a != "db.internal" || a != f {
		t.Fatalf("the API runs host %q, the saved file %q", a, f)
	}
	// The reference is what reaches the disk, not the secret it stands for.
	if saved, _ := os.ReadFile(path); !strings.Contains(string(saved), "${ARGUS_TEST_HOST}") {
		t.Fatalf("the saved file lost the reference:\n%s", saved)
	}
}

func TestSaveProbesKeepsModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	real := write(t, dir, "real.yaml", "probes: []\n")
	if err := os.Chmod(real, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "probes.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}

	if err := SaveProbes(link, []byte("probes: [] # saved\n")); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a file")
	}
	if info, _ := os.Stat(real); info.Mode().Perm() != 0o644 {
		t.Errorf("mode changed from 0644 to %o", info.Mode().Perm())
	}
	if body, _ := os.ReadFile(real); !strings.Contains(string(body), "saved") {
		t.Error("the save did not reach the file behind the link")
	}
}
