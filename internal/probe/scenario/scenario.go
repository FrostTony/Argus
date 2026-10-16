// Package scenario is the multi-step prober: a sequence of HTTP requests that
// acts on the target the way a user would — log in, upload, wait for the
// result, use it — carrying cookies and values from one step to the next.
//
// A single request proves that an endpoint answers. A pipeline behind it
// (a queue, the workers, the storage) can be stopped while every endpoint
// still answers 200, and only walking through it shows that.
package scenario

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func init() { probe.Register(config.ScenarioType, New) }

// The series a scenario adds to those every probe has. Each step's duration
// is a phase, probe_phase_*{phase="<step>"}, so a slow step is read where a
// slow TLS handshake is.
const (
	SeriesStepSuccess  = "scenario_step_success"
	SeriesStepStatus   = "scenario_step_status_code"
	SeriesStepAttempts = "scenario_step_attempts"
	SeriesValue        = "scenario_value"
	SeriesCleanup      = "scenario_cleanup_success"
	SeriesStepFailures = "scenario_step_failures_total"
)

// Prober runs one scenario. It keeps the last run of every target, which the
// API serves: a scenario fails at a step, and the step is the first question.
type Prober struct {
	sc    *compiled
	tls   *tls.Config
	files []string

	mu   sync.Mutex
	last map[string]Report
	// heir is the prober a reload replaced this one with. A run still going
	// when that happened ends here, and its report belongs to the heir.
	heir *Prober
}

// maxReports bounds what is kept of targets that discovery has since dropped.
const maxReports = 256

func New(pc config.Probe) (probe.Prober, error) {
	var cfg Config
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if pc.Dir == "" {
		if rel := cfg.relative(); rel != "" {
			return nil, fmt.Errorf("%s: a scenario sent in the check document has no directory of its own; "+
				"name the file by an absolute path, or write the content in", rel)
		}
	}
	cfg.anchor(pc.Dir)
	sc, err := compile(cfg)
	if err != nil {
		return nil, err
	}
	if err := sc.fits(pc.Interval.D(), pc.Timeout.D()); err != nil {
		return nil, err
	}
	tc, err := tlsinfo.Build(cfg.TLS)
	if err != nil {
		return nil, err
	}
	return &Prober{sc: sc, tls: tc, files: cfg.TLS.Files(), last: map[string]Report{}}, nil
}

// SelfAddressed: a session is one client, and spreading it over a target's
// addresses would log in on one and upload on another — or upload once per
// address. The steps resolve names as any client does.
func (*Prober) SelfAddressed() bool { return true }

// Files implements probe.FileBacked, for the TLS material read when the
// prober is built. Request files and secrets are read on every run instead.
func (p *Prober) Files() []string { return slices.Clone(p.files) }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	r := &run{
		sc:     p.sc,
		rec:    rec,
		report: Report{Target: req.Target.Name, Started: time.Now()},
	}
	var res probe.Result
	defer func() {
		// The runner turns a panic into a failure; the report must not say
		// otherwise.
		if v := recover(); v != nil {
			p.keep(r.finish(r.redact(probe.Fail(probe.ReasonInternal, "prober panicked: %v", v))))
			panic(v)
		}
		p.keep(r.finish(res.Err))
	}()

	base, err := req.Target.HTTPURL()
	if err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "%v", err)
		return res
	}
	if r.scope, err = p.sc.scope(); err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "%v", err)
		return res
	}
	r.session = p.session(req)
	defer r.session.close()

	res.Err = r.steps(ctx, base, &res)

	// The steps' deadline is not the cleanup's: leftovers are removed even
	// from a run that timed out or was cancelled by a reload. Cleanup time is
	// not the target's, so it stays out of probe_duration.
	start := time.Now()
	r.cleanup(ctx, base)
	res.Overhead += time.Since(start)

	res.Err = r.redact(res.Err)
	return res
}

// maxRedirects is net/http's own limit, which a CheckRedirect replaces.
const maxRedirects = 10

