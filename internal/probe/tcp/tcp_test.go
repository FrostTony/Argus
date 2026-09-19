package tcp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

type script struct {
	greet string
	// reply maps what the client sends to what the server answers.
	reply map[string]string
	// starttlsAfter upgrades the connection once this line has been answered.
	starttlsAfter string
}

func serve(t *testing.T, s script) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.converse(conn)
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func (s script) converse(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if s.greet != "" {
		if _, err := conn.Write([]byte(s.greet)); err != nil {
			return
		}
	}
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n == 0 || err != nil {
			return
		}
		line := strings.TrimSpace(string(buf[:n]))
		if answer, ok := s.reply[line]; ok {
			if _, err := conn.Write([]byte(answer)); err != nil {
				return
			}
		}
		if s.starttlsAfter != "" && line == s.starttlsAfter {
			tconn := tls.Server(conn, selfSigned())
			if err := tconn.Handshake(); err != nil {
				return
			}
			conn = tconn
		}
	}
}

// selfSigned is a certificate the client is configured to accept.
var selfSigned = sync.OnceValue(func() *tls.Config {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "argus-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}
})

func newProber(t *testing.T, options string) probe.Prober {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "tcp"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func run(t *testing.T, p probe.Prober, host string, port int) (probe.Result, []metrics.Sample) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := metrics.NewRecorder(metrics.Labels{})
	req := probe.Request{
		Target:  probe.Target{Name: host, Host: host, Port: port},
		Backend: probe.Backend{Addr: netip.MustParseAddr(host), Port: port},
		Buckets: metrics.Buckets{0.01, 0.1},
	}
	return p.Probe(ctx, req, rec), rec.Samples()
}

func TestConnectOnlySucceedsWithoutAConversation(t *testing.T) {
	host, port := serve(t, script{})
	res, _ := run(t, newProber(t, "port: "+strconv.Itoa(port)), host, port)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if !hasPhase(res, "connect") {
		t.Fatalf("want a connect phase, got %v", res.Phases)
	}
}

func TestRefusedConnectionIsAConnectFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // nothing listens here now

	res, _ := run(t, newProber(t, "port: "+strconv.Itoa(port)), "127.0.0.1", port)
	if got := probe.ReasonOf(res.Err); got != probe.ReasonConnect {
		t.Fatalf("reason = %q, want %q", got, probe.ReasonConnect)
	}
}

func TestSendExpectShorthand(t *testing.T) {
	host, port := serve(t, script{reply: map[string]string{"PING": "PONG\r\n"}})
	p := newProber(t, `
port: `+strconv.Itoa(port)+`
send: "PING\r\n"
expect: "^PONG"
`)
	res, samples := run(t, p, host, port)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	var size float64
	for _, s := range samples {
		if s.Name == "tcp_response_size_bytes" {
			size = float64(s.Value.(metrics.Gauge))
		}
	}
	if size == 0 {
		t.Fatal("want tcp_response_size_bytes to be recorded")
	}
}

func TestUnmatchedExpectIsAContentFailure(t *testing.T) {
	host, port := serve(t, script{greet: "220 mail ready\r\n"})
	p := newProber(t, `
port: `+strconv.Itoa(port)+`
expect: "^250 "
`)
	res, _ := run(t, p, host, port)
	if got := probe.ReasonOf(res.Err); got != probe.ReasonContent {
		t.Fatalf("reason = %q, want %q", got, probe.ReasonContent)
	}
	if !strings.Contains(res.Err.Error(), "220 mail ready") {
		t.Fatalf("want the unmatched text quoted, got %v", res.Err)
	}
}

func TestSendAndExpectCannotBeMixedWithSteps(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("send: \"A\"\nsteps:\n  - send: \"B\"\n"), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "tcp", Options: *node.Content[0]}
	if _, err := New(pc); err == nil {
		t.Fatal("want an error when both forms are given")
	}
}

