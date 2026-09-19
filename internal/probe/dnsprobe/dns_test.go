package dnsprobe

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

type zone struct {
	rcode   int
	answers []string // "A 1.2.3.4" style, rendered under the queried name
}

func serve(t *testing.T, z zone) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Rcode = z.rcode
		for _, a := range z.answers {
			rr, err := dns.NewRR(m.Question[0].Name + " 300 IN " + a)
			if err != nil {
				t.Errorf("bad record %q: %v", a, err)
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

func newProber(t *testing.T, options string) probe.Prober {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "dns"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func run(t *testing.T, p probe.Prober) (probe.Result, []metrics.Sample) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec := metrics.NewRecorder(metrics.Labels{})
	req := probe.Request{
		Target:  probe.Target{Name: "example.test", Host: "example.test"},
		Buckets: metrics.Buckets{0.01, 0.1},
	}
	return p.Probe(ctx, req, rec), rec.Samples()
}

func gauge(samples []metrics.Sample, name string) (float64, bool) {
	for _, s := range samples {
		if s.Name == name {
			if g, ok := s.Value.(metrics.Gauge); ok {
				return float64(g), true
			}
		}
	}
	return 0, false
}

func TestExpectedAnswerSetIsOrderInsensitive(t *testing.T) {
	addr := serve(t, zone{answers: []string{"A 1.1.1.1", "A 1.1.1.2"}})
	p := newProber(t, `
servers: ["`+addr+`"]
expect: ["1.1.1.2", "1.1.1.1"]
`)
	res, samples := run(t, p)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if n, _ := gauge(samples, "dns_answers"); n != 2 {
		t.Fatalf("dns_answers = %v, want 2", n)
	}
}

func TestMissingAnswerIsReportedByName(t *testing.T) {
	addr := serve(t, zone{answers: []string{"A 1.1.1.1"}})
	p := newProber(t, `
servers: ["`+addr+`"]
expect: ["1.1.1.1", "1.1.1.2"]
`)
	res, _ := run(t, p)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "1.1.1.2") {
		t.Fatalf("want the missing address named, got %v", res.Err)
	}
}

func TestAcceptedNxdomainDoesNotTripMinAnswers(t *testing.T) {
	addr := serve(t, zone{rcode: dns.RcodeNameError})
	p := newProber(t, `
servers: ["`+addr+`"]
valid_rcodes: ["NXDOMAIN"]
`)
	res, _ := run(t, p)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
}

func TestUnexpectedRcodeFails(t *testing.T) {
	addr := serve(t, zone{rcode: dns.RcodeServerFailure})
	p := newProber(t, `servers: ["`+addr+`"]`)
	res, _ := run(t, p)
	if res.Err == nil {
		t.Fatal("want failure on SERVFAIL")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonDNS {
		t.Fatalf("reason = %q, want %q", got, probe.ReasonDNS)
	}
}

func TestResolverDisagreementIsCaught(t *testing.T) {
	a := serve(t, zone{answers: []string{"A 1.1.1.1"}})
	b := serve(t, zone{answers: []string{"A 9.9.9.9"}})
	p := newProber(t, `
servers: ["`+a+`", "`+b+`"]
compare_servers: true
`)
	res, samples := run(t, p)
	if res.Err == nil {
		t.Fatal("want failure when resolvers disagree")
	}
	if c, _ := gauge(samples, "dns_consistent"); c != 0 {
		t.Fatalf("dns_consistent = %v, want 0", c)
	}
}

func TestAgreeingResolversPass(t *testing.T) {
	a := serve(t, zone{answers: []string{"A 1.1.1.1", "A 1.1.1.2"}})
	b := serve(t, zone{answers: []string{"A 1.1.1.2", "A 1.1.1.1"}})
	p := newProber(t, `
servers: ["`+a+`", "`+b+`"]
compare_servers: true
`)
	res, samples := run(t, p)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if c, _ := gauge(samples, "dns_consistent"); c != 1 {
		t.Fatalf("dns_consistent = %v, want 1", c)
	}
}

func TestAnswerRegexMatchesAnyRecord(t *testing.T) {
	addr := serve(t, zone{answers: []string{"A 10.0.0.7", "A 192.168.1.4"}})
	p := newProber(t, `
servers: ["`+addr+`"]
answer_regex: "^192\\.168\\."
`)
	if res, _ := run(t, p); res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
}

func TestForbiddenAnswerRegexFailsOnAnyRecord(t *testing.T) {
	addr := serve(t, zone{answers: []string{"A 10.0.0.7", "A 192.168.1.4"}})
	p := newProber(t, `
servers: ["`+addr+`"]
answer_not_regex: "^192\\.168\\."
`)
	res, _ := run(t, p)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "192.168.1.4") {
		t.Fatalf("want the forbidden record named, got %v", res.Err)
	}
}

func TestCnameIsNotCountedAsAnAnswer(t *testing.T) {
	addr := serve(t, zone{answers: []string{"CNAME elsewhere.test."}})
	p := newProber(t, `servers: ["`+addr+`"]`)
	res, _ := run(t, p)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "0 answer") {
		t.Fatalf("want a zero-answer failure, got %v", res.Err)
	}
}

