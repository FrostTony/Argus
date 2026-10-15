package probe

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

func TestPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":              true,
		"2606:4700:4700::1111": true,
		"64:ff9b::808:808":     true, // NAT64 of 8.8.8.8

		"127.0.0.1":          false,
		"::1":                false,
		"0.0.0.0":            false,
		"0.1.2.3":            false, // "this network"
		"10.1.2.3":           false,
		"172.16.0.1":         false,
		"192.168.1.1":        false,
		"169.254.169.254":    false, // AWS, GCP, Azure metadata
		"fd00:ec2::254":      false, // AWS metadata over IPv6
		"fe80::1":            false,
		"100.100.100.200":    false, // Alibaba metadata, in carrier-grade NAT space
		"192.0.0.192":        false, // Oracle metadata
		"198.18.0.1":         false,
		"240.0.0.1":          false,
		"255.255.255.255":    false,
		"224.0.0.1":          false,
		"ff02::1":            false,
		"2001:db8::1":        false,
		"::ffff:127.0.0.1":   false, // mapped loopback
		"::ffff:169.254.0.1": false,
		"64:ff9b::7f00:1":    false, // NAT64 of 127.0.0.1
		"64:ff9b::a9fe:a9fe": false, // NAT64 of 169.254.169.254
		"64:ff9b:1::a00:1":   false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %v, want %v", addr, got, want)
		}
	}
}

// The guard sits where the address is final: after resolution, on every dial.
func TestCaptureDialerVetsTheResolvedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	dial := func(c *Capture, host string) error {
		conn, err := Request{Capture: c}.Dialer("tcp").DialContext(context.Background(), "tcp", net.JoinHostPort(host, port))
		if err == nil {
			conn.Close()
		}
		return err
	}
	if err := dial(nil, "127.0.0.1"); err != nil {
		t.Errorf("a check without capture was vetted: %v", err)
	}
	if err := dial(&Capture{}, "127.0.0.1"); err != nil {
		t.Errorf("capture: all refused loopback: %v", err)
	}
	public := &Capture{Allow: Public}
	for _, host := range []string{"127.0.0.1", "localhost"} {
		err := dial(public, host)
		if err == nil {
			t.Fatalf("capture reached %s", host)
		}
		if ReasonOf(err) != ReasonConnect {
			t.Errorf("%s: %v is not a connect failure", host, err)
		}
	}
}

func FuzzPublic(f *testing.F) {
	for _, s := range []string{"8.8.8.8", "127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::7f00:1", "fe80::1"} {
		f.Add(netip.MustParseAddr(s).AsSlice())
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		a, ok := netip.AddrFromSlice(raw)
		if !ok {
			return
		}
		got := Public(a)
		if got && (a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsPrivate() || a.IsUnspecified() || a.IsMulticast()) {
			t.Fatalf("Public(%s) = true", a)
		}
		if a.Is4() && Public(netip.AddrFrom16(a.As16())) != got {
			t.Fatalf("%s and its mapped form disagree", a)
		}
		if a.Is4() && Public(netip.AddrFrom16([16]byte(append(netip.MustParseAddr("64:ff9b::").AsSlice()[:12], a.AsSlice()...)))) != got {
			t.Fatalf("%s and its NAT64 form disagree", a)
		}
	})
}
