package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

const quotaProbes = `
probes:
  - name: paid
    type: noop
    noop: {}
    cron: "0 * * * *"
    timezone: UTC
    quota: {requests: 2, per: day}
    targets: ["1.1.1.1"]
`

func quotaApp(t *testing.T, ledger string) *app.App {
	t.Helper()
	cfg := config.DefaultServer()
	cfg.HTTP.API.Enabled = true
	cfg.HTTP.API.Token = "tok"
	cfg.Probing.QuotaFile = ledger
	var probes config.Probes
	if err := yaml.Unmarshal([]byte(quotaProbes), &probes); err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(cfg, probes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestOnDemandRunsDrawOnTheQuota(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "quota.json")
	ts := serve(t, quotaApp(t, ledger), nil)

	// Two requests a day: a run through the API and a check take both.
	if code, body := do(t, ts, http.MethodPost, "/api/probes?name=paid", "tok", ""); code != http.StatusOK {
		t.Fatalf("run: %d %s", code, body)
	}
	if code, body := do(t, ts, http.MethodGet, "/api/check?probe=paid", "tok", ""); code != http.StatusOK {
		t.Fatalf("check: %d %s", code, body)
	}

	for _, path := range []string{"/api/check?probe=paid", "/probe?probe=paid"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("%s over quota: %d %s, want 429", path, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "quota exhausted") {
			t.Fatalf("%s: the answer does not say why: %s", path, body)
		}
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || secs <= 0 || secs > 86400 {
			t.Fatalf("%s: Retry-After %q, want the seconds until midnight", path, resp.Header.Get("Retry-After"))
		}
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/probes?name=paid", "tok", ""); code != http.StatusTooManyRequests {
		t.Fatalf("run over quota: %d, want 429", code)
	}

	// A restart finds the day spent.
	again := serve(t, quotaApp(t, ledger), nil)
	if code, _ := do(t, again, http.MethodGet, "/api/check?probe=paid", "tok", ""); code != http.StatusTooManyRequests {
		t.Fatalf("after a restart: %d, want 429", code)
	}
}

func TestStatusShowsCronAndQuota(t *testing.T) {
	a := quotaApp(t, "")
	ts := serve(t, a, nil)
	do(t, ts, http.MethodPost, "/api/probes?name=paid", "tok", "")

	_, body := do(t, ts, http.MethodGet, "/status.json", "", "")
	var st struct {
		Probes []struct {
			Name         string           `json:"name"`
			Interval     string           `json:"interval"`
			Cron         string           `json:"cron"`
			NextRun      string           `json:"next_run"`
			Quota        *app.QuotaStatus `json:"quota"`
			RequestsSent int64            `json:"requests_sent"`
		} `json:"probes"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	p := st.Probes[0]
	if p.Cron != "0 * * * *" || p.NextRun == "" || p.Interval != "1h0m0s" {
		t.Fatalf("cadence: cron %q next %q interval %q", p.Cron, p.NextRun, p.Interval)
	}
	if q := p.Quota; q == nil || q.Limit != 2 || q.Used != 1 || q.Remaining != 1 || q.UsedRatio != 0.5 || q.Per != "day" || q.Exhausted {
		t.Fatalf("quota: %+v", p.Quota)
	}
	if p.RequestsSent != 1 {
		t.Fatalf("requests_sent = %d, want 1", p.RequestsSent)
	}

	_, metrics := do(t, ts, http.MethodGet, "/metrics", "", "")
	for _, want := range []string{
		`argus_probe_quota_limit{probe="paid"} 2`,
		`argus_probe_quota_used{probe="paid"} 1`,
		`argus_probe_quota_used_ratio{probe="paid"} 0.5`,
		`argus_probe_requests_sent{probe="paid"} 1`,
		`argus_probe_next_run_timestamp_seconds{probe="paid"}`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

func TestQuotaBelowOneScenarioRunIsRefused(t *testing.T) {
	cfg := config.DefaultServer()
	var probes config.Probes
	// Three steps are three requests; a quota of two would never let it run.
	doc := `
scenarios:
  - name: walk
    interval: 1h
    timeout: 10s
    quota: {requests: 2, per: day}
    targets: [http://127.0.0.1:1]
    steps: [{name: a, get: /}, {name: b, get: /}, {name: c, get: /}]
`
	if err := yaml.Unmarshal([]byte(doc), &probes); err != nil {
		t.Fatal(err)
	}
	_, err := app.Build(cfg, probes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "less than one run takes (3)") {
		t.Fatalf("err = %v, want the quota refused against three steps", err)
	}
}
