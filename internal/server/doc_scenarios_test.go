package server

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
)

// shop is a service a scenario logs in to and uploads a file to; it remembers
// what it was sent.
type shop struct {
	mu       sync.Mutex
	logins   int
	uploaded map[string]string // filename -> content
}

func newShop(t *testing.T) (*shop, *httptest.Server) {
	s := &shop{uploaded: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Password string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Password != "pushed-secret" {
			// Echoes it back, as a careless service would.
			http.Error(w, "wrong password "+body.Password, http.StatusForbidden)
			return
		}
		s.mu.Lock()
		s.logins++
		s.mu.Unlock()
		_, _ = io.WriteString(w, `{"token":"t-1"}`)
	})
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			if part.FileName() != "" {
				s.mu.Lock()
				s.uploaded[part.FileName()] = string(data)
				s.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

// docScenario is a scenario as the check document carries it: a secret and a
// file written in, nothing on the node's disk.
func docScenario(target, password string) string {
	return `
  - name: shop-upload
    interval: 1h
    timeout: 30s
    targets: [` + target + `]
    secrets:
      password: {value: ` + password + `}
    steps:
      - name: login
        post: /login
        json: {password: "${secret.password}"}
        status: 200
        extract: {token: {json_path: token, secret: true}}
      - name: upload
        post: /upload
        headers: {Authorization: "Bearer ${var.token}"}
        multipart:
          fields: {title: "run ${run.id}"}
          files:
            report: {filename: report.csv, text: "a,b\n1,2\n"}
            blob: {filename: blob.bin, base64: aGVsbG8=}
        status: 201
`
}

func TestScenariosArriveWithThePushedDocument(t *testing.T) {
	n := newScenarioNode(t)
	n.settle(t, 1)
	shop, target := newShop(t)

	doc := baseProbes + "scenarios:" + docScenario(target.URL, "pushed-secret")
	if code, body := do(t, n.ts, http.MethodPut, "/api/config", "tok", doc); code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if !n.app.IsScenario("shop-upload") || !n.app.IsScenario("pipeline") {
		t.Fatal("the pushed scenario should run beside the node's own file")
	}
	// It starts on its schedule, as a file's scenario does.
	st := waitScenario(t, n, "shop-upload")
	if !st.LastRuns[0].Success {
		t.Fatalf("run failed: %+v", st.LastRuns[0])
	}
	shop.mu.Lock()
	defer shop.mu.Unlock()
	if shop.uploaded["report.csv"] != "a,b\n1,2\n" || shop.uploaded["blob.bin"] != "hello" {
		t.Errorf("uploaded = %q", shop.uploaded)
	}

	// The read token sees the scenario, not the secret written into it.
	_, conf := do(t, n.ts, http.MethodGet, "/api/config", "ro", "")
	if strings.Contains(conf, "pushed-secret") || !strings.Contains(conf, "${secret.password}") {
		t.Errorf("read token's view of the document:\n%s", conf)
	}
	_, conf = do(t, n.ts, http.MethodGet, "/api/config", "tok", "")
	if !strings.Contains(conf, "pushed-secret") {
		t.Error("the write token gets the document as written")
	}

	// The next push without it takes it away; the file's scenario stays.
	if code, body := do(t, n.ts, http.MethodPut, "/api/config", "tok", baseProbes); code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if n.app.IsScenario("shop-upload") || !n.app.IsScenario("pipeline") {
		t.Error("a document scenario should leave with the document, and only it")
	}
}

func TestPushedScenarioIsRefusedWhatAFileWouldBe(t *testing.T) {
	n := newScenarioNode(t)
	for name, tc := range map[string]struct{ doc, want string }{
		"same name as the file": {
			"scenarios:\n  - {name: pipeline, interval: 1h, timeout: 10s, targets: [http://x], steps: [{name: a, get: /}]}\n",
			"a scenario file of that name"},
		"relative file": {
			"scenarios:\n  - {name: up, interval: 1h, timeout: 10s, targets: [http://x], steps: [{name: a, post: /, body_file: data.bin}]}\n",
			"no directory of its own"},
		"no name": {
			"scenarios:\n  - {interval: 1h, timeout: 10s, targets: [http://x], steps: [{name: a, get: /}]}\n",
			"name is missing"},
		"two secret sources": {
			"scenarios:\n  - {name: s, interval: 1h, timeout: 10s, targets: [http://x], secrets: {p: {value: a, env: B}}, steps: [{name: a, get: /}]}\n",
			"one of them"},
		"inline file without a name": {
			"scenarios:\n  - {name: s, interval: 1h, timeout: 10s, targets: [http://x], steps: [{name: a, post: /, multipart: {files: {f: {text: x}}}}]}\n",
			"filename"},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := do(t, n.ts, http.MethodPut, "/api/config", "tok", baseProbes+tc.doc)
			if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
				t.Errorf("PUT: %d %s, want 400 mentioning %q", code, body, tc.want)
			}
		})
	}
}

