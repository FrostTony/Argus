package resolve

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

const answerDelay = 100 * time.Millisecond

// serveDNS answers A and AAAA for any name, truncating over UDP when asked.
func serveDNS(t *testing.T, truncate bool) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Skipf("the same port is not free over tcp: %v", err)
	}
	handler := func(overUDP bool) dns.HandlerFunc {
		return func(w dns.ResponseWriter, m *dns.Msg) {
			resp := new(dns.Msg)
			resp.SetReply(m)
			if overUDP && truncate {
				resp.Truncated = true
				_ = w.WriteMsg(resp)
				return
			}
			time.Sleep(answerDelay)
			record := " 60 IN A 10.0.0.1"
			if m.Question[0].Qtype == dns.TypeAAAA {
				record = " 60 IN AAAA fd00::1"
			}
			rr, _ := dns.NewRR(m.Question[0].Name + record)
			resp.Answer = append(resp.Answer, rr)
			_ = w.WriteMsg(resp)
		}
	}
	for _, srv := range []*dns.Server{
		{PacketConn: pc, Handler: handler(true)},
		{Listener: ln, Handler: handler(false)},
	} {
		go func() { _ = srv.ActivateAndServe() }()
		t.Cleanup(func() { _ = srv.Shutdown() })
	}
	return pc.LocalAddr().String()
}

func TestDNSResolvesBothFamiliesAtOnce(t *testing.T) {
	d := NewDNS([]string{serveDNS(t, false)}, time.Second, true)
	res, err := d.Resolve(context.Background(), "site.test", Both)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Addrs) != 2 || res.TTL != time.Minute {
		t.Fatalf("result = %+v, want one address of each family and the 60s TTL", res)
	}
	// One answer's worth of waiting, not two: the questions go out together.
	if res.Duration < answerDelay || res.Duration > 2*answerDelay-answerDelay/5 {
		t.Fatalf("duration = %s, want about %s", res.Duration, answerDelay)
	}
}

func TestDNSFollowsTruncationToTCP(t *testing.T) {
	d := NewDNS([]string{serveDNS(t, true)}, time.Second, true)
	res, err := d.Resolve(context.Background(), "site.test", IPv4)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Addrs) != 1 {
		t.Fatalf("addresses = %v, want the one only TCP carried", res.Addrs)
	}
}
