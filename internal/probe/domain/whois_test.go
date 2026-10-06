package domain

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// tci is whois.tcinet.ru's answer, verbatim but for the dates.
const tci = `% TCI Whois Service. Terms of use:
% https://tcinet.ru/documents/whois_ru_rf.pdf (in Russian)

domain:        AFISHA.RU
nserver:       ns1.rambler.ru.
state:         REGISTERED, DELEGATED, VERIFIED
org:           "Afisha" LLC
registrar:     RU-CENTER-RU
admin-contact: https://www.nic.ru/whois
created:       %CREATED%
paid-till:     %PAID%
free-date:     2099-01-01
source:        TCI

Last updated on 2026-10-06T10:00:00Z
`

// whoisd answers every query with body and records the queries it got.
type whoisd struct {
	addr  string
	asked chan string
}

func serveWHOIS(t *testing.T, body string) *whoisd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	w := &whoisd{addr: ln.Addr().String(), asked: make(chan string, 8)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			q, _ := bufio.NewReader(conn).ReadString('\n')
			w.asked <- q
			if body != "" {
				conn.Write([]byte(body))
				conn.Close()
			}
		}
	}()
	return w
}

func tciAnswer(paid, created time.Time) string {
	return strings.NewReplacer(
		"%PAID%", paid.UTC().Format(time.RFC3339),
		"%CREATED%", created.UTC().Format(time.RFC3339),
	).Replace(tci)
}

func TestRUIsReadOverWHOIS(t *testing.T) {
	srv := serveWHOIS(t, tciAnswer(time.Now().Add(300*day), time.Now().Add(-20*365*day)))
	p := newProber(t, "whois: {ru: "+srv.addr+"}")

	res, samples := run(t, p, host("www.afisha.ru"))
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if q := <-srv.asked; q != "afisha.ru\r\n" {
		t.Errorf("asked %q, want the registrable name and CRLF", q)
	}
	if got := gauge(t, samples, "domain_expiry_days"); got < 299 || got > 301 {
		t.Errorf("domain_expiry_days = %v, want about 300", got)
	}
	if got := gauge(t, samples, "domain_age_days"); got < 7299 || got > 7301 {
		t.Errorf("domain_age_days = %v, want about 7300", got)
	}
	if got := info(samples, "domain_registrar_info"); got != "RU-CENTER-RU" {
		t.Errorf("domain_registrar_info = %q", got)
	}
	if got := info(samples, "domain_status_info"); got != "REGISTERED,DELEGATED,VERIFIED" {
		t.Errorf("domain_status_info = %q", got)
	}
	// .ru has no transfer lock: 0 would read as an unlocked domain.
	for _, s := range samples {
		if s.Name == "domain_locked" {
			t.Errorf("domain_locked = %v for a registry without locks", s.Value)
		}
	}
}

func TestWHOISStatesAreChecked(t *testing.T) {
	body := strings.Replace(tciAnswer(time.Now().Add(300*day), time.Now().Add(-day)),
		"REGISTERED, DELEGATED, VERIFIED", "REGISTERED, NOT DELEGATED, VERIFIED", 1)
	srv := serveWHOIS(t, body)

	p := newProber(t, "whois: {ru: "+srv.addr+"}\nrequire_status: [delegated]")
	if res, _ := run(t, p, host("afisha.ru")); res.Err == nil {
		t.Error("NOT DELEGATED passed require_status: [delegated]")
	}
	p = newProber(t, "whois: {ru: "+srv.addr+"}\nforbid_status: [not delegated]")
	if res, _ := run(t, p, host("afisha.ru")); res.Err == nil {
		t.Error("NOT DELEGATED passed forbid_status: [not delegated]")
	}
}

