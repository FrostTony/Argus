// Package websocket checks that a server completes the RFC 6455 upgrade, and
// optionally that it answers a message sent over the socket it opened.
//
// The handshake alone is the useful check: a proxy that forwards HTTP but drops
// Upgrade returns a perfectly healthy 200 to every other prober.
package websocket

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 names sha1 for the accept token
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func init() { probe.Register("websocket", New) }

// acceptGUID is the constant RFC 6455 mixes into the accept token.
const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Config is the `websocket:` block.
type Config struct {
	Port int    `yaml:"port"`
	Path string `yaml:"path"`
	// TLS makes it wss://. A target given as a wss:// URL sets it too.
	TLS        bool            `yaml:"tls"`
	TLSOptions tlsinfo.Options `yaml:"tls_config"`

	Headers map[string]string `yaml:"headers"`
	Origin  string            `yaml:"origin"`
	// Subprotocols are offered in Sec-WebSocket-Protocol; the one the server
	// picks is reported and, when any were offered, required.
	Subprotocols []string `yaml:"subprotocols"`

	// Send is a text message written once the socket is open.
	Send string `yaml:"send"`
	// Expect is a pattern the message that comes back must match.
	Expect string `yaml:"expect"`
	// Ping asks for a ping/pong round trip, which proves the socket carries
	// frames rather than only that it was opened.
	Ping bool `yaml:"ping"`

	ReadBytes int `yaml:"read_bytes"`
}

type Prober struct {
	cfg    Config
	expect *regexp.Regexp
	tls    *tls.Config
}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{Path: "/", ReadBytes: 4096}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(cfg.Path, "/") {
		cfg.Path = "/" + cfg.Path
	}
	if cfg.Ping && (cfg.Send != "" || cfg.Expect != "") {
		return nil, fmt.Errorf("websocket: ping and send/expect are two different exchanges; pick one")
	}
	if cfg.Expect != "" && cfg.Send == "" {
		return nil, fmt.Errorf("websocket.expect: nothing is sent to answer; set send too")
	}
	p := &Prober{cfg: cfg}
	if cfg.Expect != "" {
		re, err := regexp.Compile(cfg.Expect)
		if err != nil {
			return nil, fmt.Errorf("websocket.expect: %w", err)
		}
		p.expect = re
	}
	tc, err := tlsinfo.Build(cfg.TLSOptions)
	if err != nil {
		return nil, err
	}
	p.tls = tc
	return p, nil
}

// Files implements probe.FileBacked.
func (p *Prober) Files() []string { return p.cfg.TLSOptions.Files() }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	target, err := p.endpoint(req)
	if err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "%v", err)
		return res
	}

	start := time.Now()
	conn, err := req.Dialer("tcp").DialContext(ctx, "tcp", target.addr)
	res.Add("connect", time.Since(start))
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonConnect, err)
		return res
	}
	// The closure, not the value: TLS replaces conn, and closing the raw one
	// would skip close_notify.
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if target.secure {
		if conn, err = p.upgradeTLS(ctx, conn, target.host, rec, &res); err != nil {
			res.Err = probe.Wrap(probe.ReasonTLS, err)
			return res
		}
	}

	r := bufio.NewReader(conn)
	if res.Err = p.handshake(conn, r, target, rec, &res); res.Err != nil {
		return res
	}
	res.Err = p.exchange(conn, r, rec, &res)
	return res
}

// endpoint is where to dial and what to ask for.
type endpoint struct {
	addr   string // host:port to dial, the pinned backend when there is one
	host   string // the name to verify the certificate against (SNI)
	header string // the same name as a Host header: an IPv6 literal in brackets
	path   string
	secure bool
}

