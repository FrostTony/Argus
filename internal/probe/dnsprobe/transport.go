package dnsprobe

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

// The four ways to ask the same question. They are one option rather than four
// probe types because the question, the answer and every check on it are the
// same; only the pipe differs — and a resolver behind DoT checked over :53/udp
// is a check of a path nobody uses.
const (
	protoUDP   = "udp"
	protoTCP   = "tcp"
	protoTLS   = "tls"   // DNS over TLS, RFC 7858
	protoHTTPS = "https" // DNS over HTTPS, RFC 8484
)

// protoAliases lets a probe be spelled the way its RFC is usually named.
var protoAliases = map[string]string{
	"udp": protoUDP, "tcp": protoTCP,
	"tls": protoTLS, "dot": protoTLS, "tcp-tls": protoTLS,
	"https": protoHTTPS, "doh": protoHTTPS,
}

// defaultPort is where each protocol listens when the server names no port.
var defaultPort = map[string]string{protoUDP: "53", protoTCP: "53", protoTLS: "853"}

// timing is one exchange split into what it was spent on. Connecting is the
// network, answering is the resolver, and a DoT resolver that is slow to shake
// hands is a different problem from one that is slow to answer. Overhead is
// what was spent on neither — a trip to an OCSP responder.
type timing struct{ connect, query, overhead time.Duration }

func (t timing) total() time.Duration { return t.connect + t.query }

// apply names the phases on a result and hands back the time that was not the
// resolver's, which the runner subtracts from the probe's duration.
func (t timing) apply(res *probe.Result) {
	res.Add("connect", t.connect)
	res.Add("query", t.query)
	res.Overhead += t.overhead
}

// transport carries one question to one server.
type transport struct {
	proto string
	opts  tlsinfo.Options
	tls   *tls.Config
}

func newTransport(proto string, opts tlsinfo.Options) (*transport, error) {
	t := &transport{proto: proto, opts: opts}
	if proto != protoTLS && proto != protoHTTPS {
		return t, nil
	}
	cfg, err := tlsinfo.Build(opts)
	if err != nil {
		return nil, err
	}
	t.tls = cfg
	return t, nil
}

// server normalises how one server of this protocol is written.
func (t *transport) server(s string) (string, error) {
	s = strings.TrimSpace(s)
	if t.proto == protoHTTPS {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return "", fmt.Errorf("dns.servers: %q is not an https:// URL, which DNS over HTTPS needs", s)
		}
		if u.Path == "" {
			// The path every public resolver serves, and what RFC 8484 uses in
			// its examples.
			u.Path = "/dns-query"
		}
		return u.String(), nil
	}
	if strings.Contains(s, "://") {
		return "", fmt.Errorf("dns.servers: %q is a URL, which only proto: https takes", s)
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s, nil
	}
	return net.JoinHostPort(s, defaultPort[t.proto]), nil
}

// exchange asks one server. rec receives the resolver's certificate when the
// transport has one; a resolver's certificate expires like any other.
func (t *transport) exchange(ctx context.Context, req probe.Request, m *dns.Msg, server string, rec *metrics.Recorder) (*dns.Msg, timing, error) {
	if t.proto == protoHTTPS {
		return t.overHTTPS(ctx, req, m, server, rec)
	}

	c := &dns.Client{Net: netOf(t.proto), UDPSize: edns0Size, Dialer: req.Dialer(dialNet(t.proto))}
	if deadline, ok := ctx.Deadline(); ok {
		c.Timeout = max(time.Until(deadline), time.Millisecond)
	}
	if t.proto == protoTLS {
		c.TLSConfig = t.tls.Clone()
		if c.TLSConfig.ServerName == "" {
			// The resolver's certificate names the resolver, not its address.
			c.TLSConfig.ServerName, _, _ = net.SplitHostPort(server)
		}
	}

	start := time.Now()
	conn, err := c.DialContext(ctx, server)
	span := timing{connect: time.Since(start)}
	if err != nil {
		return nil, span, err
	}
	defer conn.Close()

	var state *tls.ConnectionState
	if tc, ok := conn.Conn.(*tls.Conn); ok {
		st := tc.ConnectionState()
		state = &st
	}

	resp, rtt, err := c.ExchangeWithConnContext(ctx, m, conn)
	span.query = rtt
	if err != nil {
		return resp, span, err
	}
	// After the question, not before it: an OCSP responder on the far side of
	// the internet must not spend the resolver's share of the deadline.
	if state != nil {
		over, revoked := tlsinfo.Inspect(ctx, rec, state, t.opts, time.Now())
		span.overhead = over
		if revoked != nil {
			return resp, span, probe.Fail(probe.ReasonTLS, "%s: %v", server, revoked)
		}
	}
	return resp, span, nil
}

