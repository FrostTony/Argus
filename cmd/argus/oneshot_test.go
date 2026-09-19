package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOneshotStoresNothingAndUsesThePrefix(t *testing.T) {
	var pushes atomic.Int64
	rw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
	}))
	defer rw.Close()
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer target.Close()

	dir := t.TempDir()
	cfg := writeFile(t, dir, "argus.yaml", `
surfacers:
  - type: prometheus
    prometheus: {prefix: "acme_"}
  - type: remote_write
    remote_write: {url: "`+rw.URL+`"}
logging: {level: error}
`)
	probes := writeFile(t, dir, "probes.yaml", "probes:\n  - name: site\n    targets: [\""+target.URL+"\"]\n    http: {}\n")

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	err := run([]string{"oneshot", "-config", cfg, "-probes", probes})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if n := pushes.Load(); n != 0 {
		t.Errorf("oneshot pushed %d remote-write request(s)", n)
	}
	if !strings.Contains(string(out), "acme_probe_up") {
		t.Errorf("the configured prefix was ignored:\n%s", out)
	}
}
