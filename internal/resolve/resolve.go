// Package resolve turns a target name into its backends, measuring resolution
// separately from the probe that follows it.
package resolve

import (
	"cmp"
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
	"time"
)

type Family string

const (
	IPv4 Family = "ipv4"
	IPv6 Family = "ipv6"
	Both Family = "both"
)

func (f Family) allows(a netip.Addr) bool {
	switch f {
	case IPv4:
		return a.Is4() || a.Is4In6()
	case IPv6:
		return a.Is6() && !a.Is4In6()
	default:
		return true
	}
}

func FamilyOf(a netip.Addr) Family {
	if a.Is4() || a.Is4In6() {
		return IPv4
	}
	return IPv6
}

// Result is one resolution.
type Result struct {
	Addrs []netip.Addr
	// Duration is resolution time; a cache hit carries the real lookup's.
	Duration time.Duration
	// TTL is the smallest TTL in the answer; zero when the resolver hides it.
	TTL time.Duration
	// Cached answers are not recorded in the resolution metrics.
	Cached bool
	Server string
	At     time.Time
}

// Resolver turns a name into addresses of one family. Implementations are safe
// for concurrent use; one is shared by every probe on the node.
type Resolver interface {
	Resolve(ctx context.Context, host string, family Family) (Result, error)
}

const defaultTimeout = 3 * time.Second

// ErrNoAddresses means the name resolved but nothing matched the family.
var ErrNoAddresses = errors.New("no addresses of the configured family")

// System uses the OS resolver, which reports no TTL.
type System struct {
	Timeout time.Duration
	Order   bool
}

func (s *System) Resolve(ctx context.Context, host string, family Family) (Result, error) {
	// Never unbounded: a hanging lookup would hold every later tick behind it.
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(max(s.Timeout, 0), defaultTimeout))
	defer cancel()
	start := time.Now()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, network(family), host)
	res := Result{Duration: time.Since(start), Server: "system", At: time.Now()}
	if err != nil {
		return res, err
	}
	res.Addrs = filter(addrs, family, s.Order)
	if len(res.Addrs) == 0 {
		return res, ErrNoAddresses
	}
	return res, nil
}

func network(f Family) string {
	switch f {
	case IPv4:
		return "ip4"
	case IPv6:
		return "ip6"
	default:
		return "ip"
	}
}

func filter(addrs []netip.Addr, family Family, order bool) []netip.Addr {
	out := make([]netip.Addr, 0, len(addrs))
	seen := make(map[netip.Addr]bool, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if !family.allows(a) || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	if order {
		sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	}
	return out
}

// Static always returns the given addresses; used in tests.
type Static []netip.Addr

func (s Static) Resolve(_ context.Context, _ string, family Family) (Result, error) {
	addrs := filter(s, family, false)
	if len(addrs) == 0 {
		return Result{Server: "static"}, ErrNoAddresses
	}
	return Result{Addrs: addrs, Server: "static", At: time.Now()}, nil
}

// Literal recognises a target that is already an IP.
func Literal(host string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// Limit keeps the first n addresses, which by then are in a stable order.
func Limit(addrs []netip.Addr, n int) []netip.Addr {
	if n <= 0 || len(addrs) <= n {
		return addrs
	}
	return addrs[:n]
}
