package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

func send(t *testing.T, method, url, host string, header map[string]string, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// A page in the operator's browser cannot use the API, token or not.
func TestAPIRefusesBrowserPages(t *testing.T) {
	ts := serve(t, newApp(t), nil)
	auth := "Bearer tok"
	for name, h := range map[string]map[string]string{
		"foreign origin":   {"Authorization": auth, "Origin": "https://evil.example"},
		"cross-site":       {"Authorization": auth, "Sec-Fetch-Site": "cross-site"},
		"same-site":        {"Authorization": auth, "Sec-Fetch-Site": "same-site"},
		"malformed origin": {"Authorization": auth, "Origin": "::"},
	} {
		for _, path := range []string{"/api/check?probe=one", "/api/config", "/probe?probe=one"} {
			if code := send(t, http.MethodGet, ts.URL+path, "", h, ""); code != http.StatusForbidden {
				t.Errorf("%s %s: %d", name, path, code)
			}
		}
		if code := send(t, http.MethodPost, ts.URL+"/api/reload", "", h, ""); code != http.StatusForbidden {
			t.Errorf("%s reload: %d", name, code)
		}
	}
	// API clients send neither header; a same-origin page and a typed URL pass.
	for name, h := range map[string]map[string]string{
		"client":      {"Authorization": auth},
		"same origin": {"Authorization": auth, "Origin": ts.URL, "Sec-Fetch-Site": "same-origin"},
		"typed URL":   {"Authorization": auth, "Sec-Fetch-Site": "none"},
	} {
		if code := send(t, http.MethodGet, ts.URL+"/api/check?probe=one", "", h, ""); code != http.StatusOK {
			t.Errorf("%s: %d", name, code)
		}
	}
}

// With no write token, a write is taken only at the node's address: a page
// that rebinds its own name to the node arrives with that name.
func TestOpenWritesAnswerOnlyAtAnAddress(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.WriteAuth = config.AuthNone })
	ts := serve(t, a, nil)
	spec := "name: adhoc\ntype: noop\ntargets: [\"9.9.9.9\"]\nnoop: {}\n"
	if code := send(t, http.MethodPost, ts.URL+"/api/check", "rebound.example:80", nil, spec); code != http.StatusForbidden {
		t.Errorf("a write by name: %d", code)
	}
	for _, host := range []string{"", "localhost:6767", "[::1]:6767"} {
		if code := send(t, http.MethodPost, ts.URL+"/api/check", host, nil, spec); code != http.StatusOK {
			t.Errorf("a write at %q: %d", host, code)
		}
	}
	// A read needs no such care: a scrape by name keeps working.
	if code := send(t, http.MethodGet, ts.URL+"/probe?probe=one", "node.example:6767", map[string]string{"Authorization": "Bearer tok"}, ""); code != http.StatusOK {
		t.Errorf("a read by name: %d", code)
	}
}

func TestConfigSecretsAreForTheWriteToken(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.ReadToken = "ro" })
	ts := serve(t, a, nil)
	doc := "probes:\n  - {name: one, type: noop, targets: [\"https://alice:hunter2@1.1.1.1/\"], noop: {}, labels: {api_key: LABEL-SECRET}}\n"
	if code, out := do(t, ts, http.MethodPut, "/api/config", "tok", doc); code != http.StatusOK {
		t.Fatalf("put: %d %s", code, out)
	}
	_, read := do(t, ts, http.MethodGet, "/api/config", "ro", "")
	if strings.Contains(read, "hunter2") || strings.Contains(read, "LABEL-SECRET") {
		t.Errorf("the read token got the secrets:\n%s", read)
	}
	if _, full := do(t, ts, http.MethodGet, "/api/config", "tok", ""); full != doc {
		t.Errorf("the write token got another document:\n%s", full)
	}
	// Nor does the password reach the pages nobody signs in to.
	for _, path := range []string{"/metrics", "/status.json", "/status/data"} {
		if _, out := do(t, ts, http.MethodGet, path, "", ""); strings.Contains(out, "hunter2") {
			t.Errorf("%s carries the password", path)
		}
	}
}

func TestTokenComparison(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.API.ReadToken = "ro" })
	ts := serve(t, a, nil)
	for token, want := range map[string]int{"tok": 200, "ro": 200, "to": 401, "tokk": 401, "": 401, "Bearer tok": 401} {
		if code, _ := do(t, ts, http.MethodGet, "/api/check?probe=one", token, ""); code != want {
			t.Errorf("token %q: %d, want %d", token, code, want)
		}
	}
}
