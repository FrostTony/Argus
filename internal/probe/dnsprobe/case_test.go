package dnsprobe

import (
	"net"
	"testing"

	"github.com/miekg/dns"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// DKIM keys and other TXT payloads are case-sensitive.
func TestAnswersKeepTheirCase(t *testing.T) {
	const txt = `"v=DKIM1; k=rsa; p=MIIBAbC"`
	server := serve(t, zone{answers: []string{"TXT " + txt}})
	res, samples := run(t, newProber(t, `
servers: ["`+server+`"]
query_type: TXT
answer_regex: "p=MIIBAbC"
answer_not_regex: "p=miibabc"
`))
	if !res.OK() {
		t.Fatal(res.Err)
	}
	for _, s := range samples {
		if s.Name == "dns_answer_info" && string(s.Value.(metrics.Info)) != txt {
			t.Fatalf("dns_answer_info = %q, want %q", s.Value, txt)
		}
	}
}

// Names compare case-insensitively, between resolvers and against expect.
func TestSetComparisonsIgnoreCase(t *testing.T) {
	upper := serve(t, zone{answers: []string{"CNAME Target.Example.COM."}})
	lower := serve(t, zone{answers: []string{"CNAME target.example.com."}})
	res, samples := run(t, newProber(t, `
servers: ["`+upper+`", "`+lower+`"]
query_type: CNAME
compare_servers: true
expect: ["TARGET.example.com"]
`))
	if !res.OK() {
		t.Fatal(res.Err)
	}
	if v, _ := gauge(samples, "dns_consistent"); v != 1 {
		t.Fatalf("dns_consistent = %v, want 1", v)
	}
}

func TestZeroTTLIsTheMinimum(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		for _, rr := range []string{" 300 IN A 1.1.1.2", " 0 IN A 1.1.1.1"} {
			a, _ := dns.NewRR(m.Question[0].Name + rr)
			resp.Answer = append(resp.Answer, a)
		}
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	res, samples := run(t, newProber(t, `servers: ["`+pc.LocalAddr().String()+`"]`))
	if !res.OK() {
		t.Fatal(res.Err)
	}
	if v, ok := gauge(samples, "dns_ttl_seconds"); !ok || v != 0 {
		t.Fatalf("dns_ttl_seconds = %v (present %v), want 0", v, ok)
	}
}
