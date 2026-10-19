package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestStoredProbeRunsNothing(t *testing.T) {
	ledger := t.TempDir() + "/quota.json"
	a := quotaApp(t, ledger)
	ts := serve(t, a, nil)

	// Nothing has run yet: no series, and the quota is untouched.
	code, body := do(t, ts, http.MethodGet, "/probe?probe=paid&stored=true", "tok", "")
	if code != http.StatusOK || strings.Contains(body, "probe_up") {
		t.Fatalf("before a run: %d %q", code, body)
	}

	if code, _ := do(t, ts, http.MethodPost, "/api/probes?name=paid", "tok", ""); code != http.StatusOK {
		t.Fatalf("run: %d", code)
	}
	a.RefreshSelf() // the node's own series carry the probe's name as a label
	for i := 0; i < 5; i++ {
		code, body = do(t, ts, http.MethodGet, "/probe?probe=paid&stored=true", "tok", "")
		if code != http.StatusOK {
			t.Fatalf("stored: %d %s", code, body)
		}
	}
	for _, want := range []string{`probe_up{`, `probe_success{`, "# INFO stored last_run="} {
		if !strings.Contains(body, want) {
			t.Errorf("stored exposition lacks %s:\n%s", want, body)
		}
	}
	// Lifetime counters are not a run's result, and the agent's own series are
	// not the probe's.
	if strings.Contains(body, "probe_total") || strings.Contains(body, "argus_") {
		t.Errorf("stored exposition carries counters:\n%s", body)
	}
	// Five reads and the quota of two still has one left: none of them ran.
	_, status := do(t, ts, http.MethodGet, "/status.json", "", "")
	if !strings.Contains(status, `"used": 1,`) {
		t.Fatalf("stored reads spent the quota: %s", status)
	}
}

func TestStoredProbeRefusesAnotherTarget(t *testing.T) {
	ts := serve(t, quotaApp(t, ""), nil)
	if code, _ := do(t, ts, http.MethodGet, "/probe?probe=paid&stored=true&target=1.2.3.4", "tok", ""); code != http.StatusBadRequest {
		t.Fatalf("stored with target=: %d, want 400", code)
	}
	if code, _ := do(t, ts, http.MethodGet, "/probe?probe=absent&stored=true", "tok", ""); code != http.StatusNotFound {
		t.Fatalf("stored of an absent probe: %d, want 404", code)
	}
}

func TestStoredProbeNamesTheFailure(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)
	// Replace the configuration with a probe that fails, and run it once.
	if code, body := do(t, ts, http.MethodPut, "/api/config", "tok",
		"probes:\n  - {name: bad, type: broken, targets: [\"1.1.1.1\"], broken: {}}\n"); code != http.StatusOK {
		t.Fatalf("config: %d %s", code, body)
	}
	do(t, ts, http.MethodPost, "/api/probes?name=bad", "tok", "")
	_, body := do(t, ts, http.MethodGet, "/probe?probe=bad&stored=true", "tok", "")
	if !strings.Contains(body, "# WARN backend is down target=1.1.1.1 backend=1.1.1.1 reason=content err=") ||
		!strings.Contains(body, "maintenance") {
		t.Fatalf("the failure is not named:\n%s", body)
	}
}

func TestStoredFlagIsReadStrictly(t *testing.T) {
	ts := serve(t, quotaApp(t, t.TempDir()+"/quota.json"), nil)
	// "1" is as good as "true": a read, no run.
	code, body := do(t, ts, http.MethodGet, "/probe?probe=paid&stored=1", "tok", "")
	if code != http.StatusOK || strings.Contains(body, "probe_up") {
		t.Fatalf("stored=1: %d %q", code, body)
	}
	// Anything unclear is refused rather than read as "no" - that would run the
	// probe and spend the quota stored mode is there to keep.
	if code, _ := do(t, ts, http.MethodGet, "/probe?probe=paid&stored=yes", "tok", ""); code != http.StatusBadRequest {
		t.Fatalf("stored=yes: %d, want 400", code)
	}
	_, status := do(t, ts, http.MethodGet, "/status.json", "", "")
	if strings.Contains(status, `"used": 1,`) {
		t.Fatalf("a stored read ran the probe: %s", status)
	}
}

func TestStoredProbeKeepsTheBlackboxNames(t *testing.T) {
	a := quotaApp(t, "")
	a.Cfg.HTTP.API.ProbeAliases = true
	ts := serve(t, a, nil)
	do(t, ts, http.MethodPost, "/api/probes?name=paid", "tok", "")
	_, live := do(t, ts, http.MethodGet, "/probe?probe=paid", "tok", "")
	_, stored := do(t, ts, http.MethodGet, "/probe?probe=paid&stored=true", "tok", "")
	// A scrape moved from on-demand to stored keeps every blackbox series it had.
	seen := 0
	for _, line := range strings.Split(live, "\n") {
		name, _, _ := strings.Cut(line, "{")
		if !blackboxName(name) {
			continue
		}
		seen++
		if !strings.Contains(stored, name+"{") {
			t.Errorf("stored lacks %s", name)
		}
	}
	if seen == 0 {
		t.Fatalf("the on-demand run has no blackbox series to compare:\n%s", live)
	}
}

// blackboxName reports whether a series is one of blackbox_exporter's.
func blackboxName(name string) bool {
	for _, n := range []string{"probe_failed_due_to_regex", "probe_ip_protocol", "probe_http_status_code",
		"probe_duration_seconds", "probe_http_version"} {
		if name == n {
			return true
		}
	}
	return false
}
