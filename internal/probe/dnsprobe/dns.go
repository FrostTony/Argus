// Package dnsprobe checks that a zone answers, and answers the same everywhere.
// The target is the domain itself and the fan-out is across resolvers.
package dnsprobe

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func init() { probe.Register("dns", New) }

var tDNS = metrics.NewTiming("dns")

// edns0Size is the answer size advertised over UDP via EDNS0.
const edns0Size = 4096

// Config is the `dns:` block.
type Config struct {
	// Servers to query; empty means the system resolvers.
	Servers []string `yaml:"servers"`
	// QueryName is what to ask for; empty means the target's own name.
	QueryName string `yaml:"query_name"`
	QueryType string `yaml:"query_type"`
	// Proto is udp, tcp, tls (DoT) or https (DoH); dot and doh are accepted too.
	Proto string `yaml:"proto"`
	// TLSOptions verifies the resolver, for the two protocols that present a
	// certificate.
	TLSOptions tlsinfo.Options `yaml:"tls_config"`
	MinAnswers int             `yaml:"min_answers"`
	// Expect is the answer set, order-insensitive.
	Expect []string `yaml:"expect"`
	// ValidRcodes accepts rcodes other than NOERROR, such as an expected NXDOMAIN.
	ValidRcodes    []string `yaml:"valid_rcodes"`
	AnswerRegex    string   `yaml:"answer_regex"`
	AnswerNotRegex string   `yaml:"answer_not_regex"`
	// RequireAuthoritative fails unless the resolver set the AA flag.
	RequireAuthoritative *bool `yaml:"require_authoritative"`
	// AuthorityRegex and AdditionalRegex match the sections holding NS, SOA and glue.
	AuthorityRegex     string `yaml:"authority_regex"`
	AuthorityNotRegex  string `yaml:"authority_not_regex"`
	AdditionalRegex    string `yaml:"additional_regex"`
	AdditionalNotRegex string `yaml:"additional_not_regex"`
	// CompareServers requires every resolver to return the same set; not for a CDN.
	CompareServers bool  `yaml:"compare_servers"`
	Recursion      *bool `yaml:"recursion"`
}

type Prober struct {
	cfg         Config
	transport   *transport
	qtype       uint16
	qtypeName   string
	servers     []string
	expect      map[string]bool
	rcodes      map[string]bool
	answerRe    *regexp.Regexp
	answerNotRe *regexp.Regexp
	sections    []section
}

