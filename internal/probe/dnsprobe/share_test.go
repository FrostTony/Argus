//go:build linux

package dnsprobe

import (
	"context"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// blackhole is a TCP port whose SYNs go unanswered: its one backlog slot is taken.
func blackhole(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, _ := syscall.Getsockname(fd)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(sa.(*syscall.SockaddrInet4).Port))
	for i := 0; i < 8; i++ {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return addr
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	t.Skip("could not build a port that drops SYNs here")
	return ""
}

// A resolver's share of the run bounds the dial as well: the library applies
// Client.Timeout to the dial only when it builds the dialer itself.
func TestDeadTCPResolverDoesNotStarveTheNext(t *testing.T) {
	dead := blackhole(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{Listener: ln, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A 1.1.1.1")
		resp.Answer = append(resp.Answer, rr)
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	p := newProber(t, `{proto: tcp, servers: ["`+dead+`", "`+ln.Addr().String()+`"]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	res := p.Probe(ctx, probe.Request{Target: probe.Target{Name: "example.test", Host: "example.test"}},
		metrics.NewRecorder(metrics.Labels{}))
	if res.Err != nil {
		t.Fatalf("after %s: %v", time.Since(start).Round(100*time.Millisecond), res.Err)
	}
}