func (p *Prober) endpoint(req probe.Request) (endpoint, error) {
	out := endpoint{secure: p.cfg.TLS, path: p.cfg.Path, host: req.ServerName()}
	urlPort := 0

	if u := req.Target.URL; u != nil {
		switch u.Scheme {
		case "wss", "https":
			out.secure = true
		case "ws", "http":
			out.secure = false
		default:
			return out, fmt.Errorf("websocket: %q is not a ws, wss, http or https URL", u)
		}
		if req.Hostname == "" {
			out.host = u.Hostname()
		}
		urlPort, _ = strconv.Atoi(u.Port())
		if p.cfg.Path == "/" && u.Path != "" {
			out.path = u.RequestURI()
		}
	}
	scheme := 80
	if out.secure {
		scheme = 443
	}
	// The same order every other prober uses, ending in the scheme's own port.
	addr, err := req.Address(p.cfg.Port, urlPort, req.Backend.Port, req.Target.Port, scheme)
	if err != nil {
		return out, err
	}
	out.addr = addr
	if out.host == "" {
		out.host, _, _ = net.SplitHostPort(addr)
	}
	// An IPv6 literal is bracketed in a URL and in a Host header, and bare in
	// SNI and in certificate verification.
	out.header = out.host
	if strings.Contains(out.host, ":") {
		out.header = "[" + out.host + "]"
	}
	return out, nil
}

// handshake sends the upgrade request and checks the answer.
func (p *Prober) handshake(conn net.Conn, r *bufio.Reader, target endpoint, rec *metrics.Recorder, res *probe.Result) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return probe.Fail(probe.ReasonInternal, "websocket nonce: %v", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])

	req, err := http.NewRequest(http.MethodGet, "http://"+target.header+target.path, nil)
	if err != nil {
		return probe.Fail(probe.ReasonInternal, "%v", err)
	}
	req.Host = target.header
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	if p.cfg.Origin != "" {
		req.Header.Set("Origin", p.cfg.Origin)
	}
	if len(p.cfg.Subprotocols) > 0 {
		req.Header.Set("Sec-WebSocket-Protocol", strings.Join(p.cfg.Subprotocols, ", "))
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	if err := req.Write(conn); err != nil {
		res.Add("write", time.Since(start))
		return probe.Wrap(probe.ReasonProtocol, err)
	}
	res.Add("write", time.Since(start))

	start = time.Now()
	resp, err := http.ReadResponse(r, req)
	res.Add("ttfb", time.Since(start))
	if err != nil {
		return probe.Wrap(probe.ReasonProtocol, err)
	}
	defer resp.Body.Close()

	rec.Gauge("websocket_handshake_status_code", float64(resp.StatusCode))
	if sub := resp.Header.Get("Sec-WebSocket-Protocol"); sub != "" {
		rec.Info("websocket_subprotocol_info", sub)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		// The usual answer from a proxy that forwards HTTP and drops Upgrade.
		return probe.Fail(probe.ReasonStatus, "handshake answered %d, want 101", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return probe.Fail(probe.ReasonProtocol, "101 without Upgrade: websocket")
	}
	if !tokenIn(resp.Header.Get("Connection"), "upgrade") {
		return probe.Fail(probe.ReasonProtocol, "101 without Connection: Upgrade")
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), accept(key); got != want {
		return probe.Fail(probe.ReasonProtocol, "Sec-WebSocket-Accept is %q, want %q", got, want)
	}
	// An extension we did not offer changes what the frames mean; the payload
	// would then be matched as plaintext and fail for the wrong reason.
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		return probe.Fail(probe.ReasonProtocol, "the server turned on extensions that were not offered: %s", ext)
	}
	chosen := resp.Header.Get("Sec-WebSocket-Protocol")
	switch {
	case len(p.cfg.Subprotocols) == 0 && chosen != "":
		return probe.Fail(probe.ReasonProtocol, "the server chose subprotocol %q, which was not offered", chosen)
	case len(p.cfg.Subprotocols) == 0:
	case chosen == "":
		return probe.Fail(probe.ReasonProtocol, "the server chose none of the offered subprotocols")
	case !slices.Contains(p.cfg.Subprotocols, chosen):
		// The gateway routed elsewhere, which is exactly what offering a
		// subprotocol is meant to catch.
		return probe.Fail(probe.ReasonProtocol, "the server chose subprotocol %q, which was not offered", chosen)
	}
	return nil
}

