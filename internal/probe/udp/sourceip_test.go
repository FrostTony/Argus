package udp

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// The dialer's local address must be a UDP address, not a TCP one.
func TestSourceIPWorksOverUDP(t *testing.T) {
	port := echo(t, "pong")
	p := newProber(t, `{send: "ping", expect: "pong"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := probe.Request{
		Target:   probe.Target{Name: "local", Host: "127.0.0.1"},
		Backend:  probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
		SourceIP: netip.MustParseAddr("127.0.0.1"),
	}
	res := p.Probe(ctx, req, metrics.NewRecorder(metrics.Labels{}))
	if !res.OK() {
		t.Fatalf("healthy UDP target fails as soon as source_ip is set: %v", res.Err)
	}
}