// section is one part of a DNS message and the two expressions asked of it.
type section struct {
	name   string
	of     func(a *answer) []string
	re     *regexp.Regexp
	notRe  *regexp.Regexp
	reSrc  string
	notSrc string
}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{QueryType: "A", Proto: "udp", MinAnswers: 1}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	qtype, ok := dns.StringToType[strings.ToUpper(cfg.QueryType)]
	if !ok {
		return nil, fmt.Errorf("dns.query_type: unknown record type %q", cfg.QueryType)
	}
	proto, ok := protoAliases[strings.ToLower(strings.TrimSpace(cfg.Proto))]
	if !ok {
		return nil, fmt.Errorf("dns.proto: want udp|tcp|tls|https, got %q", cfg.Proto)
	}
	cfg.Proto = proto
	tr, err := newTransport(proto, cfg.TLSOptions)
	if err != nil {
		return nil, err
	}

	servers := make([]string, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		normalized, err := tr.server(s)
		if err != nil {
			return nil, err
		}
		servers = append(servers, normalized)
	}
	if len(servers) == 0 {
		if proto == protoHTTPS {
			return nil, fmt.Errorf("dns.servers: DNS over HTTPS has no system default; name the endpoint")
		}
		sysServers, err := systemServers()
		if err != nil {
			return nil, err
		}
		servers = sysServers
	}

	expect := make(map[string]bool, len(cfg.Expect))
	for _, e := range cfg.Expect {
		expect[normalizeAnswer(e)] = true
	}

	rcodes := make(map[string]bool, len(cfg.ValidRcodes))
	for _, r := range cfg.ValidRcodes {
		name := strings.ToUpper(strings.TrimSpace(r))
		if _, ok := dns.StringToRcode[name]; !ok {
			return nil, fmt.Errorf("dns.valid_rcodes: unknown rcode %q", r)
		}
		rcodes[name] = true
	}
	if len(rcodes) == 0 {
		rcodes["NOERROR"] = true
	}

	p := &Prober{
		cfg:       cfg,
		transport: tr,
		qtype:     qtype,
		qtypeName: strings.ToUpper(cfg.QueryType),
		servers:   servers,
		expect:    expect,
		rcodes:    rcodes,
	}
	if p.answerRe, err = probe.Pattern(cfg.AnswerRegex); err != nil {
		return nil, fmt.Errorf("dns.answer_regex: %w", err)
	}
	if p.answerNotRe, err = probe.Pattern(cfg.AnswerNotRegex); err != nil {
		return nil, fmt.Errorf("dns.answer_not_regex: %w", err)
	}
	for _, sec := range []section{
		{name: "authority", of: func(a *answer) []string { return a.Authority },
			reSrc: cfg.AuthorityRegex, notSrc: cfg.AuthorityNotRegex},
		{name: "additional", of: func(a *answer) []string { return a.Additional },
			reSrc: cfg.AdditionalRegex, notSrc: cfg.AdditionalNotRegex},
	} {
		if sec.re, err = probe.Pattern(sec.reSrc); err != nil {
			return nil, fmt.Errorf("dns.%s_regex: %w", sec.name, err)
		}
		if sec.notRe, err = probe.Pattern(sec.notSrc); err != nil {
			return nil, fmt.Errorf("dns.%s_not_regex: %w", sec.name, err)
		}
		p.sections = append(p.sections, sec)
	}
	return p, nil
}

// SelfAddressed reports that the runner must not resolve the target; it is the question.
func (p *Prober) SelfAddressed() bool { return true }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	name := p.cfg.QueryName
	if name == "" {
		name = req.Target.Host
	}

	answersBy := make(map[string][]string, len(p.servers))
	replies := make(map[string]*answer, len(p.servers))
	var failures int
	// Kept so that a run where nobody answered can say why rather than only
	// how many. A classified failure (a revoked resolver certificate, say)
	// keeps its own reason through Wrap.
	var lastErr error

	for i, server := range p.servers {
		r := rec.With("resolver", server, "qtype", p.qtypeName)
		a, err := p.query(ctx, req, r, name, server, len(p.servers)-i)

		r.Count("dns_queries_total", 1)
		r.Info("dns_rcode_info", a.Rcode)
		if err != nil {
			r.Count("dns_query_failures_total", 1)
			failures++
			lastErr = err
			continue
		}
		// Timed only when there was an answer, so a dead resolver adds no latency.
		r.Duration(tDNS, req.Buckets, a.RTT)
		a.Timing.apply(&res)
		r.Gauge("dns_answers", float64(len(a.Records)))
		r.Gauge("dns_authority_rrs", float64(len(a.Authority)))
		r.Gauge("dns_additional_rrs", float64(len(a.Additional)))
		r.Gauge("dns_authoritative", metrics.Bool(a.Authoritative))
		if a.Serial > 0 {
			r.Gauge("dns_soa_serial", float64(a.Serial))
		}
		if a.TTL > 0 {
			r.Gauge("dns_ttl_seconds", a.TTL.Seconds())
		}
		// One series carries the whole set: per-record writes share these labels.
		r.Info("dns_answer_info", strings.Join(a.Records, ", "))
		answersBy[server] = a.Records
		replies[server] = a
	}

	if len(answersBy) == 0 {
		res.Err = probe.Wrap(probe.ReasonDNS,
			fmt.Errorf("none of %d resolver(s) answered: %w", len(p.servers), lastErr))
		return res
	}
	rec.Gauge("dns_resolvers_failed", float64(failures))

	if p.cfg.CompareServers {
		// Resolvers that did not answer can neither agree nor disagree.
		if failures > 0 {
			res.Err = probe.Fail(probe.ReasonDNS,
				"%d of %d resolver(s) did not answer, so the answers cannot be compared",
				failures, len(p.servers))
			return res
		}
		consistent := allEqual(answersBy)
		rec.Gauge("dns_consistent", metrics.Bool(consistent))
		if !consistent {
			res.Err = probe.Fail(probe.ReasonContent, "resolvers disagree: %s", describe(answersBy))
			return res
		}
	}

	for server, a := range replies {
		if config.Enabled(p.cfg.RequireAuthoritative, false) && !a.Authoritative {
			res.Err = probe.Fail(probe.ReasonContent,
				"%s answered without the authoritative flag: the answer came from a cache, not from the zone", server)
			return res
		}
		for _, sec := range p.sections {
			if err := sec.match(server, a); err != nil {
				res.Err = err
				return res
			}
		}

		answers := a.Records
		// An accepted NXDOMAIN or REFUSED carries no records to check.
		if a.Rcode != "NOERROR" {
			continue
		}
		if len(answers) < p.cfg.MinAnswers {
			res.Err = probe.Fail(probe.ReasonContent, "%s returned %d answer(s), want %d", server, len(answers), p.cfg.MinAnswers)
			return res
		}
		if len(p.expect) > 0 {
			if missing := p.missing(answers); missing != "" {
				res.Err = probe.Fail(probe.ReasonContent, "%s: answer is missing %s", server, missing)
				return res
			}
		}
		if err := p.matchAnswers(rec, server, answers); err != nil {
			res.Err = err
			return res
		}
	}
	return res
}

