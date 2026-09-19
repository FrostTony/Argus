package config

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Run in a child process: the failure guarded against is a stack overflow,
// which is fatal and cannot be recovered.
func TestSelfReferencingAnchor(t *testing.T) {
	const doc = `
probes:
  - &p
    name: x
    type: http
    targets: [a.example]
    http: *p
`
	if os.Getenv("ARGUS_REVIEW_CHILD") == "1" {
		_, _ = ParseProbes([]byte(doc))
		os.Exit(0) // returning at all is the point; an error is the right answer
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestSelfReferencingAnchor$")
	cmd.Env = append(os.Environ(), "ARGUS_REVIEW_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ParseProbes took the process down: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("ParseProbes was still recursing through the anchor after 5s (it ends in a stack overflow or the OOM killer)")
	}
}

func TestAliasExpansionIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("probes:\n  - name: x\n    type: http\n    targets: [a.example]\n    http:\n      headers:\n")
	b.WriteString("        a0: &a0 {k: v}\n")
	for i := 1; i <= 7; i++ {
		prev := "*a" + string(rune('0'+i-1))
		b.WriteString("        a" + string(rune('0'+i)) + ": &a" + string(rune('0'+i)) + " [" + strings.Repeat(prev+",", 9) + prev + "]\n")
	}
	if os.Getenv("ARGUS_REVIEW_CHILD") == "2" {
		_, _ = ParseProbes([]byte(b.String()))
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestAliasExpansionIsBounded$")
	cmd.Env = append(os.Environ(), "ARGUS_REVIEW_CHILD=2")
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("a %d-byte document was still being expanded after 5s", b.Len())
	}
}
