package resolve

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// serveZone answers every question with what answer returns for its type: an
// rcode, or records written without the owner name.
func serveZone(t *testing.T, answer func(qtype uint16) (rcode int, records []string)) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		q := m.Question[0]
		rcode, records := answer(q.Qtype)
		resp.Rcode = rcode
		for _, r := range records {
			rr, err := dns.NewRR(q.Name + " " + r)
			if err != nil {
				t.Error(err)
				continue
			}
			resp.Answer = append(resp.Answer, rr)
		}
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

// failingA fails every A question and answers AAAA.
func failingA(qtype uint16) (int, []string) {
	if qtype == dns.TypeA {
		return dns.RcodeServerFailure, nil
	}
	return dns.RcodeSuccess, []string{"3600 IN AAAA fd00::1"}
}

func healthy(qtype uint16) (int, []string) {
	if qtype == dns.TypeA {
		return dns.RcodeSuccess, []string{"60 IN A 10.0.0.1"}
	}
	return dns.RcodeSuccess, []string{"3600 IN AAAA fd00::1"}
}

func TestDNSHalfAnswerMovesOnToTheNextServer(t *testing.T) {
	bad, good := serveZone(t, failingA), serveZone(t, healthy)
	d := NewDNS([]string{bad, good}, time.Second, true)
	res, err := d.Resolve(context.Background(), "site.test", Both)
	if err != nil {
		t.Fatal(err)
	}
	if res.Server != good || len(res.Addrs) != 2 {
		t.Fatalf("result = %+v, want both families from %s", res, good)
	}
}

func TestDNSHalfAnswerEverywhereIsAFailure(t *testing.T) {
	d := NewDNS([]string{serveZone(t, failingA)}, time.Second, true)
	c := NewCache(d, 0, 0, 0)
	res, err := c.Resolve(context.Background(), "site.test", Both)
	if err == nil {
		t.Fatalf("result = %+v: a lost A answer passed for the whole one", res)
	}
	if want := []netip.Addr{netip.MustParseAddr("fd00::1")}; !slices.Equal(res.Addrs, want) {
		t.Fatalf("addresses = %v, want the partial answer %v", res.Addrs, want)
	}
	if _, err := c.Resolve(context.Background(), "site.test", Both); err == nil {
		t.Fatal("the cache serves the half answer as a success")
	}
}

func TestDNSTTLCoversTheWholeChain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []string
		want    time.Duration
	}{
		{"cname", []string{"30 IN CNAME edge.test.", "3600 IN A 10.0.0.1"}, 30 * time.Second},
		{"zero", []string{"0 IN A 10.0.0.1", "60 IN A 10.0.0.2"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := serveZone(t, func(uint16) (int, []string) { return dns.RcodeSuccess, tc.records })
			res, err := NewDNS([]string{addr}, time.Second, true).Resolve(context.Background(), "site.test", IPv4)
			if err != nil {
				t.Fatal(err)
			}
			if res.TTL != tc.want {
				t.Fatalf("ttl = %s, want %s", res.TTL, tc.want)
			}
		})
	}
}
