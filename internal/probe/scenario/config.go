package scenario

import (
	"fmt"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	httpprobe "github.com/tonyamdfrost-cmd/Argus/internal/probe/http"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

// Config is a scenario file less what it shares with every probe.
type Config struct {
	// Vars are values the steps start with.
	Vars map[string]string `yaml:"vars"`
	// Secrets are read on every run, so a rotation needs no reload, and are
	// replaced by their reference in every error and report.
	Secrets map[string]Secret `yaml:"secrets"`
	TLS     tlsinfo.Options   `yaml:"tls"`
	Steps   []Step            `yaml:"steps"`
	// Cleanup runs after the steps however they ended, with a budget of its
	// own: the time the steps ran out of is not a reason to leave their
	// leftovers on the target.
	Cleanup        []Step          `yaml:"cleanup"`
	CleanupTimeout config.Duration `yaml:"cleanup_timeout"`
}

// Secret names where a secret is read from: a file or an environment variable.
type Secret struct {
	File string `yaml:"file"`
	Env  string `yaml:"env"`
}

// Step is one request, the checks its answer must pass and the values taken
// from it for the steps after.
type Step struct {
	Name    string                `yaml:"name"`
	HTTP    Request               `yaml:"http"`
	Expect  []httpprobe.Validator `yaml:"expect"`
	Extract map[string]Extract    `yaml:"extract"`
	// Until repeats the request every Every until all of these hold, for at
	// most Within. Expect still applies to every answer.
	Until  []httpprobe.Validator `yaml:"until"`
	Every  config.Duration       `yaml:"every"`
	Within config.Duration       `yaml:"within"`
	// AbortIf ends the wait early: an answer any of these holds for is a failure.
	AbortIf []httpprobe.Validator `yaml:"abort_if"`
	// Timeout bounds one request of the step.
	Timeout config.Duration `yaml:"timeout"`
}

// Request is the `http:` block of a step. Path is taken against the target,
// URL is anywhere: a presigned upload address an earlier step was handed.
type Request struct {
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	URL     string            `yaml:"url"`
	Query   map[string]string `yaml:"query"`
	Headers map[string]string `yaml:"headers"`

	// At most one body. A file is streamed from disk, never held in memory.
	Body      string            `yaml:"body"`
	BodyFile  string            `yaml:"body_file"`
	JSON      any               `yaml:"json"`
	Form      map[string]string `yaml:"form"`
	Multipart *Multipart        `yaml:"multipart"`

	FollowRedirects *bool `yaml:"follow_redirects"`
	MaxBodyBytes    int64 `yaml:"max_body_bytes"`
}

// Multipart is a multipart/form-data body: text fields and files from disk.
type Multipart struct {
	Fields map[string]string `yaml:"fields"`
	Files  map[string]string `yaml:"files"`
}

// Extract takes one value out of an answer, from exactly one place.
type Extract struct {
	JSONPath string `yaml:"json_path"`
	Header   string `yaml:"header"`
	// Regex runs on the body; the first group is the value, or the whole match
	// when there is none.
	Regex string `yaml:"regex"`
	// Metric publishes the value as scenario_value, which then has to be a number.
	Metric bool `yaml:"metric"`
}

// anchor takes the files a scenario names relative to its own file: fixtures
// and secrets live beside the scenario, not wherever the node was started.
func (c *Config) anchor(dir string) {
	if dir == "" {
		return
	}
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	for name, sec := range c.Secrets {
		sec.File = abs(sec.File)
		c.Secrets[name] = sec
	}
	c.TLS.CACert, c.TLS.ClientCert, c.TLS.ClientKey = abs(c.TLS.CACert), abs(c.TLS.ClientCert), abs(c.TLS.ClientKey)
	for _, steps := range [][]Step{c.Steps, c.Cleanup} {
		for i := range steps {
			r := &steps[i].HTTP
			r.BodyFile = abs(r.BodyFile)
			if r.Multipart != nil {
				for field, p := range r.Multipart.Files {
					r.Multipart.Files[field] = abs(p)
				}
			}
		}
	}
}

// fits checks the scenario against the probe's timing: a wait longer than the
// run may last can never end in success, and a cleanup that outlasts the
// interval runs into the next run.
func (c *compiled) fits(interval, timeout time.Duration) error {
	for _, st := range c.steps {
		if timeout > 0 && st.within > timeout {
			return fmt.Errorf("steps %s: within (%s) exceeds the scenario's timeout (%s)", st.name, st.within, timeout)
		}
	}
	for _, st := range c.cleanup {
		if st.within > c.cleanupTimeout {
			return fmt.Errorf("cleanup %s: within (%s) exceeds cleanup_timeout (%s)", st.name, st.within, c.cleanupTimeout)
		}
	}
	if len(c.cleanup) > 0 && interval > 0 && timeout+c.cleanupTimeout > interval {
		return fmt.Errorf("timeout (%s) + cleanup_timeout (%s) exceeds interval (%s): the next run would start "+
			"while this one is still cleaning up", timeout, c.cleanupTimeout, interval)
	}
	return nil
}

const (
	defaultCleanupTimeout = 30 * time.Second
	defaultEvery          = 5 * time.Second
	defaultMaxBody        = 1 << 20
)

// compiled is a scenario ready to run: every template parsed, every pattern
// compiled, every reference checked against what is defined where it is used.
type compiled struct {
	vars           map[string]string
	secrets        map[string]Secret
	steps          []*step
	cleanup        []*step
	cleanupTimeout time.Duration
}

type step struct {
	name    string
	cleanup bool
	req     request
	expect  []httpprobe.Validator
	until   []httpprobe.Validator
	abortIf []httpprobe.Validator
	extract []extractor
	every   time.Duration
	within  time.Duration
	timeout time.Duration
	// sizeLimit is how far an answer is read: what is kept, and on to the
	// largest size bound a validator sets.
	sizeLimit int64
}

type request struct {
	method   string
	path     *template
	url      *template
	query    []pair
	headers  []pair
	body     *template
	bodyFile string
	json     any // the document, with a template in place of every string
	form     []pair
	fields   []pair
	files    []filePart
	follow   bool
	maxBody  int64
}

type pair struct {
	key   string
	value template
}

type filePart struct{ field, path, contentType string }

type extractor struct {
	name     string
	jsonPath string
	header   string
	regex    *regexp.Regexp
	metric   bool
}

func compile(cfg Config) (*compiled, error) {
	c := &compiled{
		vars:           cfg.Vars,
		secrets:        cfg.Secrets,
		cleanupTimeout: cfg.CleanupTimeout.D(),
	}
	if c.cleanupTimeout <= 0 {
		c.cleanupTimeout = defaultCleanupTimeout
	}
	if len(cfg.Steps) == 0 {
		return nil, fmt.Errorf("steps: a scenario needs at least one")
	}

	defs := definitions{vars: map[string]bool{}, secrets: map[string]bool{}}
	for _, name := range slices.Sorted(maps.Keys(cfg.Vars)) {
		if !identifier.MatchString(name) {
			return nil, fmt.Errorf("vars: %q is not a name", name)
		}
		t, err := parseTemplate(cfg.Vars[name])
		if err != nil {
			return nil, fmt.Errorf("vars.%s: %w", name, err)
		}
		if len(t.refs) > 0 {
			return nil, fmt.Errorf("vars.%s: a var is a plain value; put %s where the value is used", name, t.refs[0])
		}
		defs.vars[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Secrets)) {
		s := cfg.Secrets[name]
		if !identifier.MatchString(name) {
			return nil, fmt.Errorf("secrets: %q is not a name", name)
		}
		if (s.File == "") == (s.Env == "") {
			return nil, fmt.Errorf("secrets.%s: set file or env, one of them", name)
		}
		defs.secrets[name] = true
	}

	names := map[string]bool{}
	for i, sc := range cfg.Steps {
		st, err := compileStep(sc, defs, false)
		if err != nil {
			return nil, fmt.Errorf("steps[%d]: %w", i, err)
		}
		if names[st.name] {
			return nil, fmt.Errorf("steps[%d]: the name %q is already taken", i, st.name)
		}
		names[st.name] = true
		// Set by this step, so usable from the next one on.
		for _, x := range st.extract {
			defs.vars[x.name] = true
		}
		c.steps = append(c.steps, st)
	}
	// Cleanup sees everything a step could have set; what one did not get as
	// far as setting skips the cleanup step that needs it.
	for i, sc := range cfg.Cleanup {
		st, err := compileStep(sc, defs, true)
		if err != nil {
			return nil, fmt.Errorf("cleanup[%d]: %w", i, err)
		}
		if names[st.name] {
			return nil, fmt.Errorf("cleanup[%d]: the name %q is already taken", i, st.name)
		}
		names[st.name] = true
		for _, x := range st.extract {
			defs.vars[x.name] = true
		}
		c.cleanup = append(c.cleanup, st)
	}
	return c, nil
}

// stepName is a label value and a phase: kept to what reads well in both.
var stepName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func compileStep(sc Step, defs definitions, cleanup bool) (*step, error) {
	if !stepName.MatchString(sc.Name) {
		return nil, fmt.Errorf("name: want letters, digits, _ . -, got %q", sc.Name)
	}
	st := &step{
		name:    sc.Name,
		cleanup: cleanup,
		every:   sc.Every.D(),
		within:  sc.Within.D(),
		timeout: sc.Timeout.D(),
	}
	wrap := func(err error) error { return fmt.Errorf("%s: %w", sc.Name, err) }

	req, err := compileRequest(sc.HTTP, defs)
	if err != nil {
		return nil, wrap(fmt.Errorf("http.%w", err))
	}
	st.req = req

	expect := sc.Expect
	if len(expect) == 0 {
		expect = []httpprobe.Validator{httpprobe.DefaultValidator()}
	}
	if st.expect, err = validators("expect", expect); err != nil {
		return nil, wrap(err)
	}
	if st.until, err = validators("until", sc.Until); err != nil {
		return nil, wrap(err)
	}
	if st.abortIf, err = validators("abort_if", sc.AbortIf); err != nil {
		return nil, wrap(err)
	}
	// Counted past what is kept, or a size bound above it could never decide.
	st.sizeLimit = 2 * st.req.maxBody
	for _, v := range slices.Concat(st.expect, st.until, st.abortIf) {
		st.sizeLimit = max(st.sizeLimit, v.MaxSizeBytes+1, v.MinSizeBytes)
	}
	if len(st.until) == 0 && (len(st.abortIf) > 0 || st.every != 0 || st.within != 0) {
		return nil, wrap(fmt.Errorf("every, within and abort_if belong to a step that waits: set until"))
	}
	if st.every < 0 || st.within < 0 || st.timeout < 0 {
		return nil, wrap(fmt.Errorf("every, within and timeout cannot be negative"))
	}
	if len(st.until) > 0 && st.every == 0 {
		st.every = defaultEvery
	}

	for _, name := range slices.Sorted(maps.Keys(sc.Extract)) {
		x, err := compileExtract(name, sc.Extract[name])
		if err != nil {
			return nil, wrap(fmt.Errorf("extract.%s: %w", name, err))
		}
		st.extract = append(st.extract, x)
	}
	return st, nil
}

// validators copies and compiles a list, naming the unnamed after the list.
func validators(field string, in []httpprobe.Validator) ([]httpprobe.Validator, error) {
	out := slices.Clone(in)
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = field
		}
		if err := out[i].Compile(); err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", field, i, err)
		}
	}
	return out, nil
}