func (s section) match(server string, a *answer) error {
	records := s.of(a)
	if s.notRe != nil {
		for _, r := range records {
			if s.notRe.MatchString(r) {
				return probe.FailRegex(
					"%s: %s section record %q matches the forbidden %q", server, s.name, r, s.notSrc)
			}
		}
	}
	if s.re == nil {
		return nil
	}
	for _, r := range records {
		if s.re.MatchString(r) {
			return nil
		}
	}
	return probe.FailRegex(
		"%s: nothing in the %s section matches %q", server, s.name, s.reSrc)
}

func (p *Prober) matchAnswers(rec *metrics.Recorder, server string, answers []string) error {
	for _, a := range answers {
		if p.answerNotRe != nil && p.answerNotRe.MatchString(a) {
			return probe.FailRegex("%s: answer %q matches the forbidden %q", server, a, p.cfg.AnswerNotRegex)
		}
	}
	if p.answerRe == nil {
		return nil
	}
	named := probe.Named(p.answerRe)
	for _, a := range answers {
		if !named {
			if p.answerRe.MatchString(a) {
				return nil
			}
			continue
		}
		if m := p.answerRe.FindStringSubmatch(a); m != nil {
			probe.ExpectInfo(rec, p.answerRe, m, "server", server)
			return nil
		}
	}
	return probe.FailRegex("%s: no answer matches %q", server, p.cfg.AnswerRegex)
}

func (p *Prober) missing(answers []string) string {
	got := make(map[string]bool, len(answers))
	for _, a := range answers {
		got[normalizeAnswer(a)] = true
	}
	var absent []string
	for want := range p.expect {
		if !got[want] {
			absent = append(absent, want)
		}
	}
	sort.Strings(absent)
	return strings.Join(absent, ", ")
}

// answer is one resolver's reply, reduced to what the checks need.
type answer struct {
	// Records is the answer section filtered to the query type; a CNAME is a link.
	Records    []string
	Authority  []string
	Additional []string
	Rcode      string
	TTL        time.Duration
	RTT        time.Duration
	// Timing splits the round trip into connecting and answering.
	Timing timing
	// Authoritative is the AA flag: the zone answered, not a cache.
	Authoritative bool
	Serial        uint32
}

