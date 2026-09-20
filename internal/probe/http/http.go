// Package http is the HTTP(S) prober. The request goes to a specific backend
// (DialContext pinned to the IP) while Host and SNI stay the domain, and the
// request time is split into phases with resolution left out.
package http

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/idna"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func init() { probe.Register("http", New) }

const userAgent = "argus/0.1"

// Config is the `http:` block of a probe.
type Config struct {
	Method   string            `yaml:"method"`
	Path     string            `yaml:"path"`
	Headers  map[string]string `yaml:"headers"`
	Body     string            `yaml:"body"`
	BodyFile string            `yaml:"body_file"`

	Auth         *Auth             `yaml:"auth"`
	ProxyURL     string            `yaml:"proxy_url"`
	ProxyHeaders map[string]string `yaml:"proxy_headers"`
	// ProxyFromEnvironment takes the proxy from HTTP_PROXY/HTTPS_PROXY/NO_PROXY,
	// as every other HTTP client on the host does.
	ProxyFromEnvironment bool `yaml:"proxy_from_environment"`
	// NoProxy lists hosts, domains and CIDRs to reach directly. It applies to
	// proxy_url too: a proxy for the outside world is not a proxy for a
	// neighbour, and the exception belongs beside the rule.
	NoProxy string `yaml:"no_proxy"`

	FollowRedirects *bool `yaml:"follow_redirects"`
	MaxRedirects    int   `yaml:"max_redirects"`

	ReadBody     *bool `yaml:"read_body"`
	MaxBodyBytes int64 `yaml:"max_body_bytes"`

	// Compression is off by default: gzip would make the size the compressed one.
	Compression bool `yaml:"compression"`
	// KeepAlive reuses the connection within one run; it is always off between runs.
	KeepAlive bool  `yaml:"keep_alive"`
	HTTP2     *bool `yaml:"http2"`

	TLS        tlsinfo.Options `yaml:"tls"`
	Validators []Validator     `yaml:"validators"`

	// ExposeFinalURL adds the redirect destination as a label; off by default.
	ExposeFinalURL bool `yaml:"expose_final_url"`
}

type Prober struct {
	cfg      Config
	follow   bool
	readBody bool
	keepBody bool
	maxBody  int64
	// sizeLimit must reach past the largest max_size_bytes or that validator can never fail.
	sizeLimit  int64
	tlsConfig  *tls.Config
	validators []Validator
	proxy      func(*url.URL) (*url.URL, error)
	body       []byte
}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{Method: http.MethodGet, MaxRedirects: 5, MaxBodyBytes: 1 << 20}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}

	p := &Prober{
		cfg:      cfg,
		follow:   cfg.FollowRedirects == nil || *cfg.FollowRedirects,
		readBody: cfg.ReadBody == nil || *cfg.ReadBody,
		maxBody:  cfg.MaxBodyBytes,
	}
	p.cfg.Method = strings.ToUpper(cmp.Or(cfg.Method, http.MethodGet))
	if p.maxBody <= 0 {
		p.maxBody = 1 << 20
	}

	vs := cfg.Validators
	if len(vs) == 0 {
		vs = []Validator{{Name: "status", StatusCode: []string{"200-399"}}}
	}
	p.sizeLimit = 2 * p.maxBody
	for i := range vs {
		if err := vs[i].compile(); err != nil {
			return nil, fmt.Errorf("validators[%d]: %w", i, err)
		}
		p.keepBody = p.keepBody || vs[i].readsBody()
		p.sizeLimit = max(p.sizeLimit, vs[i].MaxSizeBytes+1)
	}
	p.validators = vs

	tc, err := tlsinfo.Build(cfg.TLS)
	if err != nil {
		return nil, err
	}
	p.tlsConfig = tc

	if cfg.Auth != nil {
		if err := cfg.Auth.validate(); err != nil {
			return nil, err
		}
	}
	if cfg.ProxyURL != "" && cfg.ProxyFromEnvironment {
		return nil, fmt.Errorf("http: set proxy_url or proxy_from_environment, not both")
	}
	switch {
	case cfg.ProxyURL != "" && cfg.NoProxy == "":
		// Named without exceptions, the proxy is used for everything. The
		// environment's rules are deliberately not consulted here: a probe that
		// names its proxy is describing the path under test, and
		// httpproxy's built-in loopback bypass would quietly leave that path.
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("http.proxy_url: %w", err)
		}
		p.proxy = func(*url.URL) (*url.URL, error) { return u, nil }
	case cfg.ProxyURL != "":
		if _, err := url.Parse(cfg.ProxyURL); err != nil {
			return nil, fmt.Errorf("http.proxy_url: %w", err)
		}
		// One proxy for both schemes: proxy_url names the proxy, not a rule.
		// With exceptions, the matching is httpproxy's, which also sends
		// localhost and loopback addresses direct.
		p.proxy = (&httpproxy.Config{
			HTTPProxy: cfg.ProxyURL, HTTPSProxy: cfg.ProxyURL, NoProxy: cfg.NoProxy,
		}).ProxyFunc()
	case cfg.ProxyFromEnvironment:
		env := httpproxy.FromEnvironment()
		if cfg.NoProxy != "" {
			env.NoProxy = cfg.NoProxy
		}
		p.proxy = env.ProxyFunc()
	case cfg.NoProxy != "":
		return nil, fmt.Errorf("http.no_proxy: nothing to exclude from; set proxy_url or proxy_from_environment")
	case len(cfg.ProxyHeaders) > 0:
		return nil, fmt.Errorf("http.proxy_headers: nothing to send them to; set proxy_url or proxy_from_environment")
	}
	if cfg.Body != "" && cfg.BodyFile != "" {
		return nil, fmt.Errorf("http: set body or body_file, not both")
	}
	switch {
	case cfg.BodyFile != "":
		b, err := os.ReadFile(cfg.BodyFile)
		if err != nil {
			return nil, fmt.Errorf("http.body_file: %w", err)
		}
		p.body = b
	case cfg.Body != "":
		p.body = []byte(cfg.Body)
	}
	return p, nil
}