func compileExtract(name string, e Extract) (extractor, error) {
	x := extractor{name: name, jsonPath: e.JSONPath, header: e.Header, metric: e.Metric}
	if !identifier.MatchString(name) {
		return x, fmt.Errorf("%q is not a name", name)
	}
	set := 0
	for _, on := range []bool{e.JSONPath != "", e.Header != "", e.Regex != ""} {
		if on {
			set++
		}
	}
	if set != 1 {
		return x, fmt.Errorf("set one of json_path, header, regex")
	}
	if e.Regex != "" {
		re, err := regexp.Compile(e.Regex)
		if err != nil {
			return x, fmt.Errorf("regex: %w", err)
		}
		if re.NumSubexp() > 1 {
			return x, fmt.Errorf("regex: one group at most, the value")
		}
		x.regex = re
	}
	return x, nil
}

func compileRequest(r Request, defs definitions) (request, error) {
	out := request{
		method:  strings.ToUpper(r.Method),
		follow:  r.FollowRedirects == nil || *r.FollowRedirects,
		maxBody: r.MaxBodyBytes,
	}
	if out.maxBody <= 0 {
		out.maxBody = defaultMaxBody
	}

	var err error
	switch {
	case (r.Path == "") == (r.URL == ""):
		return out, fmt.Errorf("path: set path (on the target) or url (anywhere), one of them")
	case r.Path != "":
		if out.path, err = checkedTemplate("path", r.Path, defs); err != nil {
			return out, err
		}
		if err := checkURL(r.Path, false); err != nil {
			return out, fmt.Errorf("path: %w", err)
		}
		if err := out.path.inPath(r.Path, false); err != nil {
			return out, fmt.Errorf("path: %w", err)
		}
	default:
		if out.url, err = checkedTemplate("url", r.URL, defs); err != nil {
			return out, err
		}
		// A url that is one reference is taken whole when the run has it;
		// query: would re-encode its query, and a signed one with it.
		if out.url.whole() && len(r.Query) > 0 {
			return out, fmt.Errorf("query: a url taken whole keeps its own query, signature included")
		}
		if !out.url.whole() {
			if err := checkURL(r.URL, true); err != nil {
				return out, fmt.Errorf("url: %w", err)
			}
			if err := out.url.inPath(r.URL, true); err != nil {
				return out, fmt.Errorf("url: %w", err)
			}
		}
	}
	if out.query, err = pairs("query", r.Query, defs); err != nil {
		return out, err
	}
	if out.headers, err = pairs("headers", r.Headers, defs); err != nil {
		return out, err
	}

	bodies := 0
	for _, on := range []bool{r.Body != "", r.BodyFile != "", r.JSON != nil, len(r.Form) > 0, r.Multipart != nil} {
		if on {
			bodies++
		}
	}
	if bodies > 1 {
		return out, fmt.Errorf("body: set one of body, body_file, json, form, multipart")
	}
	switch {
	case r.Body != "":
		if out.body, err = checkedTemplate("body", r.Body, defs); err != nil {
			return out, err
		}
	case r.BodyFile != "":
		if err := readable(r.BodyFile); err != nil {
			return out, fmt.Errorf("body_file: %w", err)
		}
		out.bodyFile = r.BodyFile
	case r.JSON != nil:
		if out.json, err = compileJSON(r.JSON, defs); err != nil {
			return out, fmt.Errorf("json: %w", err)
		}
	case len(r.Form) > 0:
		if out.form, err = pairs("form", r.Form, defs); err != nil {
			return out, err
		}
	case r.Multipart != nil:
		if len(r.Multipart.Fields) == 0 && len(r.Multipart.Files) == 0 {
			return out, fmt.Errorf("multipart: set fields, files or both")
		}
		if out.fields, err = pairs("multipart.fields", r.Multipart.Fields, defs); err != nil {
			return out, err
		}
		for _, field := range slices.Sorted(maps.Keys(r.Multipart.Files)) {
			path := r.Multipart.Files[field]
			if err := readable(path); err != nil {
				return out, fmt.Errorf("multipart.files.%s: %w", field, err)
			}
			out.files = append(out.files, filePart{field, path, contentType(path)})
		}
	}
	if out.method == "" {
		// As curl does: a body means it is meant to be sent.
		out.method = http.MethodGet
		if bodies > 0 {
			out.method = http.MethodPost
		}
	}
	return out, nil
}

