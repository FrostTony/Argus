// Package domain checks how long a domain's registration has left, over RDAP.
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

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

// Config is the `domain:` block.
type Config struct {
	// Server is the RDAP endpoint.
	Server string `yaml:"server"`
	// MinDays fails the check when the registration has less left; zero only reports.
	MinDays       float64  `yaml:"min_days"`
	Registrar     string   `yaml:"registrar"`
	RequireStatus []string `yaml:"require_status"`
	ForbidStatus  []string `yaml:"forbid_status"`
}

type Prober struct {
	cfg    Config
	client *http.Client
}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{Server: defaultServer}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if cfg.MinDays < 0 {
		return nil, fmt.Errorf("domain.min_days cannot be negative")
	}
	if _, err := url.Parse(cfg.Server); err != nil {
		return nil, fmt.Errorf("domain.server: %w", err)
	}
	return &Prober{cfg: cfg, client: &http.Client{}}, nil
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

	start := time.Now()
	doc, err := p.lookup(ctx, name)
	res.Add("lookup", time.Since(start))
	rec.Duration(tLookup, req.Buckets, time.Since(start))
	if err != nil {
		res.Err = err
		return res
	}

	res.Err = p.report(doc, name, rec)
	return res
}

// registrable reduces a target to the registered name under its public suffix.
func registrable(t probe.Target) (string, error) {
	host := t.Host
	if host == "" && t.URL != nil {
		host = t.URL.Hostname()
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return "", probe.Fail(probe.ReasonInternal, "target %q has no domain name", t.Name)
	}
	name, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		// A bare public suffix is not itself a registration.
		return "", probe.Fail(probe.ReasonInternal, "%s is not a registrable domain: %v", host, err)
	}
	return name, nil
}

// rdapDomain is the part of an RFC 9083 domain object this check needs.
type rdapDomain struct {
	LDHName string `json:"ldhName"`
	Status  []string
	Events  []struct {
		Action string `json:"eventAction"`
		Date   string `json:"eventDate"`
	}
	Entities []struct {
		Roles []string `json:"roles"`
		// vCard is nested untyped arrays, so it is walked rather than decoded.
		VCard []any `json:"vcardArray"`
	}
}

func (p *Prober) lookup(ctx context.Context, name string) (*rdapDomain, error) {
	endpoint := strings.TrimSuffix(p.cfg.Server, "/") + "/domain/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, probe.Fail(probe.ReasonInternal, "%v", err)
	}
	req.Header.Set("Accept", "application/rdap+json")

	resp, err := p.client.Do(req)
	if err != nil {
		// Wrapped so the deadline stays in the chain and classifies as a timeout.
		return nil, probe.Wrap(probe.ReasonConnect, fmt.Errorf("rdap %s: %w", endpoint, unwrapURL(err)))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// 404 from a registry means the name is not registered, not a missing page.
		return nil, probe.Fail(probe.ReasonContent, "%s is not registered", name)
	}
	if resp.StatusCode/100 != 2 {
		return nil, probe.Fail(probe.ReasonStatus, "rdap %s: got %d", endpoint, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, probe.Wrap(probe.ReasonProtocol, err)
	}
	var doc rdapDomain
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, probe.Fail(probe.ReasonProtocol, "rdap %s: %v", endpoint, err)
	}
	return &doc, nil
}

// report records every metric, then decides whether the registration is acceptable.
func (p *Prober) report(doc *rdapDomain, name string, rec *metrics.Recorder) error {
	now := time.Now()
	expiry, hasExpiry := doc.event("expiration")
	registered, hasRegistered := doc.event("registration")

	if hasExpiry {
		rec.Gauge("domain_expiry_seconds", float64(expiry.Unix()))
		rec.Gauge("domain_expiry_days", expiry.Sub(now).Hours()/24)
	}
	if hasRegistered {
		rec.Gauge("domain_registered_seconds", float64(registered.Unix()))
		rec.Gauge("domain_age_days", now.Sub(registered).Hours()/24)
	}
	if registrar := doc.registrar(); registrar != "" {
		rec.Info("domain_registrar_info", registrar)
	}
	if len(doc.Status) > 0 {
		rec.Info("domain_status_info", strings.Join(doc.Status, ","))
	}
	rec.Gauge("domain_locked", boolValue(has(doc.Status, "clienttransferprohibited")))

	if !hasExpiry {
		// Some ccTLD registries publish no expiry; zero days would be a fiction.
		return probe.Fail(probe.ReasonContent, "%s: the registry published no expiry date", name)
	}
	if left := expiry.Sub(now); left <= 0 {
		return probe.Fail(probe.ReasonContent, "%s expired %s ago, on %s",
			name, round(-left), expiry.Format(time.DateOnly))
	} else if p.cfg.MinDays > 0 && left < time.Duration(p.cfg.MinDays*float64(day)) {
		return probe.Fail(probe.ReasonContent, "%s expires in %s, on %s; want at least %g days",
			name, round(left), expiry.Format(time.DateOnly), p.cfg.MinDays)
	}
	if p.cfg.Registrar != "" && !strings.EqualFold(doc.registrar(), p.cfg.Registrar) {
		return probe.Fail(probe.ReasonContent, "%s is registered with %q, want %q",
			name, doc.registrar(), p.cfg.Registrar)
	}
	for _, want := range p.cfg.RequireStatus {
		if !has(doc.Status, want) {
			return probe.Fail(probe.ReasonContent, "%s does not carry status %q; it has %s",
				name, want, strings.Join(doc.Status, ", "))
		}
	}
	for _, bad := range p.cfg.ForbidStatus {
		if has(doc.Status, bad) {
			return probe.Fail(probe.ReasonContent, "%s carries status %q", name, bad)
		}
	}
	return nil
}

// event finds a date by its action, matched case-insensitively.
func (d *rdapDomain) event(action string) (time.Time, bool) {
	for _, e := range d.Events {
		if !strings.EqualFold(e.Action, action) {
			continue
		}
		// RFC 9083 says RFC 3339; registries also emit it without a zone.
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", time.DateOnly} {
			if t, err := time.Parse(layout, e.Date); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// registrar reads the "fn" property of the registrar entity's vCard:
// ["vcard", [["version", {}, "text", "4.0"], ["fn", {}, "text", "Example Inc"]]].
func (d *rdapDomain) registrar() string {
	for _, e := range d.Entities {
		if !has(e.Roles, "registrar") || len(e.VCard) < 2 {
			continue
		}
		props, ok := e.VCard[1].([]any)
		if !ok {
			continue
		}
		for _, p := range props {
			field, ok := p.([]any)
			if !ok || len(field) < 4 {
				continue
			}
			if key, ok := field[0].(string); !ok || key != "fn" {
				continue
			}
			if value, ok := field[3].(string); ok {
				return value
			}
		}
	}
	return ""
}

// unwrapURL drops the "Get \"...\":" prefix url.Error prints.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
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

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// round renders a duration as whole days once it exceeds one.
func round(d time.Duration) string {
	if d < day {
		return d.Round(time.Hour).String()
	}
	return fmt.Sprintf("%d days", int(math.Round(d.Hours()/24)))
}