// Files implements probe.FileBacked.
func (p *Prober) Files() []string {
	files := p.cfg.TLS.Files()
	if p.cfg.BodyFile != "" {
		files = append(files, p.cfg.BodyFile)
	}
	return files
}

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	u, err := p.targetURL(req.Target)
	if err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "%v", err)
		return res
	}

	t := &timings{}
	httpReq, err := p.buildRequest(ctx, u, t)
	if err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "%v", err)
		return res
	}

	// The name presented, when it is not the one in the URL: Host and SNI move
	// together, or the server answers one virtual host and certifies another.
	if req.Hostname != "" {
		httpReq.Host = req.Hostname
	}

	client, conns := p.client(req, u)
	// An HTTP/2 stream aborted by the deadline leaves a connection that is not idle.
	defer conns.closeAll()

	// The token comes from elsewhere; its time is not the target's.
	authStart := time.Now()
	if err := p.cfg.Auth.apply(ctx, httpReq, req.Dialer("tcp"), p.tokenTimeout(ctx)); err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "%v", err)
		return res
	}
	res.Overhead = time.Since(authStart)

	resp, err := client.Do(httpReq)
	if err != nil {
		res.Err = probe.Wrap(classify(t), err)
		p.phases(&res, t, time.Time{})
		return res
	}
	defer resp.Body.Close()

	body, size, readErr := p.drain(resp)
	done := time.Now()
	p.phases(&res, t, done)
	p.recordResponse(rec, resp, size, t.wasReused())
	var revoked error
	if resp.TLS != nil {
		var over time.Duration
		over, revoked = tlsinfo.Inspect(ctx, rec, resp.TLS, p.cfg.TLS, done)
		res.Overhead += over
	}

	if readErr != nil {
		res.Err = probe.Wrap(probe.ReasonContent, readErr)
		return res
	}
	if revoked != nil {
		res.Err = probe.Fail(probe.ReasonTLS, "%v", revoked)
		return res
	}
	res.Err = p.validate(rec, resp, body, size, done)
	return res
}

// tokenTimeout leaves the token fetch inside the probe's own deadline.
func (p *Prober) tokenTimeout(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left > 0 {
			return left
		}
	}
	return 10 * time.Second
}

