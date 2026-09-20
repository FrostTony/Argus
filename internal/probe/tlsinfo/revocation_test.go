package tlsinfo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"golang.org/x/crypto/ocsp"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Argus Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return authority{cert: cert, key: key}
}

func (ca authority) issue(t *testing.T, serial int64) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "leaf.test"},
		DNSNames:     []string{"leaf.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// staple is the OCSP answer a server would attach to its handshake.
func (ca authority) staple(t *testing.T, leaf *x509.Certificate, status int) []byte {
	t.Helper()
	tmpl := ocsp.Response{
		SerialNumber: leaf.SerialNumber,
		Status:       status,
		ThisUpdate:   time.Now().Add(-time.Minute),
		NextUpdate:   time.Now().Add(time.Hour),
	}
	if status == ocsp.Revoked {
		tmpl.RevokedAt = time.Now().Add(-2 * time.Hour)
		tmpl.RevocationReason = ocsp.KeyCompromise
	}
	der, err := ocsp.CreateResponse(ca.cert, ca.cert, tmpl, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func inspect(t *testing.T, st *tls.ConnectionState, opts Options) (error, map[string]metrics.Sample) {
	t.Helper()
	rec := metrics.NewRecorder(nil)
	_, err := Inspect(context.Background(), rec, st, opts, time.Now())
	by := map[string]metrics.Sample{}
	for _, s := range rec.Samples() {
		by[s.Name] = s
	}
	return err, by
}

func TestStapledGoodResponse(t *testing.T) {
	ca := newCA(t)
	leaf := ca.issue(t, 42)
	st := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf, ca.cert},
		OCSPResponse:     ca.staple(t, leaf, ocsp.Good),
	}

	err, by := inspect(t, st, Options{CheckRevoked: true})
	if err != nil {
		t.Fatalf("a good certificate failed the check: %v", err)
	}
	if got := by["tls_ocsp_status_info"].Value; got != metrics.Info("good") {
		t.Errorf("status = %v, want good", got)
	}
	if got := by["tls_cert_revoked"].Value; got != metrics.Gauge(0) {
		t.Errorf("tls_cert_revoked = %v, want 0", got)
	}
}

// The whole point: every other check passes on a revoked certificate.
func TestRevokedCertificateFailsTheCheck(t *testing.T) {
	ca := newCA(t)
	leaf := ca.issue(t, 43)
	st := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf, ca.cert},
		OCSPResponse:     ca.staple(t, leaf, ocsp.Revoked),
	}

	err, by := inspect(t, st, Options{CheckRevoked: true})
	if err == nil {
		t.Fatal("a revoked certificate passed")
	}
	if got := by["tls_cert_revoked"].Value; got != metrics.Gauge(1) {
		t.Errorf("tls_cert_revoked = %v, want 1", got)
	}
	if got := by["tls_ocsp_status_info"].Value; got != metrics.Info("revoked") {
		t.Errorf("status = %v, want revoked", got)
	}
	// The expiry date says nothing is wrong, which is why the check exists.
	if got := by["tls_cert_valid"].Value; got != metrics.Gauge(1) {
		t.Errorf("tls_cert_valid = %v; the dates are fine, the certificate is not", got)
	}
}

// A responder that cannot be reached is an answer about the responder. Failing
// on it would make every service depend on a third party's uptime.
func TestUnreachableResponderDoesNotFail(t *testing.T) {
	ca := newCA(t)
	leaf := ca.issue(t, 44)
	// No staple and no responder named: there is nothing to ask.
	st := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca.cert}}

	err, by := inspect(t, st, Options{CheckRevoked: true})
	if err != nil {
		t.Fatalf("an unanswerable question failed the check: %v", err)
	}
	if got := by["tls_ocsp_status_info"].Value; got != metrics.Info("unavailable") {
		t.Errorf("status = %v, want unavailable", got)
	}
}

func TestRevocationIsOptIn(t *testing.T) {
	ca := newCA(t)
	leaf := ca.issue(t, 45)
	st := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf, ca.cert},
		OCSPResponse:     ca.staple(t, leaf, ocsp.Revoked),
	}

	err, by := inspect(t, st, Options{})
	if err != nil {
		t.Fatalf("the check ran without being asked for: %v", err)
	}
	if _, ok := by["tls_cert_revoked"]; ok {
		t.Error("tls_cert_revoked was recorded without check_revoked")
	}
	// Recording the certificate itself is not optional.
	if _, ok := by["tls_cert_expiry_days"]; !ok {
		t.Error("the certificate was not recorded")
	}
}

// A stale "good" is what a revoked certificate's owner would like us to accept.
func TestStaleResponseIsNotGood(t *testing.T) {
	ca := newCA(t)
	leaf := ca.issue(t, 46)
	tmpl := ocsp.Response{
		SerialNumber: leaf.SerialNumber,
		Status:       ocsp.Good,
		ThisUpdate:   time.Now().Add(-8 * 24 * time.Hour),
		NextUpdate:   time.Now().Add(-7 * 24 * time.Hour),
	}
	der, err := ocsp.CreateResponse(ca.cert, ca.cert, tmpl, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	st := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf, ca.cert},
		OCSPResponse:     der,
	}

	err, by := inspect(t, st, Options{CheckRevoked: true})
	if err != nil {
		t.Fatalf("a stale answer failed the check outright: %v", err)
	}
	if got := by["tls_ocsp_status_info"].Value; got != metrics.Info("stale") {
		t.Errorf("status = %v, want stale", got)
	}
	if got := by["tls_cert_revoked"].Value; got != metrics.Gauge(0) {
		t.Errorf("tls_cert_revoked = %v, want 0", got)
	}
}

// Revocation is permanent; the answer being old does not undo it.
func TestStaleRevocationStillFails(t *testing.T) {
	ca := newCA(t)
	leaf := ca.issue(t, 47)
	tmpl := ocsp.Response{
		SerialNumber:     leaf.SerialNumber,
		Status:           ocsp.Revoked,
		RevokedAt:        time.Now().Add(-30 * 24 * time.Hour),
		RevocationReason: ocsp.KeyCompromise,
		ThisUpdate:       time.Now().Add(-8 * 24 * time.Hour),
		NextUpdate:       time.Now().Add(-7 * 24 * time.Hour),
	}
	der, err := ocsp.CreateResponse(ca.cert, ca.cert, tmpl, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	st := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf, ca.cert},
		OCSPResponse:     der,
	}

	err, by := inspect(t, st, Options{CheckRevoked: true})
	if err == nil {
		t.Fatal("a stale revocation was let through")
	}
	if got := by["tls_cert_revoked"].Value; got != metrics.Gauge(1) {
		t.Errorf("tls_cert_revoked = %v, want 1", got)
	}
}

// One name, one series: the exposition would otherwise carry it twice.
func TestNoSeriesIsRecordedTwice(t *testing.T) {
	ca := newCA(t)
	leaf := ca.issue(t, 48)
	st := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf, ca.cert},
		OCSPResponse:     ca.staple(t, leaf, ocsp.Good),
	}

	rec := metrics.NewRecorder(nil)
	if _, err := Inspect(context.Background(), rec, st, Options{CheckRevoked: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, s := range rec.Samples() {
		seen[s.Name+s.Labels.Key()]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("%s was recorded %d times in one batch", name, n)
		}
	}
}
