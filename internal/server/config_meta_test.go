package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
)

// statusOf reads /status.json the way the backend polling the node does.
func statusOf(t *testing.T, ts *httptest.Server) map[string]any {
	t.Helper()
	code, body := do(t, ts, http.MethodGet, "/status.json", "", "")
	if code != http.StatusOK {
		t.Fatalf("/status.json: %d %s", code, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return out
}

func TestStatusCarriesTheHashOfWhatWasPushed(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)

	before := statusOf(t, ts)
	if before["config_source"] != "none" {
		t.Fatalf("a node nobody pushed to reports %v, want none", before["config_source"])
	}
	if before["version"] == "" {
		t.Fatal("the health-gate of a deploy compares versions, and there is none")
	}

	pushed := `
probes:
  - {name: two, type: noop, targets: ["2.2.2.2"], noop: {}}
`
	code, body := do(t, ts, http.MethodPut, "/api/config", "tok", pushed)
	if code != http.StatusOK {
		t.Fatalf("push: %d %s", code, body)
	}

	after := statusOf(t, ts)
	if want := config.Hash([]byte(pushed)); after["config_hash"] != want {
		t.Fatalf("config_hash is %v, want the sha256 of the bytes sent (%s)", after["config_hash"], want)
	}
	if after["config_source"] != "api" {
		t.Fatalf("config_source is %v, want api", after["config_source"])
	}
	if after["config_applied_at"] == nil || after["config_applied_at"] == "" {
		t.Fatal("config_applied_at is missing")
	}
	// The push answer is a status of its own, and says the same thing.
	var answer map[string]any
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatal(err)
	}
	if answer["config_hash"] != after["config_hash"] {
		t.Fatalf("the answer to the push disagrees with /status.json: %v vs %v",
			answer["config_hash"], after["config_hash"])
	}
}

func TestARejectedPushLeavesTheHashAlone(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)

	good := "probes:\n  - {name: two, type: noop, targets: [\"2.2.2.2\"], noop: {}}\n"
	if code, body := do(t, ts, http.MethodPut, "/api/config", "tok", good); code != http.StatusOK {
		t.Fatalf("push: %d %s", code, body)
	}
	applied := statusOf(t, ts)

	if code, _ := do(t, ts, http.MethodPut, "/api/config", "tok",
		"probes:\n  - {name: broken, type: nosuchkind, targets: [\"3.3.3.3\"]}\n"); code != http.StatusBadRequest {
		t.Fatalf("a configuration the node cannot run must be refused, got %d", code)
	}

	after := statusOf(t, ts)
	if after["config_hash"] != applied["config_hash"] {
		t.Fatalf("a refused push moved the hash: %v -> %v", applied["config_hash"], after["config_hash"])
	}
}

func TestEmptyConfigurationIsAccepted(t *testing.T) {
	a := newApp(t)
	ts := serve(t, a, nil)

	// A node whose last domain was taken away is pushed an empty list; refusing
	// it would leave the node probing what it is no longer meant to probe.
	empty := "probes: []\n"
	code, body := do(t, ts, http.MethodPut, "/api/config", "tok", empty)
	if code != http.StatusOK {
		t.Fatalf("empty push: %d %s", code, body)
	}
	st := statusOf(t, ts)
	if probes, _ := st["probes"].([]any); len(probes) != 0 {
		t.Fatalf("the node kept %d probe(s) after being told to check nothing", len(probes))
	}
	if st["config_hash"] != config.Hash([]byte(empty)) {
		t.Fatalf("config_hash is %v, want the sha256 of the empty configuration", st["config_hash"])
	}
	if len(a.Runners()) != 0 {
		t.Fatalf("%d runner(s) survived an empty configuration", len(a.Runners()))
	}
}
