package probe

import (
	"net/netip"
	"testing"
)

// private admits a company's own networks and nothing that is the node's.
func TestInternalAdmitsServicesNotTheNode(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":            true,
		"10.1.2.3":           true,
		"172.16.5.5":         true,
		"192.168.1.10":       true,
		"100.64.1.1":         true,
		"fd00::1":            true,
		"127.0.0.1":          false,
		"169.254.169.254":    false, // cloud metadata
		"::1":                false,
		"fe80::1":            false,
		"0.0.0.0":            false,
		"224.0.0.1":          false,
		"::ffff:127.0.0.1":   false,
		"64:ff9b::a9fe:a9fe": false, // metadata through NAT64
	} {
		if got := Internal(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Internal(%s) = %v, want %v", addr, got, want)
		}
	}
}
