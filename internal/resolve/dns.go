package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// DNS queries the listed servers directly, reporting TTL, rcode and which
// server answered.
type DNS struct {
	// Servers are tried in order until one answers. A missing port becomes :53.
	Servers []string
	Timeout time.Duration
	Order   bool
	client  *dns.Client
	tcp     *dns.Client
}

func NewDNS(servers []string, timeout time.Duration, order bool) *DNS {
	norm := make([]string, 0, len(servers))
	for _, s := range servers {
		norm = append(norm, withPort(s, "53"))
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &DNS{
		Servers: norm,
		Timeout: timeout,
		Order:   order,
		client:  &dns.Client{Timeout: timeout, UDPSize: udpSize},
		tcp:     &dns.Client{Timeout: timeout, Net: "tcp"},
	}
}

// udpSize is the EDNS0 answer size advertised over UDP; without it an answer is
// cut at 512 bytes.
const udpSize = 1232

func withPort(addr, def string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, def)
}

func (d *DNS) Resolve(ctx context.Context, host string, family Family) (Result, error) {
	qtypes := []uint16{dns.TypeA, dns.TypeAAAA}
	switch family {
	case IPv4:
		qtypes = qtypes[:1]
	case IPv6:
		qtypes = qtypes[1:]
	}

	// A server counts only when it answers every question: a lost A reply beside
	// a good AAAA one is no answer. Failing that, the first partial answer comes
	// back with its error, so the cache holds it as a failure, not for its TTL.
	var partial Result
	var partialErr error
	err := errNoServers
	for _, server := range d.Servers {
		start := time.Now()
		res, answered, qerr := merge(d.ask(ctx, host, qtypes, server))
		// Both families are asked at once, so this is the slower of the two.
		res.Duration, res.Server = time.Since(start), server
		res.Addrs = filter(res.Addrs, family, d.Order)
		switch {
		case qerr == nil && len(res.Addrs) == 0:
			return res, ErrNoAddresses
		case qerr == nil:
			return res, nil
		case answered > 0 && partialErr == nil:
			partial, partialErr = res, qerr
		}
		err = qerr
	}
	if partialErr != nil {
		return partial, partialErr
	}
	return Result{}, err
}

// merge combines one server's replies. The TTL is the smallest of any record,
// so the cache outlives no part of the answer.
func merge(replies []reply) (res Result, answered int, err error) {
	var seen bool
	for _, r := range replies {
		if r.err != nil {
			err = r.err
			continue
		}
		answered++
		res.Addrs = append(res.Addrs, r.addrs...)
		if r.hasTTL && (!seen || r.ttl < res.TTL) {
			res.TTL, seen = r.ttl, true
		}
	}
	return res, answered, err
}

var errNoServers = errors.New("no resolvers configured")

type reply struct {
	addrs  []netip.Addr
	ttl    time.Duration
	hasTTL bool
	err    error
}

// ask puts every question to one server at the same time.
func (d *DNS) ask(ctx context.Context, host string, qtypes []uint16, server string) []reply {
	out := make([]reply, len(qtypes))
	if len(qtypes) == 1 {
		out[0] = d.query(ctx, host, qtypes[0], server)
		return out
	}
	var wg sync.WaitGroup
	for i, qt := range qtypes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = d.query(ctx, host, qt, server)
		}()
	}
	wg.Wait()
	return out
}

func (d *DNS) query(ctx context.Context, host string, qtype uint16, server string) reply {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), qtype)
	m.RecursionDesired = true
	m.SetEdns0(udpSize, false)

	resp, _, err := d.client.ExchangeContext(ctx, m, server)
	if err == nil && resp.Truncated {
		// Too big for a datagram: ask again over TCP, keeping what fits when
		// TCP/53 is filtered.
		if full, _, tcpErr := d.tcp.ExchangeContext(ctx, m, server); tcpErr == nil {
			resp = full
		}
	}
	if err != nil {
		return reply{err: fmt.Errorf("%s: %w", server, err)}
	}
	if resp.Rcode != dns.RcodeSuccess {
		return reply{err: fmt.Errorf("%s: rcode %s", server, dns.RcodeToString[resp.Rcode])}
	}

	var out reply
	for _, rr := range resp.Answer {
		// Every record of the chain bounds the answer's life, CNAMEs included.
		if t := time.Duration(rr.Header().Ttl) * time.Second; !out.hasTTL || t < out.ttl {
			out.ttl, out.hasTTL = t, true
		}
		var a netip.Addr
		switch v := rr.(type) {
		case *dns.A:
			a, _ = netip.AddrFromSlice(v.A.To4())
		case *dns.AAAA:
			a, _ = netip.AddrFromSlice(v.AAAA)
		}
		if a.IsValid() {
			out.addrs = append(out.addrs, a.Unmap())
		}
	}
	return out
}
