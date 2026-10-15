package tlsinfo

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

func (ca authority) issueNaming(t *testing.T, serial int64, responder string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "leaf.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		OCSPServer:   []string{responder},
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

// The responder is asked only when a trusted chain vouches for the
// certificate that names it; otherwise its address is the peer's choice.
func TestOnlyAVerifiedCertificateNamesAResponder(t *testing.T) {
	var asked atomic.Int64
	responder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer responder.Close()

	ca := newCA(t)
	leaf := ca.issueNaming(t, 77, responder.URL)
	unverified := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca.cert}}
	if err, by := inspect(t, unverified, Options{CheckRevoked: true}); err != nil || asked.Load() != 0 {
		t.Fatalf("unverified: err %v, responder asked %d times", err, asked.Load())
	} else if by["tls_ocsp_status_info"].Value != metrics.Info("unavailable") {
		t.Errorf("unverified: status %v", by["tls_ocsp_status_info"].Value)
	}

	verified := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{ca.issueNaming(t, 78, responder.URL), ca.cert},
	}
	verified.VerifiedChains = [][]*x509.Certificate{verified.PeerCertificates}
	_, _ = inspect(t, verified, Options{CheckRevoked: true})
	if asked.Load() != 1 {
		t.Errorf("verified: responder asked %d times, want 1", asked.Load())
	}
}