func TestCaptureGroupsCarryIntoLaterSteps(t *testing.T) {
	host, port := serve(t, script{
		greet: "HELLO session=42\r\n",
		reply: map[string]string{"RESUME 42": "OK\r\n"},
	})
	p := newProber(t, `
port: `+strconv.Itoa(port)+`
steps:
  - expect: "session=([0-9]+)"
  - send: "RESUME ${1}\r\n"
    expect: "^OK"
`)
	if res, _ := run(t, p, host, port); res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
}

func TestWrongCaptureIsRejectedByTheServer(t *testing.T) {
	host, port := serve(t, script{
		greet: "HELLO session=42\r\n",
		reply: map[string]string{"RESUME 42": "OK\r\n", "RESUME 7": "ERR\r\n"},
	})
	p := newProber(t, `
port: `+strconv.Itoa(port)+`
steps:
  - expect: "session=[0-9]+"
  - send: "RESUME 7\r\n"
    expect: "^OK"
`)
	if res, _ := run(t, p, host, port); res.Err == nil {
		t.Fatal("want failure when the server answers ERR")
	}
}

func TestStartTLSUpgradesMidConversation(t *testing.T) {
	host, port := serve(t, script{
		greet:         "220 ready\r\n",
		reply:         map[string]string{"STARTTLS": "220 go ahead\r\n"},
		starttlsAfter: "STARTTLS",
	})
	p := newProber(t, `
port: `+strconv.Itoa(port)+`
tls_config:
  insecure_skip_verify: true
steps:
  - expect: "^220 "
  - send: "STARTTLS\r\n"
    expect: "^220 "
    starttls: true
`)
	res, samples := run(t, p, host, port)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if !hasPhase(res, "tls") {
		t.Fatalf("want a tls phase after the upgrade, got %v", res.Phases)
	}
	if !has(samples, "tls_cert_expiry_days") {
		t.Fatal("want the certificate recorded after the upgrade")
	}
}

func TestImmediateTLSRecordsTheCertificate(t *testing.T) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", selfSigned())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = conn.Write([]byte("READY\r\n"))
				time.Sleep(50 * time.Millisecond)
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	p := newProber(t, `
port: `+strconv.Itoa(port)+`
tls: true
tls_config:
  insecure_skip_verify: true
expect: "^READY"
`)
	res, samples := run(t, p, "127.0.0.1", port)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if !has(samples, "tls_cert_expiry_days") {
		t.Fatal("want the certificate recorded")
	}
}

func TestMissingPortIsAnInternalFailure(t *testing.T) {
	p := newProber(t, "send: \"x\"")
	ctx := context.Background()
	rec := metrics.NewRecorder(metrics.Labels{})
	req := probe.Request{
		Target:  probe.Target{Name: "example.test", Host: "example.test"},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1")},
	}
	res := p.Probe(ctx, req, rec)
	if got := probe.ReasonOf(res.Err); got != probe.ReasonInternal {
		t.Fatalf("reason = %q, want %q", got, probe.ReasonInternal)
	}
}

func TestBadExpectRegexIsRejectedAtBuild(t *testing.T) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("expect: \"(\""), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "tcp", Options: *node.Content[0]}
	if _, err := New(pc); err == nil {
		t.Fatal("want an error for an unparsable expect")
	}
}

func has(samples []metrics.Sample, name string) bool {
	for _, s := range samples {
		if s.Name == name {
			return true
		}
	}
	return false
}

func hasPhase(res probe.Result, name string) bool {
	for _, p := range res.Phases {
		if p.Name == name {
			return true
		}
	}
	return false
}

func TestResponseSizeIsRecordedOnFailureToo(t *testing.T) {
	host, port := serve(t, script{greet: "220 mail ready\r\n"})
	p := newProber(t, `
port: `+strconv.Itoa(port)+`
expect: "^250 "
`)
	res, samples := run(t, p, host, port)
	if res.Err == nil {
		t.Fatal("want a content failure")
	}
	for _, s := range samples {
		if s.Name == "tcp_response_size_bytes" {
			if got := float64(s.Value.(metrics.Gauge)); got != 16 {
				t.Fatalf("tcp_response_size_bytes = %v, want 16", got)
			}
			return
		}
	}
	t.Fatal("tcp_response_size_bytes is missing from a failed conversation")
}
