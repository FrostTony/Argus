package dnsprobe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func TestServerSpelling(t *testing.T) {
	cases := []struct {
		proto, in, want string
		bad             bool
	}{
		{proto: protoUDP, in: "1.1.1.1", want: "1.1.1.1:53"},
		{proto: protoUDP, in: "1.1.1.1:5353", want: "1.1.1.1:5353"},
		{proto: protoTCP, in: "ns.example.com", want: "ns.example.com:53"},
		{proto: protoTLS, in: "one.one.one.one", want: "one.one.one.one:853"},
		{proto: protoTLS, in: "one.one.one.one:8853", want: "one.one.one.one:8853"},
		{proto: protoHTTPS, in: "https://dns.google", want: "https://dns.google/dns-query"},
		{proto: protoHTTPS, in: "https://dns.google/resolve", want: "https://dns.google/resolve"},
		{proto: protoHTTPS, in: "1.1.1.1", bad: true},
		{proto: protoTLS, in: "https://dns.google", bad: true},
	}
	for _, c := range cases {
		tr, err := newTransport(c.proto, tlsinfo.Options{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := tr.server(c.in)
		switch {
		case c.bad && err == nil:
			t.Errorf("%s: %q was accepted, got %q", c.proto, c.in, got)
		case !c.bad && err != nil:
			t.Errorf("%s: %q was refused: %v", c.proto, c.in, err)
		case !c.bad && got != c.want:
			t.Errorf("%s: %q became %q, want %q", c.proto, c.in, got, c.want)
		}
	}
}

func TestProtoAliases(t *testing.T) {
	for in, want := range map[string]string{
		"udp": protoUDP, "TCP": protoTCP, "dot": protoTLS,
		"tcp-tls": protoTLS, "doh": protoHTTPS, "https": protoHTTPS,
	} {
		if got := protoAliases[strings.ToLower(in)]; got != want {
			t.Errorf("%q resolved to %q, want %q", in, got, want)
		}
	}
}

// RFC 8484: the question goes in the body as wire format, and comes back the
// same way. The id is zeroed so that every client asking it shares a cache key.
func TestDNSOverHTTPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/dns-message" {
			t.Errorf("content-type = %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if err := q.Unpack(body); err != nil {
			t.Errorf("the body is not a DNS message: %v", err)
			return
		}
		if q.Id != 0 {
			t.Errorf("id = %d, want 0", q.Id)
		}
		resp := new(dns.Msg)
		resp.SetReply(q)
		rr, _ := dns.NewRR("example.com. 300 IN A 192.0.2.1")
		resp.Answer = []dns.RR{rr}
		wire, _ := resp.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(wire)
	}))
	defer srv.Close()

	tr, err := newTransport(protoHTTPS, tlsinfo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	m.Id = 4242

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// httptest serves plain HTTP; the URL check lives in server(), not here.
	resp, span, err := tr.overHTTPS(ctx, probe.Request{}, m, srv.URL, metrics.NewRecorder(nil))
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("got %d answers, want 1", len(resp.Answer))
	}
	if span.query <= 0 {
		t.Error("the exchange was not timed")
	}
}

func TestDNSOverHTTPSRejectsANonAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	tr, _ := newTransport(protoHTTPS, tlsinfo.Options{})
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	if _, _, err := tr.overHTTPS(context.Background(), probe.Request{}, m, srv.URL, metrics.NewRecorder(nil)); err == nil {
		t.Error("a 503 was read as an answer")
	}
}

// An HTTPS endpoint is a server that returned some bytes; unlike the resolver
// library, nothing else checks that the bytes answer the question asked.
func TestDNSOverHTTPSRejectsAnAnswerToAnotherQuestion(t *testing.T) {
	cases := map[string]func(q *dns.Msg) *dns.Msg{
		"another name": func(q *dns.Msg) *dns.Msg {
			resp := new(dns.Msg)
			resp.SetQuestion("totally-different.invalid.", dns.TypeA)
			resp.Response = true
			resp.Id = q.Id
			rr, _ := dns.NewRR("totally-different.invalid. 60 IN A 9.9.9.9")
			resp.Answer = []dns.RR{rr}
			return resp
		},
		"another type": func(q *dns.Msg) *dns.Msg {
			resp := new(dns.Msg)
			resp.SetQuestion("example.com.", dns.TypeAAAA)
			resp.Response = true
			resp.Id = q.Id
			return resp
		},
		"not a response": func(q *dns.Msg) *dns.Msg {
			resp := q.Copy()
			resp.Response = false
			return resp
		},
		"another id": func(q *dns.Msg) *dns.Msg {
			resp := q.Copy()
			resp.Response = true
			resp.Id = 1234
			return resp
		},
	}

	for name, forge := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			q := new(dns.Msg)
			if err := q.Unpack(body); err != nil {
				t.Errorf("%s: the body is not a DNS message: %v", name, err)
				return
			}
			wire, _ := forge(q).Pack()
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write(wire)
		}))

		tr, _ := newTransport(protoHTTPS, tlsinfo.Options{})
		m := new(dns.Msg)
		m.SetQuestion("example.com.", dns.TypeA)
		_, _, err := tr.overHTTPS(context.Background(), probe.Request{}, m, srv.URL, metrics.NewRecorder(nil))
		if err == nil {
			t.Errorf("%s: the answer was accepted", name)
		}
		srv.Close()
	}
}

// The resolver's certificate expires like any other, and the handshake is a
// separate problem from the answer.
func TestDNSOverHTTPSSplitsConnectFromQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		_ = q.Unpack(body)
		resp := new(dns.Msg)
		resp.SetReply(q)
		time.Sleep(40 * time.Millisecond)
		wire, _ := resp.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(wire)
	}))
	defer srv.Close()

	tr, _ := newTransport(protoHTTPS, tlsinfo.Options{})
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	_, span, err := tr.overHTTPS(context.Background(), probe.Request{}, m, srv.URL, metrics.NewRecorder(nil))
	if err != nil {
		t.Fatal(err)
	}
	if span.connect <= 0 {
		t.Error("connect was not measured, so DoT and DoH cannot be compared")
	}
	if span.query < 30*time.Millisecond {
		t.Errorf("query = %v, want the server's own delay", span.query)
	}
}
