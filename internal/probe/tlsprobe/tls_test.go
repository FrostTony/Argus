package tlsprobe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// serve starts a TLS listener with a self-signed certificate valid for the
// given span, and returns its port and the certificate as a CA file.
func serve(t *testing.T, notBefore, notAfter time.Time) (int, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "site.test"},
		DNSNames:              []string{"site.test"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
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
				_ = conn.(*tls.Conn).Handshake()
			}()
		}
	}()

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return ln.Addr().(*net.TCPAddr).Port, ca
}

func check(t *testing.T, port int, ca string) (probe.Result, map[string]float64) {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("ca_cert: "+ca), &node); err != nil {
		t.Fatal(err)
	}
	p, err := New(config.Probe{Name: "t", Type: "tls", Options: *node.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec := metrics.NewRecorder(metrics.Labels{})
	res := p.Probe(ctx, probe.Request{
		Target:  probe.Target{Name: "site.test", Host: "site.test", Port: port},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}, rec)

	gauges := map[string]float64{}
	for _, s := range rec.Samples() {
		if g, ok := s.Value.(metrics.Gauge); ok {
			gauges[s.Name] = float64(g)
		}
	}
	return res, gauges
}

func TestValidCertificatePasses(t *testing.T) {
	port, ca := serve(t, time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour))
	res, gauges := check(t, port, ca)
	if !res.OK() {
		t.Fatal(res.Err)
	}
	if days := gauges["tls_cert_expiry_days"]; days < 29 || days > 30 {
		t.Fatalf("tls_cert_expiry_days = %v, want about 30", days)
	}
}

func TestExpiredCertificateIsStillReported(t *testing.T) {
	port, ca := serve(t, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	res, gauges := check(t, port, ca)
	if res.OK() || probe.ReasonOf(res.Err) != probe.ReasonTLS {
		t.Fatalf("an expired certificate: err = %v, want a tls failure", res.Err)
	}
	if days, ok := gauges["tls_cert_expiry_days"]; !ok || days > -0.9 {
		t.Fatalf("tls_cert_expiry_days = %v (present %v), want about -1", days, ok)
	}
	if gauges["tls_cert_valid"] != 0 {
		t.Fatal("tls_cert_valid = 1 for an expired certificate")
	}
}

func TestWrongNameStillFails(t *testing.T) {
	port, ca := serve(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("{ca_cert: "+ca+", server_name: other.test}"), &node); err != nil {
		t.Fatal(err)
	}
	p, err := New(config.Probe{Name: "t", Type: "tls", Options: *node.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Probe(context.Background(), probe.Request{
		Target:  probe.Target{Name: "site.test", Host: "site.test", Port: port},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}, metrics.NewRecorder(metrics.Labels{}))
	if res.OK() {
		t.Fatal("a certificate for site.test was accepted for other.test")
	}
}
