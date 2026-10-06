// Package domain checks how long a domain's registration has left, over RDAP
// or, for registries that run none, WHOIS.
package domain

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func init() { probe.Register("domain", New) }

var tLookup = metrics.NewTiming("domain_lookup")

const (
	// rdap.org redirects to whichever registry is authoritative for the TLD.
	defaultServer = "https://rdap.org"
	maxBody       = 1 << 20
	day           = 24 * time.Hour
)

// defaultWHOIS maps public suffixes missing from the IANA RDAP bootstrap to their WHOIS server.
var defaultWHOIS = map[string]string{
	"ru":       "whois.tcinet.ru",
	"su":       "whois.tcinet.ru",
	"xn--p1ai": "whois.tcinet.ru", // .рф
}

// Config is the `domain:` block.
type Config struct {
	// Server is the RDAP endpoint.
	Server string `yaml:"server"`
	// WHOIS maps a public suffix to a WHOIS server asked instead of RDAP, on top of
	// the built-in table; an empty address sends that suffix back to RDAP.
	WHOIS map[string]string `yaml:"whois"`
	// Refresh is how long an answer is reused before the registry is asked again.
	Refresh config.Duration `yaml:"refresh"`
	// MinDays fails the check when the registration has less left; zero only reports.
	MinDays       float64  `yaml:"min_days"`
	Registrar     string   `yaml:"registrar"`
	RequireStatus []string `yaml:"require_status"`
	ForbidStatus  []string `yaml:"forbid_status"`
}

// record is a registration as any source reports it; zero times are unknown.
type record struct {
	expiry, registered time.Time
	registrar          string
	status             []string
	// lockable says the statuses are EPP codes, where a missing transfer lock means something.
	lockable bool
}

// source looks a registrable name up in a registry; String names the registry.
type source interface {
	lookup(ctx context.Context, name string) (*record, error)
	fmt.Stringer
}

type Prober struct {
	cfg   Config
	rdap  source
	whois map[string]source // by public suffix
	cache *cache
}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{Server: defaultServer, Refresh: config.Duration(defaultRefresh)}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if cfg.MinDays < 0 {
		return nil, fmt.Errorf("domain.min_days cannot be negative")
	}
	if cfg.Refresh <= 0 {
		return nil, fmt.Errorf("domain.refresh must be positive")
	}
	if _, err := url.Parse(cfg.Server); err != nil {
		return nil, fmt.Errorf("domain.server: %w", err)
	}

	servers := maps.Clone(defaultWHOIS)
	for suffix, addr := range cfg.WHOIS {
		key, err := toASCII(strings.Trim(strings.ToLower(suffix), "."))
		if err != nil || key == "" {
			return nil, fmt.Errorf("domain.whois: %q is not a domain suffix", suffix)
		}
		servers[key] = addr
	}
	whois := make(map[string]source, len(servers))
	for suffix, addr := range servers {
		if addr == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, whoisPort)
		}
		whois[suffix] = &whoisServer{addr: addr}
	}

	return &Prober{
		cfg:   cfg,
		rdap:  &rdapServer{url: cfg.Server, client: &http.Client{}},
		whois: whois,
		cache: answers,
	}, nil
}

// SelfAddressed reports that the runner must not resolve the target.
func (p *Prober) SelfAddressed() bool { return true }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	name, err := registrable(req.Target)
	if err != nil {
		res.Err = err
		return res
	}

	src := p.sourceFor(name)
	start := time.Now()
	a, fetched := p.cache.get(ctx, src.String()+" "+name,
		func(ctx context.Context) (*record, error) { return src.lookup(ctx, name) }, p.ttl)
	if fetched {
		// A cached answer is not a lookup: timing it would measure the map.
		res.Add("lookup", time.Since(start))
		rec.Duration(tLookup, req.Buckets, time.Since(start))
	}
	if a.err != nil {
		res.Err = a.err
		return res
	}

	rec.Gauge("domain_fetched_seconds", float64(a.fetched.Unix()))
	res.Err = p.report(a.rec, name, rec)
	return res
}

// ttl believes a record for refresh, but one close to failing min_days only for
// retryAfter, so that a renewal clears the alert; nil is a failure.
func (p *Prober) ttl(r *record) time.Duration {
	short := min(p.cfg.Refresh.D(), retryAfter)
	if r == nil {
		return short
	}
	watch := max(time.Duration(p.cfg.MinDays*float64(day)), 7*day)
	if !r.expiry.IsZero() && time.Until(r.expiry) < watch {
		return short
	}
	return p.cfg.Refresh.D()
}