func TestUnknownQueryTypeIsRejectedAtBuild(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("query_type: NOPE"), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "dns", Options: *node.Content[0]}
	if _, err := New(pc); err == nil {
		t.Fatal("want an error for an unknown record type")
	}
}

func TestTruncatedAnswersAreRetriedOverTCP(t *testing.T) {
	var records []string
	for i := 0; i < 60; i++ {
		records = append(records, fmt.Sprintf("A 10.0.%d.%d", i/256, i%256))
	}
	addr := serveTruncating(t, records)

	p := newProber(t, `
servers: ["`+addr+`"]
min_answers: 60
`)
	res, samples := run(t, p)
	if res.Err != nil {
		t.Fatalf("a full zone was reported as short: %v", res.Err)
	}
	if n, _ := gauge(samples, "dns_answers"); n != 60 {
		t.Fatalf("dns_answers = %v, want 60", n)
	}
}

// serveTruncating answers UDP with a truncated message and TCP with everything.
func serveTruncating(t *testing.T, records []string) string {
	t.Helper()
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		for _, a := range records {
			rr, err := dns.NewRR(m.Question[0].Name + " 300 IN " + a)
			if err != nil {
				t.Errorf("bad record %q: %v", a, err)
				continue
			}
			resp.Answer = append(resp.Answer, rr)
		}
		if _, ok := w.RemoteAddr().(*net.UDPAddr); ok {
			resp.Answer = resp.Answer[:10]
			resp.Truncated = true
		}
		_ = w.WriteMsg(resp)
	})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	udp := &dns.Server{PacketConn: pc, Handler: handler}
	tcp := &dns.Server{Listener: ln, Handler: handler}
	go func() { _ = udp.ActivateAndServe() }()
	go func() { _ = tcp.ActivateAndServe() }()
	t.Cleanup(func() { _ = udp.Shutdown(); _ = tcp.Shutdown() })
	return pc.LocalAddr().String()
}

func TestCompareServersFailsWhenAResolverIsSilent(t *testing.T) {
	alive := serve(t, zone{answers: []string{"A 1.1.1.1"}})
	dead := deadResolver(t)

	p := newProber(t, `
servers: ["`+alive+`", "`+dead+`"]
compare_servers: true
`)
	res, samples := run(t, p)
	if res.Err == nil {
		t.Fatal("a comparison against a silent resolver was reported as agreement")
	}
	if c, ok := gauge(samples, "dns_consistent"); ok && c == 1 {
		t.Error("dns_consistent = 1 while a resolver was not answering")
	}
}

