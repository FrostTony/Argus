package discovery

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func targetsFrom(t *testing.T, y string) config.Targets {
	t.Helper()
	var tg config.Targets
	if err := yaml.Unmarshal([]byte(y), &tg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return tg
}

func TestStaticTargets(t *testing.T) {
	src, err := Build("p", targetsFrom(t, `["https://a.example/", "b.example:8080"]`), discard())
	if err != nil {
		t.Fatal(err)
	}
	got := src.Targets()
	if len(got) != 2 || got[0].Host != "a.example" || got[1].Port != 8080 {
		t.Fatalf("targets: %+v", got)
	}
}

func TestFileDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`["https://one.example/"]`)

	src, err := Build("p", targetsFrom(t, "file: {path: "+path+"}\nrefresh: 1s"), discard())
	if err != nil {
		t.Fatal(err)
	}
	if got := src.Targets(); len(got) != 1 {
		t.Fatalf("initial load: %+v", got)
	}

	// A changed file is picked up without a restart.
	write(`["https://one.example/", "https://two.example/"]`)
	d := src.(*Dynamic)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := d.Targets(); len(got) != 2 {
		t.Fatalf("after reload: %+v", got)
	}
}

func TestFailedRefreshKeepsTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.yaml")
	if err := os.WriteFile(path, []byte("- https://one.example/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := Build("p", targetsFrom(t, "file: {path: "+path+"}"), discard())
	if err != nil {
		t.Fatal(err)
	}
	d := src.(*Dynamic)

	if err := os.WriteFile(path, []byte("{ this is not a list"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Refresh(context.Background()); err == nil {
		t.Fatal("a broken file must fail the reload")
	}
	if got := d.Targets(); len(got) != 1 {
		t.Fatalf("previous targets were lost: %+v", got)
	}
}

func TestHTTPDiscovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"url": "https://api.example/health", "labels": {"kind": "api"}}]`))
	}))
	defer srv.Close()

	src, err := Build("p", targetsFrom(t, "http: {url: "+srv.URL+", timeout: 5s}"), discard())
	if err != nil {
		t.Fatal(err)
	}
	got := src.Targets()
	if len(got) != 1 || got[0].Labels.Get("kind") != "api" || got[0].Host != "api.example" {
		t.Fatalf("targets: %+v", got)
	}
}

func TestUnreachableSourceFailsBuild(t *testing.T) {
	_, err := Build("p", targetsFrom(t, "file: {path: /nonexistent/targets.json}"), discard())
	if err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestRefreshDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.yaml")
	if err := os.WriteFile(path, []byte("- a.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, _ := Build("p", targetsFrom(t, "file: {path: "+path+"}"), discard())
	if got := src.(*Dynamic).refresh; got != 5*time.Minute {
		t.Fatalf("refresh: %s", got)
	}
}

func TestNegativeDurationsFallBackToTheDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.yaml")
	if err := os.WriteFile(path, []byte("- a.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := Build("p", targetsFrom(t, "file: {path: "+path+"}\nrefresh: -1s"), discard())
	if err != nil {
		t.Fatal(err)
	}
	if got := src.(*Dynamic).refresh; got != defaultRefresh {
		t.Fatalf("refresh = %s, want the default %s", got, defaultRefresh)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`["a.example"]`))
	}))
	defer srv.Close()
	src, err = Build("p", targetsFrom(t, "http: {url: "+srv.URL+", timeout: -5s}"), discard())
	if err != nil {
		t.Fatal(err)
	}
	if got := src.(*Dynamic).loader.(*httpLoader).client.Timeout; got <= 0 {
		t.Fatalf("client timeout = %s, which means no timeout at all", got)
	}
}

func TestReservedTargetLabelIsRejected(t *testing.T) {
	_, err := Convert([]config.Target{{Host: "a.example", Labels: map[string]string{"le": "zone-a"}}})
	if err == nil || !strings.Contains(err.Error(), "le") {
		t.Fatalf("want the reserved label named, got %v", err)
	}
}
