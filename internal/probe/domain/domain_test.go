package domain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// registry serves /domain/<name> and records the names it was asked for.
type registry struct {
	url    string
	asked  []string
	doc    map[string]any
	status int
}

func serve(t *testing.T, doc map[string]any) *registry {
	t.Helper()
	r := &registry{doc: doc, status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.asked = append(r.asked, strings.TrimPrefix(req.URL.Path, "/domain/"))
		if r.status != http.StatusOK {
			w.WriteHeader(r.status)
			return
		}
		w.Header().Set("Content-Type", "application/rdap+json")
		_ = json.NewEncoder(w).Encode(r.doc)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

// answer builds a minimal RDAP document.
func answer(expiry, registered time.Time, status ...string) map[string]any {
	return map[string]any{
		"ldhName": "example.com",
		"status":  status,
		"events": []map[string]string{
			{"eventAction": "registration", "eventDate": registered.Format(time.RFC3339)},
			{"eventAction": "expiration", "eventDate": expiry.Format(time.RFC3339)},
		},
		"entities": []map[string]any{{
			"roles": []string{"registrar"},
			"vcardArray": []any{"vcard", []any{
				[]any{"version", map[string]any{}, "text", "4.0"},
				[]any{"fn", map[string]any{}, "text", "Example Registrar Inc."},
			}},
		}},
	}
}

func newProber(t *testing.T, options string) probe.Prober {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "domain"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func run(t *testing.T, p probe.Prober, target probe.Target) (probe.Result, []metrics.Sample) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec := metrics.NewRecorder(metrics.Labels{})
	req := probe.Request{Target: target, Buckets: metrics.Buckets{0.1, 1}}
	return p.Probe(ctx, req, rec), rec.Samples()
}

func host(name string) probe.Target { return probe.Target{Name: name, Host: name} }

func gauge(t *testing.T, samples []metrics.Sample, name string) float64 {
	t.Helper()
	for _, s := range samples {
		if s.Name == name {
			if g, ok := s.Value.(metrics.Gauge); ok {
				return float64(g)
			}
		}
	}
	t.Fatalf("%s was not recorded", name)
	return 0
}

func info(samples []metrics.Sample, name string) string {
	for _, s := range samples {
		if s.Name == name {
			if i, ok := s.Value.(metrics.Info); ok {
				return string(i)
			}
		}
	}
	return ""
}

func TestExpiryIsReportedInDays(t *testing.T) {
	reg := serve(t, answer(time.Now().Add(400*24*time.Hour), time.Now().Add(-3*365*24*time.Hour)))
	p := newProber(t, "server: "+reg.url)

	res, samples := run(t, p, host("example.com"))
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if got := gauge(t, samples, "domain_expiry_days"); got < 399 || got > 401 {
		t.Errorf("domain_expiry_days = %v, want about 400", got)
	}
	if got := gauge(t, samples, "domain_age_days"); got < 1094 || got > 1096 {
		t.Errorf("domain_age_days = %v, want about 1095", got)
	}
	if got := info(samples, "domain_registrar_info"); got != "Example Registrar Inc." {
		t.Errorf("domain_registrar_info = %q", got)
	}
}

func TestExpiryIsRecordedEvenWhenTheCheckFails(t *testing.T) {
	reg := serve(t, answer(time.Now().Add(9*24*time.Hour), time.Now().Add(-365*24*time.Hour)))
	p := newProber(t, "server: "+reg.url+"\nmin_days: 30")

	res, samples := run(t, p, host("example.com"))
	if res.Err == nil {
		t.Fatal("nine days left passed a 30-day minimum")
	}
	if !strings.Contains(res.Err.Error(), "9 days") {
		t.Errorf("the message does not say how long is left: %v", res.Err)
	}
	if got := gauge(t, samples, "domain_expiry_days"); got < 8 || got > 10 {
		t.Errorf("domain_expiry_days = %v, want about 9", got)
	}
}

func TestAlreadyExpiredIsNamedAsSuch(t *testing.T) {
	reg := serve(t, answer(time.Now().Add(-2*24*time.Hour), time.Now().Add(-365*24*time.Hour)))
	p := newProber(t, "server: "+reg.url)

	res, _ := run(t, p, host("example.com"))
	if res.Err == nil {
		t.Fatal("an expired domain passed")
	}
	if !strings.Contains(res.Err.Error(), "expired") {
		t.Errorf("the message does not say it has expired: %v", res.Err)
	}
}

// A host is looked up under its public suffix, not as given.
func TestTheRegistrableNameIsAsked(t *testing.T) {
	reg := serve(t, answer(time.Now().Add(400*24*time.Hour), time.Now().Add(-365*24*time.Hour)))
	p := newProber(t, "server: "+reg.url)

	cases := map[string]string{
		"www.example.co.uk":    "example.co.uk",
		"API.Example.COM.":     "example.com",
		"deep.sub.example.org": "example.org",
	}
	for given, want := range cases {
		reg.asked = nil
		if res, _ := run(t, p, host(given)); res.Err != nil {
			t.Fatalf("%s: %v", given, res.Err)
		}
		if len(reg.asked) != 1 || reg.asked[0] != want {
			t.Errorf("%s was looked up as %v, want %q", given, reg.asked, want)
		}
	}
}

func TestAURLTargetIsReducedToItsDomain(t *testing.T) {
	reg := serve(t, answer(time.Now().Add(400*24*time.Hour), time.Now().Add(-365*24*time.Hour)))
	p := newProber(t, "server: "+reg.url)

	u, _ := url.Parse("https://www.example.com/status")
	if res, _ := run(t, p, probe.Target{Name: u.String(), URL: u}); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(reg.asked) != 1 || reg.asked[0] != "example.com" {
		t.Errorf("looked up %v, want example.com", reg.asked)
	}
}

// A 404 is reported as content, not as an HTTP status failure.
func TestUnregisteredDomainSaysSo(t *testing.T) {
	reg := serve(t, nil)
	reg.status = http.StatusNotFound
	p := newProber(t, "server: "+reg.url)

	res, _ := run(t, p, host("example.com"))
	if res.Err == nil {
		t.Fatal("a missing registration passed")
	}
	if !strings.Contains(res.Err.Error(), "not registered") {
		t.Errorf("message: %v", res.Err)
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonContent {
		t.Errorf("reason = %q, want %q", got, probe.ReasonContent)
	}
}

func TestAMissingExpiryIsNotZeroDays(t *testing.T) {
	reg := serve(t, map[string]any{
		"ldhName": "example.de",
		"events": []map[string]string{
			{"eventAction": "last changed", "eventDate": time.Now().Format(time.RFC3339)},
		},
	})
	p := newProber(t, "server: "+reg.url)

	res, samples := run(t, p, host("example.de"))
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no expiry date") {
		t.Fatalf("want the missing date named, got %v", res.Err)
	}
	for _, s := range samples {
		if s.Name == "domain_expiry_days" {
			t.Errorf("a made-up expiry of %v was recorded", s.Value)
		}
	}
}

func TestTransferLockIsCheckedAndReported(t *testing.T) {
	unlocked := serve(t, answer(time.Now().Add(400*24*time.Hour), time.Now().Add(-365*24*time.Hour)))
	p := newProber(t, "server: "+unlocked.url+"\nrequire_status: [clientTransferProhibited]")

	res, samples := run(t, p, host("example.com"))
	if res.Err == nil {
		t.Fatal("an unlocked domain passed a check that requires the lock")
	}
	if got := gauge(t, samples, "domain_locked"); got != 0 {
		t.Errorf("domain_locked = %v, want 0", got)
	}

	// Spelled with spaces, as RFC 9083 registers it, not in EPP camelCase.
	locked := serve(t, answer(time.Now().Add(400*24*time.Hour), time.Now().Add(-365*24*time.Hour),
		"client delete prohibited", "client transfer prohibited"))
	p = newProber(t, "server: "+locked.url+"\nrequire_status: [clientTransferProhibited]")
	res, samples = run(t, p, host("example.com"))
	if res.Err != nil {
		t.Fatalf("a locked domain failed: %v", res.Err)
	}
	if got := gauge(t, samples, "domain_locked"); got != 1 {
		t.Errorf("domain_locked = %v, want 1", got)
	}
}

func TestForbiddenStatusFails(t *testing.T) {
	reg := serve(t, answer(time.Now().Add(400*24*time.Hour), time.Now().Add(-365*24*time.Hour), "client hold"))
	p := newProber(t, "server: "+reg.url+"\nforbid_status: [clientHold]")

	res, _ := run(t, p, host("example.com"))
	if res.Err == nil || !strings.Contains(res.Err.Error(), "clientHold") {
		t.Fatalf("want the status named, got %v", res.Err)
	}
}

func TestRegistrarChangeIsCaught(t *testing.T) {
	reg := serve(t, answer(time.Now().Add(400*24*time.Hour), time.Now().Add(-365*24*time.Hour)))
	p := newProber(t, "server: "+reg.url+"\nregistrar: \"Our Registrar Ltd\"")

	res, _ := run(t, p, host("example.com"))
	if res.Err == nil {
		t.Fatal("a domain at the wrong registrar passed")
	}
	if !strings.Contains(res.Err.Error(), "Example Registrar Inc.") {
		t.Errorf("the message does not name who holds it: %v", res.Err)
	}
}

// Registries also emit zoneless and date-only timestamps, not just RFC 3339.
func TestOtherDateFormatsAreRead(t *testing.T) {
	for _, date := range []string{"2030-04-01T12:00:00Z", "2030-04-01T12:00:00", "2030-04-01"} {
		reg := serve(t, map[string]any{
			"ldhName": "example.com",
			"events":  []map[string]string{{"eventAction": "expiration", "eventDate": date}},
		})
		p := newProber(t, "server: "+reg.url)
		res, samples := run(t, p, host("example.com"))
		if res.Err != nil {
			t.Errorf("%s: %v", date, res.Err)
			continue
		}
		if got := gauge(t, samples, "domain_expiry_seconds"); got == 0 {
			t.Errorf("%s: no expiry recorded", date)
		}
	}
}

// A bare TLD fails before the registry is asked.
func TestATLDIsNotARegistrableDomain(t *testing.T) {
	reg := serve(t, nil)
	p := newProber(t, "server: "+reg.url)

	res, _ := run(t, p, host("com"))
	if res.Err == nil {
		t.Fatal("a bare TLD was looked up")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonInternal {
		t.Errorf("reason = %q, want %q", got, probe.ReasonInternal)
	}
	if len(reg.asked) != 0 {
		t.Errorf("the registry was asked anyway: %v", reg.asked)
	}
}

func TestNegativeMinDaysIsRejectedAtBuild(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("min_days: -1"), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "domain", Options: *node.Content[0]}
	if _, err := New(pc); err == nil {
		t.Fatal("want an error for a negative min_days")
	}
}

func TestProberAddressesItself(t *testing.T) {
	p := newProber(t, "server: http://example.invalid")
	sa, ok := p.(probe.Addressing)
	if !ok || !sa.SelfAddressed() {
		t.Fatalf("the domain prober must address itself")
	}
}
