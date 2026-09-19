package dnsprobe

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func TestSlowResolverInsideTheTimeoutAnswers(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		time.Sleep(2300 * time.Millisecond)
		resp := new(dns.Msg)
		resp.SetReply(m)
		rr, _ := dns.NewRR(m.Question[0].Name + " 300 IN A 1.1.1.1")
		resp.Answer = append(resp.Answer, rr)
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	p := newProber(t, `servers: ["`+pc.LocalAddr().String()+`"]`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	res := p.Probe(ctx, probe.Request{Target: probe.Target{Name: "example.test", Host: "example.test"}},
		metrics.NewRecorder(metrics.Labels{}))
	if res.Err != nil {
		t.Fatalf("gave up after %s of a 5s timeout: %v", time.Since(start).Round(100*time.Millisecond), res.Err)
	}
}