// client pins one backend at DialContext, leaving Host, SNI and verification on the domain.
func (p *Prober) client(req probe.Request, u *url.URL) (*http.Client, *connSet) {
	b := req.Backend
	origin := asciiHost(u.Hostname())
	dialer := req.Dialer("tcp")
	conns := &connSet{}
	tlsCfg := p.tlsConfig.Clone()
	if tlsCfg.ServerName == "" && req.Hostname != "" {
		tlsCfg.ServerName = req.Hostname
	}
	tr := &http.Transport{
		TLSClientConfig:    tlsCfg,
		DisableKeepAlives:  !p.cfg.KeepAlive,
		DisableCompression: !p.cfg.Compression,
		ForceAttemptHTTP2:  p.cfg.HTTP2 == nil || *p.cfg.HTTP2,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Only the domain under test is pinned.
			if b.Valid() && sameHost(addr, origin) {
				addr = pin(addr, b)
			}
			return conns.dial(ctx, dialer, network, addr)
		},
	}
	if p.proxy != nil {
		// A proxied request is dialled to the proxy, which the pinning dialer
		// leaves alone: it only pins the domain under test. A request no_proxy
		// sends direct is pinned like any other.
		tr.Proxy = func(r *http.Request) (*url.URL, error) { return p.proxy(r.URL) }
		if len(p.cfg.ProxyHeaders) > 0 {
			tr.ProxyConnectHeader = http.Header{}
			for k, v := range p.cfg.ProxyHeaders {
				tr.ProxyConnectHeader.Set(k, v)
			}
		}
	}
	c := &http.Client{Transport: tr}
	if !p.follow {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	} else {
		limit := p.cfg.MaxRedirects
		c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
			if len(via) >= limit {
				return fmt.Errorf("redirect limit exceeded (%d)", limit)
			}
			// The presented name is pinned to the transport, so following a
			// redirect elsewhere would offer it to a host it does not belong
			// to. Another domain is another service, as it is for the backend.
			if req.Hostname != "" && !sameName(r.URL.Hostname(), origin) {
				return fmt.Errorf("redirect to %s: hostname=%s is the name presented to %s, and another host is another service",
					r.URL.Host, req.Hostname, origin)
			}
			return nil
		}
	}
	return c, conns
}

// connSet holds the connections one check opened, to close them all when it ends.
type connSet struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (s *connSet) dial(ctx context.Context, d *net.Dialer, network, addr string) (net.Conn, error) {
	c, err := d.DialContext(ctx, network, addr)
	if err == nil {
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
	}
	return c, err
}

func (s *connSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
}

// asciiHost is the host as the transport dials it: IDNA ASCII for a Unicode name.
func asciiHost(host string) string {
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		return ascii
	}
	return host
}

// sameName compares two host names, in the form the transport dials them.
func sameName(a, b string) bool { return strings.EqualFold(asciiHost(a), asciiHost(b)) }

func sameHost(addr, host string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	return strings.EqualFold(h, host)
}

// pin swaps in the backend address, keeping the port the transport chose.
func pin(addr string, b probe.Backend) string {
	port := strconv.Itoa(b.Port)
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		port = p
	}
	return net.JoinHostPort(b.Addr.String(), port)
}

// targetURL builds the request address; the probe's path overrides the target's.
func (p *Prober) targetURL(t probe.Target) (*url.URL, error) {
	if t.URL != nil {
		u := *t.URL
		if p.cfg.Path != "" {
			u.Path = p.cfg.Path
			u.RawQuery = ""
		}
		return &u, nil
	}
	if t.Host == "" {
		return nil, fmt.Errorf("target has neither url nor host")
	}
	scheme := "https"
	if t.Port == 80 {
		scheme = "http"
	}
	host := t.Host
	if t.Port != 0 && t.Port != 80 && t.Port != 443 {
		host = net.JoinHostPort(host, strconv.Itoa(t.Port))
	}
	return &url.URL{Scheme: scheme, Host: host, Path: cmp.Or(p.cfg.Path, "/")}, nil
}

func (p *Prober) buildRequest(ctx context.Context, u *url.URL, t *timings) (*http.Request, error) {
	var body io.Reader
	if len(p.body) > 0 {
		body = bytes.NewReader(p.body)
	}
	req, err := http.NewRequestWithContext(withTrace(ctx, t), p.cfg.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if !p.cfg.Compression {
		req.Header.Set("Accept-Encoding", "identity")
	}
	for k, v := range p.cfg.Headers {
		// Host is not an ordinary header; net/http reads it from the struct field.
		if strings.EqualFold(k, "host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

// drain reads the body, bounded, and returns what was kept and the size read.
func (p *Prober) drain(resp *http.Response) (string, int64, error) {
	if !p.readBody {
		n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, p.maxBody))
		return "", n, nil
	}
	var sb strings.Builder
	var kept io.Writer = io.Discard
	if p.keepBody {
		kept = &sb
	}
	n, err := io.Copy(kept, io.LimitReader(resp.Body, p.maxBody))
	// Read past the limit so the connection survives and max_size_bytes can fail.
	extra, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, p.sizeLimit-n))
	return sb.String(), n + extra, err
}

