package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A value from the environment is data: whatever it holds, it stays the value
// of the key it stands in for.
func TestEnvValuesStayInsideTheirScalar(t *testing.T) {
	for _, secret := range []string{"s3cret #2", "a: b", "{x", "line\nfoo: bar", "'quoted\""} {
		t.Setenv("ARGUS_TEST_TOKEN", secret)
		path := filepath.Join(t.TempDir(), "argus.yaml")
		if err := os.WriteFile(path, []byte("http:\n  api:\n    enabled: true\n    token: ${ARGUS_TEST_TOKEN}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadServer(path)
		if err != nil {
			t.Fatalf("%q: %v", secret, err)
		}
		if cfg.HTTP.API.Token != secret {
			t.Errorf("token loaded as %q, want %q", cfg.HTTP.API.Token, secret)
		}
	}
}

// A plain reference is typed by what it expands to, as if it had been written out.
func TestEnvNumbersStayNumbers(t *testing.T) {
	t.Setenv("ARGUS_TEST_N", "3")
	p, err := ParseProbes([]byte(`probes:
  - name: a
    type: http
    targets: ["https://a.example/"]
    requests_per_probe: ${ARGUS_TEST_N}
    labels: {n: "${ARGUS_TEST_N}"}
    http:
      port: ${ARGUS_TEST_N}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := p.List[0]
	if got.RequestsPerProbe != 3 || got.Labels["n"] != "3" {
		t.Fatalf("requests_per_probe=%d labels=%v", got.RequestsPerProbe, got.Labels)
	}
	var opts struct{ Port int }
	if err := got.DecodeOptions(&opts); err != nil || opts.Port != 3 {
		t.Fatalf("prober block: port=%d err=%v", opts.Port, err)
	}
}

func TestUnsetEnvIsLeftAsWritten(t *testing.T) {
	p, err := ParseProbes([]byte(`probes: [{name: a, type: http, targets: ["https://a.example/"], labels: {k: "${ARGUS_TEST_UNSET_VAR}"}, http: {}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.List[0].Labels["k"]; got != "${ARGUS_TEST_UNSET_VAR}" {
		t.Fatalf("label = %q", got)
	}
}

func TestLoadFilesKeepsTheTextAsWritten(t *testing.T) {
	t.Setenv("ARGUS_TEST_HOST", "secret.internal")
	dir := t.TempDir()
	one := write(t, dir, "10-a.yaml", "probes: [{name: a, type: tcp, targets: [\"${ARGUS_TEST_HOST}:1\"]}]")
	two := write(t, dir, "20-b.yaml", "probes: [{name: b, type: tcp, targets: [\"b:1\"]}]\n")

	single, err := LoadFiles(one)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(one); string(single.Document) != string(raw) {
		t.Fatalf("a single file is served as it is, got:\n%s", single.Document)
	}
	if fp, _ := Fingerprint(one); single.Fingerprint != fp {
		t.Fatal("the fingerprint differs from Fingerprint's")
	}
	if single.Probes.List[0].Targets.Static[0].Host != "secret.internal" {
		t.Fatal("the probes are not expanded")
	}

	both, err := LoadFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(both.Document)
	if strings.Contains(doc, "secret.internal") || !strings.Contains(doc, "${ARGUS_TEST_HOST}") {
		t.Fatalf("the text is expanded:\n%s", doc)
	}
	if !strings.Contains(doc, "--- # "+one) || !strings.Contains(doc, "--- # "+two) {
		t.Fatalf("each file is not its own document:\n%s", doc)
	}
	// Sent back whole, the stream must be refused rather than cut to its first file.
	if _, err := ParseProbes(both.Document); err == nil || !strings.Contains(err.Error(), "single YAML document") {
		t.Fatalf("a stream of documents was accepted: %v", err)
	}
}

func TestEmptyTrailingDocumentIsAccepted(t *testing.T) {
	if _, err := ParseProbes([]byte("probes: []\n---\n")); err != nil {
		t.Fatal(err)
	}
}
