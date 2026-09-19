package resolve

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Where TCP/53 is blocked, the truncated UDP answer is used as far as it goes.
func TestTruncatedAnswerSurvivesBlockedTCP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Truncated = true
		if m.Question[0].Qtype == dns.TypeA {
			rr, _ := dns.NewRR(m.Question[0].Name + " 60 IN A 10.0.0.1")
			resp.Answer = append(resp.Answer, rr)
		}
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	d := NewDNS([]string{pc.LocalAddr().String()}, time.Second, true)
	res, err := d.Resolve(context.Background(), "site.test", IPv4)
	if err != nil || len(res.Addrs) != 1 {
		t.Fatalf("udp answered 10.0.0.1 (truncated), tcp is closed: addrs=%v err=%v", res.Addrs, err)
	}
}
