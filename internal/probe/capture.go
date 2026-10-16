package probe

import (
	"net/netip"
	"syscall"
)

// Capture is an on-demand run's request for what the target answered.
type Capture struct {
	// Allow vets every address a capturing check connects to — redirects,
	// proxies and a name that resolves elsewhere the second time included.
	// Nil allows any.
	Allow func(netip.Addr) bool
}

// CaptureLimit bounds one captured body, whatever the probe allows itself:
// every answer is held whole until the reply is written.
const CaptureLimit = 2 << 20

// Artifact is something the target answered, handed back as it came. The node
// keeps none of it.
type Artifact struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	// URL is where it came from, which a redirect may have moved off the target.
	URL string `json:"url,omitempty"`
	// Data goes out as base64: a page in windows-1251 is bytes, not JSON text.
	Data []byte `json:"data"`
	// Truncated means the answer went on past a limit and Data stops there.
	Truncated bool `json:"truncated,omitempty"`
}

// Control is a net.Dialer.Control that refuses any address allow does not
// admit, naming it in refusal's %s. It runs on the address about to be
// connected to, after resolution — the only point a name that resolves
// elsewhere the second time cannot slip past.
func Control(allow func(netip.Addr) bool, refusal string) func(string, string, syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return Fail(ReasonConnect, "%v", err)
		}
		if !allow(ap.Addr()) {
			return Fail(ReasonConnect, refusal, ap.Addr())
		}
		return nil
	}
}

// control vets a capturing request's connections, when the node limits them.
func (c *Capture) control() func(string, string, syscall.RawConn) error {
	if c.Allow == nil {
		return nil
	}
	return Control(c.Allow, "capture does not reach %s: the node's http.api.capture does not admit it")
}

// Internal is Public plus the networks a node inside a company checks its
// services on — private (RFC 1918, fc00::/7) and carrier-grade NAT space —
// and still never loopback, link-local (cloud metadata), unspecified,
// multicast or reserved space: those are the node's own, not a service's.
func Internal(a netip.Addr) bool {
	if Public(a) {
		return true
	}
	a = a.Unmap()
	if nat64.Contains(a) {
		a = netip.AddrFrom4([4]byte(a.AsSlice()[12:]))
	}
	return a.IsGlobalUnicast() && (a.IsPrivate() || cgnat.Contains(a))
}

// Public reports whether the internet at large reaches an address — what a
// capture may read without reading this node's own network: no loopback,
// link-local (cloud metadata lives there), private, shared or reserved space.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if nat64.Contains(a) {
		// The gateway would carry the request to the IPv4 address inside.
		a = netip.AddrFrom4([4]byte(a.AsSlice()[12:]))
	}
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var (
	cgnat    = netip.MustParsePrefix("100.64.0.0/10")
	nat64    = netip.MustParsePrefix("64:ff9b::/96")
	reserved = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),     // "this network": 0.x reaches the node itself
		netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
		netip.MustParsePrefix("192.0.0.0/24"),  // protocol assignments
		netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
		netip.MustParsePrefix("240.0.0.0/4"),   // reserved
		netip.MustParsePrefix("64:ff9b:1::/48"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
)