func checkedTemplate(field, s string, defs definitions) (*template, error) {
	t, err := parseTemplate(s)
	if err == nil {
		err = defs.check(t)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return &t, nil
}

func pairs(field string, m map[string]string, defs definitions) ([]pair, error) {
	out := make([]pair, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		t, err := checkedTemplate(field+"."+k, m[k], defs)
		if err != nil {
			return nil, err
		}
		out = append(out, pair{k, *t})
	}
	return out, nil
}

// compileJSON swaps every string of a decoded document for its template.
func compileJSON(v any, defs definitions) (any, error) {
	switch t := v.(type) {
	case string:
		return checkedTemplate("value", t, defs)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			c, err := compileJSON(item, defs)
			if err != nil {
				return nil, fmt.Errorf("%s.%w", k, err)
			}
			out[k] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			c, err := compileJSON(item, defs)
			if err != nil {
				return nil, fmt.Errorf("[%d].%w", i, err)
			}
			out[i] = c
		}
		return out, nil
	case map[any]any:
		return nil, fmt.Errorf("object keys must be strings")
	}
	return v, nil
}

// checkURL parses a URL with every reference standing in as a plain word, which
// is what the shape has to survive: a path stays on the target, a url leaves it.
func checkURL(s string, absolute bool) error {
	u, err := url.Parse(refPattern.ReplaceAllString(s, "x"))
	if err != nil {
		return err
	}
	switch {
	case absolute && (u.Scheme != "http" && u.Scheme != "https" || u.Host == ""):
		return fmt.Errorf("%q is not an http(s) URL; a path on the target goes in path", s)
	case !absolute && (u.Scheme != "" || u.Host != ""):
		return fmt.Errorf("%q is not a path; an address elsewhere goes in url", s)
	}
	return nil
}

// readable checks a file the scenario sends when it is built, not minutes into
// a run that has already changed things on the target.
func readable(path string) error {
	if refPattern.MatchString(path) {
		return fmt.Errorf("%q: a file is named in the configuration, not by a reference", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}

func contentType(path string) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t
	}
	return "application/octet-stream"
}