func deadResolver(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

// serveFull answers with all three sections, the AA flag and an SOA.
func serveFull(t *testing.T, z zone, authoritative bool, soa string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Authoritative = authoritative
		resp.Rcode = z.rcode
		for _, a := range z.answers {
			rr, err := dns.NewRR(m.Question[0].Name + " 300 IN " + a)
			if err != nil {
				t.Errorf("bad record %q: %v", a, err)
				continue
			}
			resp.Answer = append(resp.Answer, rr)
		}
		if soa != "" {
			rr, err := dns.NewRR(m.Question[0].Name + " 300 IN SOA " + soa)
			if err != nil {
				t.Errorf("bad SOA %q: %v", soa, err)
			} else {
				resp.Ns = append(resp.Ns, rr)
			}
		}
		ns, _ := dns.NewRR(m.Question[0].Name + " 300 IN NS ns1.example.test.")
		resp.Ns = append(resp.Ns, ns)
		glue, _ := dns.NewRR("ns1.example.test. 300 IN A 10.9.8.7")
		resp.Extra = append(resp.Extra, glue)
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

func TestSOASerialIsReported(t *testing.T) {
	addr := serveFull(t, zone{answers: []string{"A 1.1.1.1"}}, true,
		"ns1.example.test. hostmaster.example.test. 2026091901 7200 3600 1209600 300")
	p := newProber(t, `servers: ["`+addr+`"]`)

	res, samples := run(t, p)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if got, ok := gauge(samples, "dns_soa_serial"); !ok || got != 2026091901 {
		t.Fatalf("dns_soa_serial = %v (present=%v), want 2026091901", got, ok)
	}
}

// The AA flag separates an answer from the zone from one out of a cache.
func TestAuthoritativeFlagIsCheckedAndReported(t *testing.T) {
	cached := serveFull(t, zone{answers: []string{"A 1.1.1.1"}}, false, "")
	p := newProber(t, `
servers: ["`+cached+`"]
require_authoritative: true
`)
	res, samples := run(t, p)
	if res.Err == nil {
		t.Fatal("a cached answer passed a check that requires the authoritative flag")
	}
	if got, _ := gauge(samples, "dns_authoritative"); got != 0 {
		t.Errorf("dns_authoritative = %v, want 0", got)
	}

	authoritative := serveFull(t, zone{answers: []string{"A 1.1.1.1"}}, true, "")
	p = newProber(t, `
servers: ["`+authoritative+`"]
require_authoritative: true
`)
	res, samples = run(t, p)
	if res.Err != nil {
		t.Fatalf("an authoritative answer failed: %v", res.Err)
	}
	if got, _ := gauge(samples, "dns_authoritative"); got != 1 {
		t.Errorf("dns_authoritative = %v, want 1", got)
	}
}

// The NS set behind a delegation never reaches the answer section.
func TestAuthoritySectionIsChecked(t *testing.T) {
	addr := serveFull(t, zone{answers: []string{"A 1.1.1.1"}}, true, "")
	samplesOf := func(options string) (probe.Result, []metrics.Sample) {
		return run(t, newProber(t, options))
	}

	res, samples := samplesOf(`
servers: ["` + addr + `"]
authority_regex: "ns1\\.example\\.test"
`)
	if res.Err != nil {
		t.Fatalf("the expected nameserver was not found: %v", res.Err)
	}
	if got, ok := gauge(samples, "dns_authority_rrs"); !ok || got < 1 {
		t.Errorf("dns_authority_rrs = %v, want at least 1", got)
	}

	res, _ = samplesOf(`
servers: ["` + addr + `"]
authority_regex: "ns9\\.elsewhere\\.test"
`)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "authority") {
		t.Fatalf("a missing nameserver passed: %v", res.Err)
	}
}

// Glue records live in the additional section.
func TestAdditionalSectionIsChecked(t *testing.T) {
	addr := serveFull(t, zone{answers: []string{"A 1.1.1.1"}}, true, "")

	res, samples := run(t, newProber(t, `
servers: ["`+addr+`"]
additional_regex: "10\\.9\\.8\\.7"
`))
	if res.Err != nil {
		t.Fatalf("the glue record was not found: %v", res.Err)
	}
	if got, ok := gauge(samples, "dns_additional_rrs"); !ok || got != 1 {
		t.Errorf("dns_additional_rrs = %v, want 1", got)
	}

	res, _ = run(t, newProber(t, `
servers: ["`+addr+`"]
additional_not_regex: "10\\.9\\."
`))
	if res.Err == nil || !strings.Contains(res.Err.Error(), "additional") {
		t.Fatalf("a forbidden glue record passed: %v", res.Err)
	}
}

// The EDNS0 OPT pseudo-record is this prober's own question coming back.
func TestOwnEDNS0RecordIsNotCountedAsAnAnswer(t *testing.T) {
	addr := serve(t, zone{answers: []string{"A 1.1.1.1"}})
	_, samples := run(t, newProber(t, `servers: ["`+addr+`"]`))
	if got, ok := gauge(samples, "dns_additional_rrs"); ok && got != 0 {
		t.Fatalf("dns_additional_rrs = %v, want 0 for a zone with no additional records", got)
	}
}

func TestAnswerInfoCarriesTheWholeSet(t *testing.T) {
	server := serve(t, zone{answers: []string{"A 10.0.0.2", "A 10.0.0.1"}})
	res, samples := run(t, newProber(t, "servers: ["+server+"]"))
	if !res.OK() {
		t.Fatal(res.Err)
	}
	var infos []string
	for _, s := range samples {
		if s.Name == "dns_answer_info" {
			infos = append(infos, string(s.Value.(metrics.Info)))
		}
	}
	if len(infos) != 1 || infos[0] != "10.0.0.1, 10.0.0.2" {
		t.Fatalf("dns_answer_info = %q, want one series naming both records", infos)
	}
	// Written when it is zero as well, so the gauge falls back after a recovery.
	if failed, ok := gauge(samples, "dns_resolvers_failed"); !ok || failed != 0 {
		t.Fatalf("dns_resolvers_failed = %v (present %v), want 0", failed, ok)
	}
}
