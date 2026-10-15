package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// videoService is the admin of a video platform, as much of it as a scenario
// walks through: a session, a list, an upload that converts in the background,
// a player, comments.
type videoService struct {
	t        *testing.T
	password string
	// readyAfter is how many status polls a video spends converting.
	readyAfter int
	// fail turns the conversion into a failure.
	fail bool

	mu       sync.Mutex
	videos   map[string]*video
	deleted  []string
	comments []string
	uploads  []upload
	nextID   int
}

type video struct{ polls int }

type upload struct {
	field, filename, contentType string
	data                         []byte
	contentLength                int64
	chunked                      bool
}

func newVideoService(t *testing.T) (*videoService, *httptest.Server) {
	vs := &videoService{t: t, password: "s3cret-pass", readyAfter: 2, videos: map[string]*video{}}
	srv := httptest.NewServer(vs.handler())
	t.Cleanup(srv.Close)
	return vs, srv
}

func (vs *videoService) handler() http.Handler {
	mux := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie("session"); err != nil || c.Value != "ok" {
				http.Error(w, `{"error":"login first"}`, http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Login, Password string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password != vs.password {
			// Echoes what it was sent, as a careless service would.
			http.Error(w, "bad password "+body.Password, http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "ok", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/videos", authed(func(w http.ResponseWriter, _ *http.Request) {
		vs.mu.Lock()
		defer vs.mu.Unlock()
		_, _ = io.WriteString(w, `{"total": `+strconv.Itoa(len(vs.videos))+`}`)
	}))
	mux.HandleFunc("POST /api/videos", authed(func(w http.ResponseWriter, r *http.Request) {
		up := upload{contentLength: r.ContentLength, chunked: len(r.TransferEncoding) > 0}
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if part.FileName() != "" {
				up.field, up.filename, up.contentType = part.FormName(), part.FileName(), part.Header.Get("Content-Type")
				up.data, _ = io.ReadAll(part)
			}
		}
		vs.mu.Lock()
		vs.nextID++
		id := "v/" + strconv.Itoa(vs.nextID) // a slash, to keep the path honest
		vs.videos[id] = &video{}
		vs.uploads = append(vs.uploads, up)
		vs.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id": "`+id+`"}`)
	}))
	mux.HandleFunc("GET /api/videos/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		vs.mu.Lock()
		defer vs.mu.Unlock()
		v, ok := vs.videos[r.PathValue("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		v.polls++
		status := "processing"
		switch {
		case vs.fail && v.polls > 1:
			status = "failed"
		case !vs.fail && v.polls > vs.readyAfter:
			status = "ready"
		}
		_, _ = io.WriteString(w, `{"status": "`+status+`"}`)
	}))
	mux.HandleFunc("DELETE /api/videos/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		vs.mu.Lock()
		defer vs.mu.Unlock()
		delete(vs.videos, r.PathValue("id"))
		vs.deleted = append(vs.deleted, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /hls/{id}/master.m3u8", authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nlow.m3u8\n")
	}))
	mux.HandleFunc("POST /api/videos/{id}/comments", authed(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		vs.mu.Lock()
		vs.comments = append(vs.comments, body.Text)
		vs.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	return mux
}

// pipeline is the scenario of the video admin, with the fixture and the secret
// laid out in a temporary directory.
func pipeline(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "clip.bin")
	if err := os.WriteFile(fixture, bytes.Repeat([]byte("frame"), 2000), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "password")
	if err := os.WriteFile(secret, []byte("s3cret-pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return `
secrets:
  password: {file: ` + secret + `}
vars:
  login: argus-synthetic
steps:
  - name: login
    http:
      path: /api/login
      json: {login: "${var.login}", password: "${secret.password}"}
    expect: [{status_code: [200]}]
  - name: list
    http: {path: /api/videos}
    extract:
      videos: {json_path: total, metric: true}
  - name: upload
    http:
      path: /api/videos
      multipart:
        fields: {title: "argus ${run.id}"}
        files: {video: ` + fixture + `}
    expect: [{status_code: [201]}]
    extract:
      video_id: {json_path: id}
  - name: convert
    http: {path: "/api/videos/${var.video_id}"}
    until: [{json_path: status, json_equals: ready}]
    abort_if: [{json_path: status, json_equals: failed}]
    every: 10ms
    within: 5s
  - name: playback
    http: {path: "/hls/${var.video_id}/master.m3u8"}
    expect: [{status_code: [200], body_regex: "^#EXTM3U"}]
  - name: comment
    http:
      path: "/api/videos/${var.video_id}/comments"
      json: {text: "argus ${run.id}"}
    expect: [{status_code: [201]}]
cleanup:
  - name: delete
    http: {method: DELETE, path: "/api/videos/${var.video_id}"}
    expect: [{status_code: [204]}]
` + extra
}

func newProber(t *testing.T, options string) *Prober {
	t.Helper()
	p, err := build(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func build(options string) (*Prober, error) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		return nil, err
	}
	pc := config.Probe{Name: "test", Type: config.ScenarioType, Options: *node.Content[0]}
	p, err := New(pc)
	if err != nil {
		return nil, err
	}
	return p.(*Prober), nil
}

func requestFor(t *testing.T, srv *httptest.Server) probe.Request {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return probe.Request{Target: probe.Target{Name: "admin", Host: u.Hostname(), URL: u}}
}

func gauge(t *testing.T, rec *metrics.Recorder, name string, labels ...string) (float64, bool) {
	t.Helper()
	want := metrics.L(labels...)
	for _, s := range rec.Samples() {
		if s.Name != name {
			continue
		}
		match := true
		for _, l := range want {
			if s.Labels.Get(l.Name) != l.Value {
				match = false
			}
		}
		if match {
			g, ok := s.Value.(metrics.Gauge)
			if !ok {
				t.Fatalf("%s is not a gauge", name)
			}
			return float64(g), true
		}
	}
	return 0, false
}

func TestPipelineWalksEveryStepAndCleansUp(t *testing.T) {
	vs, srv := newVideoService(t)
	p := newProber(t, pipeline(t, ""))
	rec := metrics.NewRecorder(metrics.L("probe", "test", "target", "admin"))

	res := p.Probe(context.Background(), requestFor(t, srv), rec)
	if !res.OK() {
		t.Fatalf("the scenario failed: %v", res.Err)
	}

	// Every step is a phase, the wait included: that is the conversion time.
	var phases []string
	for _, ph := range res.Phases {
		phases = append(phases, ph.Name)
	}
	if got := strings.Join(phases, ","); got != "login,list,upload,convert,playback,comment" {
		t.Errorf("phases = %s", got)
	}
	for _, step := range []string{"login", "list", "upload", "convert", "playback", "comment", "delete"} {
		if v, ok := gauge(t, rec, SeriesStepSuccess, "step", step); !ok || v != 1 {
			t.Errorf("%s{step=%s} = %v, %v; want 1", SeriesStepSuccess, step, v, ok)
		}
	}
	if v, _ := gauge(t, rec, SeriesStepAttempts, "step", "convert"); v != 3 {
		t.Errorf("convert attempts = %v, want 3: two polls processing, then ready", v)
	}
	if _, ok := gauge(t, rec, SeriesStepAttempts, "step", "login"); ok {
		t.Error("a step that does not wait has no attempts series")
	}
	if v, ok := gauge(t, rec, SeriesValue, "step", "list", "name", "videos"); !ok || v != 0 {
		t.Errorf("scenario_value{name=videos} = %v, %v; want 0", v, ok)
	}
	if v, _ := gauge(t, rec, SeriesStepStatus, "step", "upload"); v != 201 {
		t.Errorf("upload status = %v, want 201", v)
	}
	if v, ok := gauge(t, rec, SeriesCleanup); !ok || v != 1 {
		t.Errorf("%s = %v, %v; want 1", SeriesCleanup, v, ok)
	}
	if res.Overhead <= 0 {
		t.Error("cleanup time must be kept out of probe_duration")
	}

	vs.mu.Lock()
	defer vs.mu.Unlock()
	if len(vs.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(vs.uploads))
	}
	up := vs.uploads[0]
	if up.chunked || up.contentLength <= int64(len(up.data)) {
		t.Errorf("upload went out chunked=%v with Content-Length %d: the length must be known up front",
			up.chunked, up.contentLength)
	}
	if !bytes.Equal(up.data, bytes.Repeat([]byte("frame"), 2000)) || up.field != "video" || up.filename != "clip.bin" {
		t.Errorf("upload = field %q, file %q, %d bytes", up.field, up.filename, len(up.data))
	}
	if len(vs.deleted) != 1 || vs.deleted[0] != "v/1" || len(vs.videos) != 0 {
		t.Errorf("deleted = %v, left = %d: cleanup must remove the upload", vs.deleted, len(vs.videos))
	}
	if len(vs.comments) != 1 || !strings.HasPrefix(vs.comments[0], "argus ") || len(vs.comments[0]) != len("argus ")+12 {
		t.Errorf("comments = %q: want the run id in it", vs.comments)
	}

	reports := p.Reports()
	if len(reports) != 1 || !reports[0].Success || reports[0].Target != "admin" {
		t.Fatalf("reports = %+v", reports)
	}
	if got := reports[0].Steps[3]; got.Name != "convert" || got.Status != StatusPassed || got.Attempts != 3 || got.Code != 200 {
		t.Errorf("convert report = %+v", got)
	}
	if got := reports[0].Cleanup; len(got) != 1 || got[0].Status != StatusPassed {
		t.Errorf("cleanup report = %+v", got)
	}
}

func TestAbortIfFailsTheStepAndCleanupStillRuns(t *testing.T) {
	vs, srv := newVideoService(t)
	vs.fail = true
	p := newProber(t, pipeline(t, ""))
	rec := metrics.NewRecorder(metrics.L("probe", "test", "target", "admin"))

	res := p.Probe(context.Background(), requestFor(t, srv), rec)
	if res.OK() {
		t.Fatal("a failed conversion must fail the scenario")
	}
	if probe.ReasonOf(res.Err) != probe.ReasonContent || !strings.Contains(res.Err.Error(), "step convert: abort_if") {
		t.Errorf("err = %v, want a content failure naming the step", res.Err)
	}
	if v, ok := gauge(t, rec, SeriesStepSuccess, "step", "convert"); !ok || v != 0 {
		t.Errorf("convert success = %v, %v; want 0", v, ok)
	}
	if _, ok := gauge(t, rec, SeriesStepSuccess, "step", "playback"); ok {
		t.Error("a step that never ran has no series")
	}
	rep := p.Reports()[0]
	if rep.Steps[4].Status != StatusNotRun || rep.Steps[5].Status != StatusNotRun {
		t.Errorf("steps after the failure = %+v", rep.Steps[4:])
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if len(vs.deleted) != 1 {
		t.Errorf("deleted = %v: cleanup runs however the steps ended", vs.deleted)
	}
}

func TestCleanupSkipsWhatNoStepGotAsFarAsCreating(t *testing.T) {
	vs, srv := newVideoService(t)
	vs.password = "rotated"
	p := newProber(t, pipeline(t, ""))
	rec := metrics.NewRecorder(metrics.L("probe", "test", "target", "admin"))

	res := p.Probe(context.Background(), requestFor(t, srv), rec)
	if res.OK() || probe.ReasonOf(res.Err) != probe.ReasonStatus {
		t.Fatalf("err = %v, want a status failure", res.Err)
	}
	rep := p.Reports()[0]
	if c := rep.Cleanup; len(c) != 1 || c[0].Status != StatusSkipped || !strings.Contains(c[0].Error, "${var.video_id}") {
		t.Errorf("cleanup = %+v, want skipped for want of video_id", c)
	}
	if v, ok := gauge(t, rec, SeriesCleanup); !ok || v != 1 {
		t.Errorf("a skipped cleanup step is no failed one: %s = %v, %v", SeriesCleanup, v, ok)
	}
}

func TestSecretsNeverReachTheError(t *testing.T) {
	vs, srv := newVideoService(t)
	vs.password = "something-else"
	options := strings.Replace(pipeline(t, ""), "expect: [{status_code: [200]}]",
		`expect: [{status_code: [200], body_regex: "^welcome"}]`, 1)
	p := newProber(t, options)
	rec := metrics.NewRecorder(metrics.L("probe", "test", "target", "admin"))

	res := p.Probe(context.Background(), requestFor(t, srv), rec)
	if res.OK() {
		t.Fatal("want a failure")
	}
	for _, text := range []string{res.Err.Error(), p.Reports()[0].Error, p.Reports()[0].Steps[0].Error} {
		if strings.Contains(text, "s3cret-pass") {
			t.Errorf("the secret leaked: %s", text)
		}
	}
}

func TestUntilGivesUpWithin(t *testing.T) {
	vs, srv := newVideoService(t)
	vs.readyAfter = 1 << 30
	options := strings.Replace(pipeline(t, ""), "within: 5s", "within: 50ms", 1)
	p := newProber(t, options)
	rec := metrics.NewRecorder(metrics.L("probe", "test", "target", "admin"))

	res := p.Probe(context.Background(), requestFor(t, srv), rec)
	if probe.ReasonOf(res.Err) != probe.ReasonTimeout || !strings.Contains(res.Err.Error(), "until did not hold within 50ms") {
		t.Errorf("err = %v, want a timeout naming within", res.Err)
	}
}

func TestCleanupOutlivesTheDeadline(t *testing.T) {
	vs, srv := newVideoService(t)
	vs.readyAfter = 1 << 30
	options := strings.Replace(pipeline(t, ""), "within: 5s", "within: 1h", 1)
	p := newProber(t, options)
	rec := metrics.NewRecorder(metrics.L("probe", "test", "target", "admin"))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res := p.Probe(ctx, requestFor(t, srv), rec)
	if probe.ReasonOf(res.Err) != probe.ReasonTimeout {
		t.Errorf("err = %v, want a timeout", res.Err)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if len(vs.deleted) != 1 {
		t.Errorf("deleted = %v: the steps' deadline is not the cleanup's", vs.deleted)
	}
}

func TestSecretIsReadOnEveryRun(t *testing.T) {
	_, srv := newVideoService(t)
	options := pipeline(t, "")
	p := newProber(t, options)
	secret := strings.TrimSpace(strings.SplitN(strings.SplitN(options, "{file: ", 2)[1], "}", 2)[0])
	if err := os.Remove(secret); err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
	if probe.ReasonOf(res.Err) != probe.ReasonInternal || !strings.Contains(res.Err.Error(), "secrets.password") {
		t.Errorf("err = %v, want the missing secret named", res.Err)
	}
	if rep := p.Reports()[0]; rep.Steps[0].Status != StatusNotRun || rep.Cleanup[0].Status != StatusNotRun {
		t.Errorf("report = %+v: nothing ran", rep)
	}
}

func TestRequestShapes(t *testing.T) {
	var (
		mu  sync.Mutex
		got []*http.Request
		raw []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r)
		raw = append(raw, string(body))
		mu.Unlock()
		switch r.URL.Path {
		case "/start":
			w.Header().Set("Location", "http://"+r.Host+"/upload/target?sig=a%2Bb")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	p := newProber(t, `
vars: {id: "a/b c"}
steps:
  - name: start
    http: {path: /start, follow_redirects: false}
    expect: [{status_code: [302]}]
    extract: {upload: {header: Location}}
  - name: put
    http:
      method: PUT
      url: "${var.upload}"
      body: "raw ${var.id}"
      headers: {X-Id: "${var.id}"}
  - name: segment
    http:
      path: "/items/${var.id}"
      query: {q: "${var.id}&x=1"}
      form: {name: "${var.id}"}
`)
	res := p.Probe(context.Background(), requestFor(t, srv), metrics.NewRecorder(nil))
	if !res.OK() {
		t.Fatal(res.Err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("requests = %d, want 3: a redirect not followed is the answer", len(got))
	}
	put := got[1]
	if put.Method != http.MethodPut || put.URL.RequestURI() != "/upload/target?sig=a%2Bb" {
		t.Errorf("put = %s %s: a whole-url reference is taken as it is", put.Method, put.URL.RequestURI())
	}
	if raw[1] != "raw a/b c" || put.Header.Get("X-Id") != "a/b c" {
		t.Errorf("body %q, header %q", raw[1], put.Header.Get("X-Id"))
	}
	seg := got[2]
	if seg.Method != http.MethodPost || seg.URL.EscapedPath() != "/items/a%2Fb%20c" {
		t.Errorf("segment = %s %s: a value in a path is one segment", seg.Method, seg.URL.EscapedPath())
	}
	if seg.URL.Query().Get("q") != "a/b c&x=1" || len(seg.URL.Query()) != 1 {
		t.Errorf("query = %q", seg.URL.RawQuery)
	}
	if seg.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || raw[2] != "name=a%2Fb+c" {
		t.Errorf("form = %q (%s)", raw[2], seg.Header.Get("Content-Type"))
	}
	if ua := seg.Header.Get("User-Agent"); !strings.HasPrefix(ua, "argus-scenario/") {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestConfigurationErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ options, want string }{
		"no steps": {`vars: {a: b}`, "at least one"},
		"unknown variable": {`
steps:
  - {name: a, http: {path: "/x/${var.id}"}}`, "${var.id} is not set"},
		"set by a later step": {`
steps:
  - {name: a, http: {path: "/x/${var.id}"}}
  - {name: b, http: {path: /y}, extract: {id: {json_path: id}}}`, "${var.id} is not set"},
		"undeclared secret": {`
steps:
  - {name: a, http: {path: /x, headers: {X: "${secret.token}"}}}`, "not declared under secrets"},
		"unset environment variable": {`
steps:
  - {name: a, http: {path: "/x/${API_TOKEN}"}}`, "environment variable is not set"},
		"unknown run field": {`
steps:
  - {name: a, http: {path: "/x/${run.started}"}}`, "only ${run.id}"},
		"path and url": {`
steps:
  - {name: a, http: {path: /x, url: "http://h/x"}}`, "one of them"},
		"path that leaves": {`
steps:
  - {name: a, http: {path: "http://elsewhere/x"}}`, "is not a path"},
		"url that is a path": {`
steps:
  - {name: a, http: {url: /x}}`, "not an http(s) URL"},
		"two bodies": {`
steps:
  - {name: a, http: {path: /x, body: b, form: {a: b}}}`, "set one of body"},
		"missing file": {`
steps:
  - {name: a, http: {path: /x, body_file: /nonexistent/file}}`, "no such file"},
		"file by reference": {`
vars: {f: x}
steps:
  - {name: a, http: {path: /x, multipart: {files: {f: "${var.f}"}}}}`, "not by a reference"},
		"wait without until": {`
steps:
  - {name: a, http: {path: /x}, every: 1s}`, "set until"},
		"duplicate step": {`
steps:
  - {name: a, http: {path: /x}}
cleanup:
  - {name: a, http: {path: /y}}`, "already taken"},
		"two extract sources": {`
steps:
  - {name: a, http: {path: /x}, extract: {v: {json_path: a, header: B}}}`, "set one of"},
		"bad step name": {`
steps:
  - {name: "a b", http: {path: /x}}`, "name: want"},
		"empty validator": {`
steps:
  - {name: a, http: {path: /x}, expect: [{name: nothing}]}`, "checks nothing"},
		"secret with both": {`
secrets: {t: {file: ` + file + `, env: T}}
steps:
  - {name: a, http: {path: /x}}`, "one of them"},
		"unknown field": {`
steps:
  - {name: a, http: {path: /x}, retries: 3}`, "retries"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := build(tc.options)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestCleanupMaySpeakOfAnyStepsVariable(t *testing.T) {
	_, err := build(`
steps:
  - {name: a, http: {path: /x}, extract: {id: {json_path: id}}}
cleanup:
  - {name: z, http: {method: DELETE, path: "/x/${var.id}"}}
  - {name: y, http: {method: DELETE, path: "/x/${var.typo}"}}`)
	if err == nil || !strings.Contains(err.Error(), "cleanup[1]") || !strings.Contains(err.Error(), "${var.typo}") {
		t.Errorf("err = %v: a variable no step sets is a typo, even in cleanup", err)
	}
}
