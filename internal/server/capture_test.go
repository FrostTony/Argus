package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/http"
)

func captureSite(t *testing.T) string {
	t.Helper()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<h1>internal admin</h1>"))
	}))
	t.Cleanup(site.Close)
	return site.URL
}

// The node's own network is what a capture must not read unless told to.
func TestCaptureModes(t *testing.T) {
	url := captureSite(t)
	spec := "name: page\ntype: http\ntargets: [\"" + url + "/\"]\nhttp: {}\n"

	for mode, want := range map[string]string{
		config.CapturePublic: "capture does not reach 127.0.0.1",
		config.CaptureAll:    "PGgxPmludGVybmFsIGFkbWluPC9oMT4=", // the page
		config.CaptureOff:    "capture is off on this node",
	} {
		ts := serve(t, newAppWith(t, func(c *config.Server) { c.HTTP.API.Capture = mode }), nil)
		code, out := do(t, ts, http.MethodPost, "/api/check?capture=true", "tok", spec)
		if !strings.Contains(out, want) {
			t.Errorf("%s: %d, want %q in:\n%s", mode, code, want, out)
		}
		if mode == config.CapturePublic && strings.Contains(out, "aW50ZXJuYWw") {
			t.Errorf("public: the page leaked:\n%s", out)
		}
	}
}

func TestCaptureModeIsValidated(t *testing.T) {
	cfg := config.DefaultServer()
	cfg.HTTP.API.Enabled, cfg.HTTP.API.Token = true, "tok"
	if cfg.HTTP.API.Capture != config.CapturePublic {
		t.Errorf("default capture = %q, want public", cfg.HTTP.API.Capture)
	}
	cfg.HTTP.API.Capture = "everything"
	if err := cfg.Validate(); err == nil {
		t.Error("an unknown capture mode was accepted")
	}
}

func TestCheckAnswersAreNeverSniffed(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	for _, path := range []string{"/api/check?probe=one", "/api/check?probe=one&capture=true"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
}

// Compact or indented, the answer is the same document.
func TestCaptureAnswerIsTheSameDocument(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	_, out := do(t, ts, http.MethodGet, "/api/check?probe=one&capture=true", "tok", "")
	if strings.Contains(out, "\n  ") {
		t.Errorf("a capture answer was indented:\n%s", out)
	}
	var res app.CheckResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Probe != "one" || len(res.Targets) != 1 {
		t.Errorf("result = %+v", res)
	}
}