func TestWHOISUnknownNameIsNotRegistered(t *testing.T) {
	srv := serveWHOIS(t, "% TCI Whois Service.\n\nNo entries found for the selected source(s).\n")
	p := newProber(t, "whois: {ru: "+srv.addr+"}")

	res, _ := run(t, p, host("no-such-name.ru"))
	if res.Err == nil || !strings.Contains(res.Err.Error(), "not registered") {
		t.Fatalf("want not registered, got %v", res.Err)
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonContent {
		t.Errorf("reason = %q, want %q", got, probe.ReasonContent)
	}
}

// A rate limit is not "not registered", and its text is what the operator needs to see.
func TestWHOISRefusalIsAProtocolError(t *testing.T) {
	srv := serveWHOIS(t, "% comment\nYou have exceeded allowed connection rate.\n")
	p := newProber(t, "whois: {ru: "+srv.addr+"}")

	res, _ := run(t, p, host("afisha.ru"))
	if got := probe.ReasonOf(res.Err); got != probe.ReasonProtocol {
		t.Fatalf("reason = %q, want %q (err: %v)", got, probe.ReasonProtocol, res.Err)
	}
	if !strings.Contains(res.Err.Error(), "exceeded allowed connection rate") {
		t.Errorf("the server's words are lost: %v", res.Err)
	}
}

func TestSilentWHOISIsATimeout(t *testing.T) {
	srv := serveWHOIS(t, "")
	p := newProber(t, "whois: {ru: "+srv.addr+"}")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	res := p.Probe(ctx, probe.Request{Target: host("afisha.ru")}, metrics.NewRecorder(metrics.Labels{}))
	if got := probe.ReasonOf(res.Err); got != probe.ReasonTimeout {
		t.Fatalf("reason = %q, want %q (err: %v)", got, probe.ReasonTimeout, res.Err)
	}
}

// The ICANN layout: one EPP code per line, followed by a link.
func TestICANNStyleWHOIS(t *testing.T) {
	r, err := parseWHOIS("example.kz", `Domain Name: EXAMPLE.KZ
Registrar: Example Registrar
Creation Date: 2001-02-03T04:05:06Z
Registry Expiry Date: 2030-02-03T04:05:06Z
Domain Status: clientTransferProhibited https://icann.org/epp#clientTransferProhibited
Domain Status: clientDeleteProhibited https://icann.org/epp#clientDeleteProhibited
`)
	if err != nil {
		t.Fatal(err)
	}
	if r.expiry.Year() != 2030 || r.registered.Year() != 2001 || r.registrar != "Example Registrar" {
		t.Errorf("record = %+v", r)
	}
	if want := []string{"clientTransferProhibited", "clientDeleteProhibited"}; strings.Join(r.status, " ") != strings.Join(want, " ") {
		t.Errorf("status = %q, want %q", r.status, want)
	}
}

func TestSuffixesAreRouted(t *testing.T) {
	p := newProber(t, "whois: {рф: other.example:4343, su: '', kz: whois.nic.kz}").(*Prober)

	cases := map[string]string{
		"afisha.ru":             "whois.tcinet.ru:43",
		"xn--e1afmkfd.xn--p1ai": "other.example:4343",
		"example.kz":            "whois.nic.kz:43",
		"example.su":            "rdap",
		"example.com":           "rdap",
		"example.msk.ru":        "rdap", // its own registry, not TCI
		"example.co.uk":         "rdap",
	}
	for name, want := range cases {
		got := "rdap"
		if w, ok := p.sourceFor(name).(*whoisServer); ok {
			got = w.addr
		}
		if got != want {
			t.Errorf("%s goes to %s, want %s", name, got, want)
		}
	}
}

func TestUnicodeNamesAreAskedInPunycode(t *testing.T) {
	srv := serveWHOIS(t, tciAnswer(time.Now().Add(300*day), time.Now().Add(-day)))
	p := newProber(t, "whois: {рф: "+srv.addr+"}")

	if res, _ := run(t, p, host("www.Пример.РФ")); res.Err != nil {
		t.Fatal(res.Err)
	}
	if q := <-srv.asked; q != "xn--e1afmkfd.xn--p1ai\r\n" {
		t.Errorf("asked %q", q)
	}
}

func TestBadWHOISSuffixIsRejectedAtBuild(t *testing.T) {
	if _, err := New(probeConfig(t, "whois: {'': whois.example}")); err == nil {
		t.Error("an empty suffix was accepted")
	}
}