// query asks one resolver one question and normalises the reply.
func (p *Prober) query(ctx context.Context, req probe.Request, rec *metrics.Recorder, name, server string, serversLeft int) (*answer, error) {
	ctx, cancel := share(ctx, serversLeft)
	defer cancel()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), p.qtype)
	m.RecursionDesired = p.cfg.Recursion == nil || *p.cfg.Recursion
	if p.cfg.Proto == protoUDP {
		m.SetEdns0(edns0Size, false)
	}

	resp, span, err := p.transport.exchange(ctx, req, m, server, rec)
	if err != nil {
		return &answer{Rcode: "ERROR", RTT: span.total(), Timing: span}, err
	}
	if resp.Truncated && p.cfg.Proto == protoUDP {
		// A truncated answer is half a zone: retry over TCP.
		over := &transport{proto: protoTCP}
		full, tcpSpan, tcpErr := over.exchange(ctx, req, m, server, rec)
		span.connect += tcpSpan.connect
		span.query += tcpSpan.query
		if tcpErr != nil {
			return &answer{Rcode: "ERROR", RTT: span.total(), Timing: span},
				fmt.Errorf("%s: truncated over UDP and TCP failed: %w", server, tcpErr)
		}
		resp = full
	}

	out := &answer{
		Rcode:         dns.RcodeToString[resp.Rcode],
		RTT:           span.total(),
		Timing:        span,
		Authoritative: resp.Authoritative,
		Authority:     records(resp.Ns),
		Additional:    records(resp.Extra),
	}
	if !p.rcodes[out.Rcode] {
		return out, fmt.Errorf("%s: rcode %s", server, out.Rcode)
	}

	for _, rr := range resp.Answer {
		if rr.Header().Rrtype != p.qtype {
			continue // CNAME links are not part of the answer set
		}
		out.Records = append(out.Records, normalizeAnswer(rrValue(rr)))
		if t := time.Duration(rr.Header().Ttl) * time.Second; out.TTL == 0 || t < out.TTL {
			out.TTL = t
		}
	}
	sort.Strings(out.Records) // DNS rotates order; sets are what we compare
	out.Serial = serialOf(resp)
	return out, nil
}

// share gives one resolver its part of what is left of the run. The limit lives
// on the context because Client.Timeout does not reach a dialer of our own.
func share(ctx context.Context, serversLeft int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Until(deadline)/time.Duration(max(1, serversLeft)))
}

func records(rrs []dns.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		// OPT is the EDNS0 pseudo-record this prober added to the question.
		if rr.Header().Rrtype == dns.TypeOPT {
			continue
		}
		out = append(out, rrValue(rr))
	}
	sort.Strings(out)
	return out
}

// serialOf finds the SOA serial in whichever section carried it: the answer
// section for an SOA query, the authority section for a negative answer.
func serialOf(resp *dns.Msg) uint32 {
	for _, rrs := range [][]dns.RR{resp.Answer, resp.Ns} {
		for _, rr := range rrs {
			if soa, ok := rr.(*dns.SOA); ok {
				return soa.Serial
			}
		}
	}
	return 0
}

// rrValue is the record without its header, whose TTL changes as the answer ages.
func rrValue(rr dns.RR) string {
	return strings.TrimSpace(strings.TrimPrefix(rr.String(), rr.Header().String()))
}

func normalizeAnswer(s string) string {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().String()
	}
	return strings.ToLower(strings.TrimSuffix(s, "."))
}

func allEqual(m map[string][]string) bool {
	// An empty answer is a nil slice, so a separate flag tracks the first entry.
	var first string
	seen := false
	for _, v := range m {
		set := strings.Join(v, ",")
		if !seen {
			first, seen = set, true
			continue
		}
		if set != first {
			return false
		}
	}
	return true
}

func describe(m map[string][]string) string {
	parts := make([]string, 0, len(m))
	for s, a := range m {
		parts = append(parts, fmt.Sprintf("%s=[%s]", s, strings.Join(a, " ")))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// systemServers reads the resolvers from /etc/resolv.conf.
func systemServers() ([]string, error) {
	cfg, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("dns.servers is unset and the system resolvers could not be read: %v", err)
	}
	out := make([]string, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		out = append(out, net.JoinHostPort(s, cfg.Port))
	}
	return out, nil
}