// netOf is the name the dns client knows the protocol by.
func netOf(proto string) string {
	if proto == protoTLS {
		return "tcp-tls"
	}
	return proto
}

// dialNet is what the source address must be bound for.
func dialNet(proto string) string {
	if proto == protoUDP {
		return "udp"
	}
	return "tcp"
}

// dohMaxBody bounds what a resolver may answer with. A DNS message is 64 KiB at
// the very most, and an HTTPS endpoint can send anything at all.
const dohMaxBody = 1 << 17

func (t *transport) overHTTPS(ctx context.Context, req probe.Request, m *dns.Msg, endpoint string, rec *metrics.Recorder) (*dns.Msg, timing, error) {
	// RFC 8484: the id is zero, so that the same question is the same cache key
	// for every client asking it.
	q := m.Copy()
	q.Id = 0
	wire, err := q.Pack()
	if err != nil {
		return nil, timing{}, err
	}

	var connected time.Duration
	start := time.Now()
	trace := &httptrace.ClientTrace{
		// Everything up to a usable connection — dial and handshake both.
		GotConn: func(httptrace.GotConnInfo) { connected = time.Since(start) },
	}
	hr, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace),
		http.MethodPost, endpoint, bytes.NewReader(wire))
	if err != nil {
		return nil, timing{}, err
	}
	hr.Header.Set("Content-Type", "application/dns-message")
	hr.Header.Set("Accept", "application/dns-message")

	tr := t.transport(req)
	defer tr.CloseIdleConnections()

	resp, err := (&http.Client{Transport: tr}).Do(hr)
	span := timing{connect: connected, query: time.Since(start) - connected}
	if err != nil {
		return nil, span, err
	}
	defer resp.Body.Close()

	if resp.TLS != nil {
		over, revoked := tlsinfo.Inspect(ctx, rec, resp.TLS, t.opts, time.Now())
		span.overhead = over
		if revoked != nil {
			return nil, span, probe.Fail(probe.ReasonTLS, "%s: %v", endpoint, revoked)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, span, fmt.Errorf("%s answered %d, want 200", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dohMaxBody))
	if err != nil {
		return nil, span, err
	}
	out := new(dns.Msg)
	if err := out.Unpack(body); err != nil {
		return nil, span, fmt.Errorf("%s: the answer is not a DNS message: %w", endpoint, err)
	}
	if err := answersQuestion(out, q); err != nil {
		return nil, span, fmt.Errorf("%s: %w", endpoint, err)
	}
	return out, span, nil
}

// answersQuestion rejects a reply that is not to the question asked. Over UDP
// the resolver library checks this; an HTTPS endpoint is just a server that
// returned some bytes, and its records go straight into the answer checks.
func answersQuestion(out, q *dns.Msg) error {
	switch {
	case !out.Response:
		return fmt.Errorf("the answer is a query, not a response")
	case out.Id != q.Id:
		return fmt.Errorf("the answer carries id %d, want %d", out.Id, q.Id)
	case len(out.Question) != 1:
		return fmt.Errorf("the answer carries %d questions, want 1", len(out.Question))
	}
	got, want := out.Question[0], q.Question[0]
	if !strings.EqualFold(got.Name, want.Name) || got.Qtype != want.Qtype || got.Qclass != want.Qclass {
		return fmt.Errorf("the answer is to %s %s, not %s %s",
			dns.TypeToString[got.Qtype], got.Name, dns.TypeToString[want.Qtype], want.Name)
	}
	return nil
}

// transport is built per exchange so that it carries the run's source address
// and keeps nothing between runs: a probe measures a fresh connection, not a
// pool. HTTP/2 is left off deliberately — its transport ignores
// DisableKeepAlives, and a pooled connection is not what is being measured.
func (t *transport) transport(req probe.Request) *http.Transport {
	return &http.Transport{
		TLSClientConfig:   t.tls.Clone(),
		DisableKeepAlives: true,
		DialContext:       req.Dialer("tcp").DialContext,
	}
}