// confine follows a redirect, keeping what a step wrote for one origin off
// another: net/http drops only Authorization and Cookie on a hop to another
// host, carries every other header — an API key among them — and sends a
// 307's body again. The jar's cookies are added after this, per host.
func confine(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if sameOrigin(req.URL, via[0].URL) {
		return nil
	}
	if req.Body != nil && req.Body != http.NoBody {
		return fmt.Errorf("a redirect to %s://%s would send the step's body to another origin", req.URL.Scheme, req.URL.Host)
	}
	req.Header = http.Header{"User-Agent": {userAgent}}
	return nil
}

// sameOrigin compares scheme, host and port: an https origin is not its http
// counterpart, which anyone on the path can read.
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// session is what one run sends all of its steps through: one cookie jar,
// connections kept between steps and closed when the run ends.
type session struct {
	follow *http.Client
	// stay answers a redirect instead of following it, for a step that
	// checks the redirect itself.
	stay  *http.Client
	close func()
}

func (p *Prober) session(req probe.Request) session {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	dialer := req.Dialer("tcp")
	conns := &probe.Conns{}
	tr := &http.Transport{
		TLSClientConfig:   p.tls.Clone(),
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return conns.Dial(ctx, dialer, network, addr)
		},
	}
	follow := &http.Client{Transport: tr, Jar: jar, CheckRedirect: confine}
	stay := *follow
	stay.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return session{follow: follow, stay: &stay, close: func() {
		tr.CloseIdleConnections()
		conns.CloseAll()
	}}
}

// keep records a run for the API.
func (p *Prober) keep(r Report) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prev, ok := p.last[r.Target]; ok && prev.Started.After(r.Started) {
		return // a forwarded run older than one already here
	}
	p.last[r.Target] = r
	if p.heir != nil {
		p.heir.keep(r)
	}
	if len(p.last) <= maxReports {
		return
	}
	// The oldest is a target discovery no longer returns.
	oldest := r
	for _, rep := range p.last {
		if rep.Started.Before(oldest.Started) {
			oldest = rep
		}
	}
	delete(p.last, oldest.Target)
}

// Inherit implements probe.Heir: an edited scenario keeps the account of its
// last runs, the one the reload interrupted included.
func (p *Prober) Inherit(old probe.Prober) {
	prev, ok := old.(*Prober)
	if !ok || prev == p {
		return
	}
	// Always the old prober's lock first, as keep takes them.
	prev.mu.Lock()
	defer prev.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	for target, r := range prev.last {
		if _, newer := p.last[target]; !newer {
			p.last[target] = r
		}
	}
	prev.heir = p
}

// Reports is the last run of every target, by target name.
func (p *Prober) Reports() []Report {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Report, 0, len(p.last))
	for _, r := range p.last {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Report) int { return strings.Compare(a.Target, b.Target) })
	return out
}

// Steps names the steps and the cleanup steps, in the order they run.
func (p *Prober) Steps() (steps, cleanup []string) {
	for _, s := range p.sc.steps {
		steps = append(steps, s.name)
	}
	for _, s := range p.sc.cleanup {
		cleanup = append(cleanup, s.name)
	}
	return steps, cleanup
}

// scope reads the secrets and starts the variables for one run.
func (c *compiled) scope() (*scope, error) {
	secrets := make(map[string]string, len(c.secrets))
	for name, sec := range c.secrets {
		v, err := sec.read()
		if err != nil {
			return nil, fmt.Errorf("secrets.%s: %w", name, err)
		}
		secrets[name] = v
	}
	vars := make(map[string]string, len(c.vars))
	maps.Copy(vars, c.vars)
	return newScope(vars, secrets), nil
}

func (s Secret) read() (string, error) {
	if s.Value != "" {
		return s.Value, nil
	}
	if s.Env != "" {
		v, ok := os.LookupEnv(s.Env)
		if !ok || v == "" {
			return "", fmt.Errorf("environment variable %s is not set", s.Env)
		}
		return v, nil
	}
	data, err := os.ReadFile(s.File)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", fmt.Errorf("%s is empty", s.File)
	}
	return v, nil
}

// newRunID tells one run's leftovers from another's on the target.
func newRunID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