func (p *Prober) recordResponse(rec *metrics.Recorder, resp *http.Response, size int64, reused bool) {
	rec.Gauge("http_status_code", float64(resp.StatusCode))
	rec.Gauge("http_response_size_bytes", float64(size))
	rec.Gauge("http_connection_reused", metrics.Bool(reused))
	rec.Info("http_proto_info", resp.Proto)
	if resp.ContentLength >= 0 {
		rec.Gauge("http_content_length", float64(resp.ContentLength))
	}
	rec.Gauge("http_redirects", float64(hops(resp)))
	rec.Gauge("http_ssl", metrics.Bool(resp.TLS != nil))
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			rec.Gauge("http_last_modified_seconds", float64(t.Unix()))
		}
	}
	if p.cfg.ExposeFinalURL && resp.Request != nil && resp.Request.URL != nil {
		rec.Info("http_final_url_info", resp.Request.URL.String())
	}
}

// hops counts redirects followed via the Response.Request.Response chain.
func hops(resp *http.Response) int {
	n := 0
	for r := resp.Request; r != nil && r.Response != nil; r = r.Response.Request {
		n++
	}
	return n
}

// timings holds the httptrace marks the phases are built from.
type timings struct {
	// mu guards the fields below; the transport's dial goroutines write them.
	mu sync.Mutex

	gotConn      time.Time
	connectStart time.Time
	connectDone  time.Time
	tlsStart     time.Time
	tlsDone      time.Time
	wroteRequest time.Time
	firstByte    time.Time
	reused       bool
	// Redirect hops share one trace; every hop before the last accumulates here.
	connect, tls, write, ttfb time.Duration
	hops                      int
	// ConnectDone and TLSHandshakeDone fire on failure too.
	connectErr error
	tlsErr     error
}

func withTrace(ctx context.Context, t *timings) context.Context {
	mark := func(set func(now time.Time)) {
		now := time.Now()
		t.mu.Lock()
		defer t.mu.Unlock()
		set(now)
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		// A new attempt on the same trace is a redirect hop.
		GetConn:           func(string) { mark(func(time.Time) { t.bank() }) },
		ConnectStart:      func(string, string) { mark(func(now time.Time) { t.connectStart = now }) },
		ConnectDone:       func(_, _ string, err error) { mark(func(now time.Time) { t.connectDone, t.connectErr = now, err }) },
		TLSHandshakeStart: func() { mark(func(now time.Time) { t.tlsStart = now }) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mark(func(now time.Time) { t.tlsDone, t.tlsErr = now, err })
		},
		GotConn: func(i httptrace.GotConnInfo) {
			mark(func(now time.Time) { t.gotConn, t.reused = now, i.Reused })
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { mark(func(now time.Time) { t.wroteRequest = now }) },
		GotFirstResponseByte: func() { mark(func(now time.Time) { t.firstByte = now }) },
	})
}

// bank folds the current hop's marks into the totals and clears them; mu is held.
func (t *timings) bank() {
	if t.connectStart.IsZero() && t.wroteRequest.IsZero() {
		return // the first hop has nothing to bank yet
	}
	t.hops++
	t.connect += sub(t.connectDone, t.connectStart)
	t.tls += sub(t.tlsDone, t.tlsStart)
	// Measured from gotConn: a hop on a reused connection has no earlier mark.
	t.write += sub(t.wroteRequest, t.gotConn)
	t.ttfb += sub(t.firstByte, t.wroteRequest)
	t.gotConn = time.Time{}
	t.connectStart, t.connectDone = time.Time{}, time.Time{}
	t.tlsStart, t.tlsDone = time.Time{}, time.Time{}
	t.wroteRequest, t.firstByte = time.Time{}, time.Time{}
}

// phases records only the marks that were reached, summed over every redirect hop.
func (p *Prober) phases(res *probe.Result, t *timings, done time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	last := t.firstByte
	t.bank()
	res.Add("connect", t.connect)
	res.Add("tls", t.tls)
	res.Add("write", t.write)
	res.Add("ttfb", t.ttfb)
	if !done.IsZero() {
		res.Add("transfer", sub(done, last))
	}
}

func (t *timings) wasReused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reused
}

func sub(b, a time.Time) time.Duration {
	if a.IsZero() || b.IsZero() || !b.After(a) {
		return 0
	}
	return b.Sub(a)
}

// classify derives the failure reason from where the attempt stopped.
func classify(t *timings) probe.FailureReason {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.connectErr != nil:
		return probe.ReasonConnect
	case t.reused:
		// Nothing was dialled, so a missing connect mark says nothing.
		return probe.ReasonProtocol
	case t.connectDone.IsZero():
		return probe.ReasonConnect
	case t.tlsErr != nil, !t.tlsStart.IsZero() && t.tlsDone.IsZero():
		return probe.ReasonTLS
	default:
		return probe.ReasonProtocol
	}
}
