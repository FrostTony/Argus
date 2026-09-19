package tlsinfo

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

func leafFor(t *testing.T, name string, serial int64) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		Issuer:       pkix.Name{CommonName: "Test CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func record(t *testing.T, certs ...*x509.Certificate) []metrics.Sample {
	t.Helper()
	rec := metrics.NewRecorder(metrics.Labels{})
	Record(rec, &tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: certs}, time.Now())
	return rec.Samples()
}

func info(t *testing.T, samples []metrics.Sample, name string) string {
	t.Helper()
	for _, s := range samples {
		if s.Name == name {
			if i, ok := s.Value.(metrics.Info); ok {
				return string(i)
			}
		}
	}
	t.Fatalf("%s was not recorded", name)
	return ""
}

func TestFingerprintChangesWithTheCertificate(t *testing.T) {
	first := leafFor(t, "site.example", 1)
	second := leafFor(t, "site.example", 2)

	a := info(t, record(t, first), "tls_cert_fingerprint_info")
	b := info(t, record(t, second), "tls_cert_fingerprint_info")
	if a == b {
		t.Fatal("two different certificates for the same name share a fingerprint")
	}
	if got := info(t, record(t, first), "tls_cert_fingerprint_info"); got != a {
		t.Fatal("the same certificate produced two fingerprints")
	}
}

func TestFingerprintIsTheSHA256OfTheCertificate(t *testing.T) {
	leaf := leafFor(t, "site.example", 7)
	sum := sha256.Sum256(leaf.Raw)

	if got := info(t, record(t, leaf), "tls_cert_fingerprint_info"); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint = %q, want the SHA-256 of the DER", got)
	}
	if got := info(t, record(t, leaf), "tls_cert_serial_info"); got != "7" {
		t.Fatalf("serial = %q, want 7 in hex", got)
	}
}

func TestNoCertificateReportsNoFingerprint(t *testing.T) {
	for _, s := range record(t) {
		if s.Name == "tls_cert_fingerprint_info" {
			t.Fatal("a fingerprint was reported for a connection with no certificate")
		}
	}
}