// sourceFor picks the WHOIS server mapped to the name's public suffix, RDAP otherwise.
func (p *Prober) sourceFor(name string) source {
	_, suffix, _ := strings.Cut(name, ".")
	if s, ok := p.whois[suffix]; ok {
		return s
	}
	return p.rdap
}

// registrable reduces a target to the registered name under its public suffix, in ASCII.
func registrable(t probe.Target) (string, error) {
	host := t.Host
	if host == "" && t.URL != nil {
		host = t.URL.Hostname()
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return "", probe.Fail(probe.ReasonInternal, "target %q has no domain name", t.Name)
	}
	host, err := toASCII(host)
	if err != nil {
		return "", probe.Fail(probe.ReasonInternal, "%s is not a domain name: %v", t.Name, err)
	}
	name, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		// A bare public suffix is not itself a registration.
		return "", probe.Fail(probe.ReasonInternal, "%s is not a registrable domain: %v", host, err)
	}
	return name, nil
}

// toASCII converts an internationalised name to punycode and leaves ASCII alone:
// IDNA rules reject names such as _dmarc.example.com that DNS serves fine.
func toASCII(name string) (string, error) {
	for _, r := range name {
		if r >= utf8.RuneSelf {
			return idna.Lookup.ToASCII(name)
		}
	}
	return name, nil
}

// report records every metric, then decides whether the registration is acceptable.
func (p *Prober) report(r *record, name string, rec *metrics.Recorder) error {
	now := time.Now()
	if !r.expiry.IsZero() {
		rec.Gauge("domain_expiry_seconds", float64(r.expiry.Unix()))
		rec.Gauge("domain_expiry_days", r.expiry.Sub(now).Hours()/24)
	}
	if !r.registered.IsZero() {
		rec.Gauge("domain_registered_seconds", float64(r.registered.Unix()))
		rec.Gauge("domain_age_days", now.Sub(r.registered).Hours()/24)
	}
	if r.registrar != "" {
		rec.Info("domain_registrar_info", r.registrar)
	}
	if len(r.status) > 0 {
		rec.Info("domain_status_info", strings.Join(r.status, ","))
	}
	if r.lockable {
		rec.Gauge("domain_locked", metrics.Bool(has(r.status, "clienttransferprohibited")))
	}

	if r.expiry.IsZero() {
		// Some ccTLD registries publish no expiry; zero days would be a fiction.
		return probe.Fail(probe.ReasonContent, "%s: the registry published no expiry date", name)
	}
	if left := r.expiry.Sub(now); left <= 0 {
		return probe.Fail(probe.ReasonContent, "%s expired %s ago, on %s",
			name, round(-left), r.expiry.Format(time.DateOnly))
	} else if p.cfg.MinDays > 0 && left < time.Duration(p.cfg.MinDays*float64(day)) {
		return probe.Fail(probe.ReasonContent, "%s expires in %s, on %s; want at least %g days",
			name, round(left), r.expiry.Format(time.DateOnly), p.cfg.MinDays)
	}
	if p.cfg.Registrar != "" && !strings.EqualFold(r.registrar, p.cfg.Registrar) {
		return probe.Fail(probe.ReasonContent, "%s is registered with %q, want %q",
			name, r.registrar, p.cfg.Registrar)
	}
	for _, want := range p.cfg.RequireStatus {
		if !has(r.status, want) {
			return probe.Fail(probe.ReasonContent, "%s does not carry status %q; it has %s",
				name, want, strings.Join(r.status, ", "))
		}
	}
	for _, bad := range p.cfg.ForbidStatus {
		if has(r.status, bad) {
			return probe.Fail(probe.ReasonContent, "%s carries status %q", name, bad)
		}
	}
	return nil
}

// parseTime reads the dates registries emit: RFC 3339, the same without a zone, or a bare date.
func parseTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", time.DateTime, time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// has matches a status across both spellings in use: RFC 9083 registers them
// with spaces ("client transfer prohibited"), EPP uses camelCase.
func has(list []string, want string) bool {
	want = foldStatus(want)
	return slices.ContainsFunc(list, func(s string) bool { return foldStatus(s) == want })
}

func foldStatus(s string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(s))
}

// round renders a duration as whole days once it exceeds one.
func round(d time.Duration) string {
	if d < day {
		return d.Round(time.Hour).String()
	}
	return fmt.Sprintf("%d days", int(math.Round(d.Hours()/24)))
}