func TestScenarioTrial(t *testing.T) {
	n := newScenarioNode(t)
	shop, target := newShop(t)
	scenario := strings.ReplaceAll(strings.TrimPrefix(docScenario(target.URL, "pushed-secret"), "\n  - "), "\n    ", "\n")

	if code, _ := do(t, n.ts, http.MethodPost, "/api/scenario-trials", "ro", scenario); code != http.StatusUnauthorized {
		t.Errorf("a read token started a trial: %d", code)
	}

	// A dry run builds it and runs nothing.
	code, body := do(t, n.ts, http.MethodPost, "/api/scenario-trials?dry_run=true", "tok", scenario)
	if code != http.StatusOK {
		t.Fatalf("dry run: %d %s", code, body)
	}
	var dry app.Trial
	if err := json.Unmarshal([]byte(body), &dry); err != nil || strings.Join(dry.Steps, ",") != "login,upload" {
		t.Fatalf("dry run: %s (%v)", body, err)
	}
	if shop.logins != 0 {
		t.Fatal("a dry run logged in")
	}
	if code, body := do(t, n.ts, http.MethodPost, "/api/scenario-trials?dry_run=true", "tok",
		strings.Replace(scenario, "${var.token}", "${var.tokn}", 1)); code != http.StatusBadRequest || !strings.Contains(body, "tokn") {
		t.Errorf("a typo in a reference: %d %s", code, body)
	}

	// A trial runs once, in the background, and keeps its report.
	code, body = do(t, n.ts, http.MethodPost, "/api/scenario-trials", "tok",
		strings.Replace(scenario, "pushed-secret", "wrong-secret", 1))
	if code != http.StatusAccepted {
		t.Fatalf("trial: %d %s", code, body)
	}
	var started app.Trial
	_ = json.Unmarshal([]byte(body), &started)
	var tr app.Trial
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body = do(t, n.ts, http.MethodGet, "/api/scenario-trials/"+started.ID, "ro", "")
		_ = json.Unmarshal([]byte(body), &tr)
		if !tr.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if tr.Running || len(tr.Reports) != 1 {
		t.Fatalf("trial = %s", body)
	}
	rep := tr.Reports[0]
	if rep.Success || rep.Steps[0].Status != "failed" || rep.Steps[0].Code != http.StatusForbidden {
		t.Errorf("report = %+v", rep)
	}
	// The service echoed the password; the report does not.
	if strings.Contains(body, "wrong-secret") {
		t.Errorf("the secret is in the report: %s", body)
	}
	if n.app.IsScenario("shop-upload") {
		t.Error("a trial configured the scenario")
	}
	if code, _ := do(t, n.ts, http.MethodGet, "/api/scenario-trials/nope", "ro", ""); code != http.StatusNotFound {
		t.Errorf("unknown trial: %d", code)
	}
}

func waitScenario(t *testing.T, n *scenarioNode, name string) app.ScenarioStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, body := do(t, n.ts, http.MethodGet, "/api/scenarios/"+name, "ro", "")
		var st app.ScenarioStatus
		if code == http.StatusOK && json.Unmarshal([]byte(body), &st) == nil && !st.Running && len(st.LastRuns) == 1 {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never finished a run", name)
	return app.ScenarioStatus{}
}