// tokenIn reports whether a comma-separated header list carries a token.
func tokenIn(header, want string) bool {
	for _, t := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// exchange is what happens on the open socket, if anything was asked for.
func (p *Prober) exchange(conn net.Conn, r *bufio.Reader, rec *metrics.Recorder, res *probe.Result) error {
	// Closed politely whatever happens: a server that counts open sockets
	// should not count this one for the next hour.
	defer func() { _ = writeFrame(conn, opClose, nil) }()

	switch {
	case p.cfg.Send != "" || p.expect != nil:
		start := time.Now()
		if err := writeFrame(conn, opText, []byte(p.cfg.Send)); err != nil {
			res.Add("write", time.Since(start))
			return probe.Wrap(probe.ReasonProtocol, err)
		}
		res.Add("write", time.Since(start))
		if p.expect == nil {
			return nil
		}
		payload, err := p.message(conn, r, res, opText, opBinary)
		if err != nil {
			return err
		}
		rec.Gauge("websocket_response_size_bytes", float64(len(payload)))
		match := p.expect.FindStringSubmatch(string(payload))
		if match == nil {
			return probe.FailRegex("message %q does not match %q", probe.Excerpt(string(payload)), p.cfg.Expect)
		}
		probe.ExpectInfo(rec, p.expect, match)

	case p.cfg.Ping:
		start := time.Now()
		if err := writeFrame(conn, opPing, []byte("argus")); err != nil {
			res.Add("write", time.Since(start))
			return probe.Wrap(probe.ReasonProtocol, err)
		}
		res.Add("write", time.Since(start))
		if _, err := p.message(conn, r, res, opPong); err != nil {
			return err
		}
	}
	return nil
}

// message waits for one complete message of a wanted opcode. It answers the
// housekeeping frames that arrive in between and joins the continuation frames
// a server may split its answer into — a proxy that flushes per chunk
// fragments everything, and matching only the first fragment would fail a
// healthy service. A close frame ends the wait: the server has spoken.
func (p *Prober) message(conn net.Conn, r *bufio.Reader, res *probe.Result, want ...opcode) ([]byte, error) {
	limit := p.cfg.ReadBytes
	if limit <= 0 || limit > maxFrame {
		limit = maxFrame
	}
	var payload []byte
	started := false

	for {
		start := time.Now()
		f, err := readFrame(r, limit)
		res.Add("read", time.Since(start))
		if err != nil {
			return nil, probe.Wrap(probe.ReasonProtocol, err)
		}

		switch f.op {
		case opPing:
			// Answered rather than ignored: a server that pings an unresponsive
			// client is entitled to hang up on it.
			_ = writeFrame(conn, opPong, f.payload)
			continue
		case opPong:
			if slices.Contains(want, opPong) {
				return f.payload, nil
			}
			continue
		case opClose:
			return nil, probe.Fail(probe.ReasonProtocol, "the server closed the socket: %s", closeReason(f.payload))
		}

		switch {
		case !started && !slices.Contains(want, f.op):
			return nil, probe.Fail(probe.ReasonProtocol, "the server sent a %s frame, which was not what was asked for", f.op)
		case started && f.op != opContinuation:
			return nil, probe.Fail(probe.ReasonProtocol, "a %s frame arrived inside a fragmented message", f.op)
		}
		started = true

		if len(payload)+len(f.payload) > limit {
			return nil, probe.Fail(probe.ReasonProtocol,
				"the message is over the %d-byte read_bytes limit", limit)
		}
		payload = append(payload, f.payload...)
		if f.final {
			return payload, nil
		}
	}
}

func (p *Prober) upgradeTLS(ctx context.Context, conn net.Conn, host string, rec *metrics.Recorder, res *probe.Result) (net.Conn, error) {
	cfg := p.tls.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	tconn := tls.Client(conn, cfg)
	start := time.Now()
	err := tconn.HandshakeContext(ctx)
	res.Add("tls", time.Since(start))
	if err != nil {
		return nil, err
	}
	state := tconn.ConnectionState()
	over, err := tlsinfo.Inspect(ctx, rec, &state, p.cfg.TLSOptions, time.Now())
	res.Overhead += over
	if err != nil {
		return nil, err
	}
	return tconn, nil
}

// accept is the token RFC 6455 says the server must echo back.
func accept(key string) string {
	sum := sha1.Sum([]byte(key + acceptGUID)) //nolint:gosec // named by the RFC
	return base64.StdEncoding.EncodeToString(sum[:])
}

// closeReason reads the status code and text of a close frame, if it carries one.
func closeReason(payload []byte) string {
	if len(payload) < 2 {
		return "no reason given"
	}
	code := int(payload[0])<<8 | int(payload[1])
	if len(payload) == 2 {
		return strconv.Itoa(code)
	}
	return fmt.Sprintf("%d %s", code, probe.Excerpt(string(payload[2:])))
}
