// Package tlsinfo builds TLS settings from configuration and turns a
// connection state into metrics.
package tlsinfo

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// Options is the `tls:` block of a probe.
type Options struct {
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	ServerName         string `yaml:"server_name"`
	MinVersion         string `yaml:"min_version"`
	MaxVersion         string `yaml:"max_version"`
	CACert             string `yaml:"ca_cert"`
	ClientCert         string `yaml:"client_cert"`
	ClientKey          string `yaml:"client_key"`
	// CheckRevoked asks the OCSP responder whether the certificate still
	// stands. An expiry date says nothing about a key that leaked last week.
	CheckRevoked bool `yaml:"check_revoked"`
}

// Files lists the paths Build reads, so a reload can detect a rotated file.
func (o Options) Files() []string {
	var out []string
	for _, f := range []string{o.CACert, o.ClientCert, o.ClientKey} {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func Build(o Options) (*tls.Config, error) {
	cfg := &tls.Config{
		InsecureSkipVerify: o.InsecureSkipVerify, //nolint:gosec // opt-in, for self-signed endpoints
		ServerName:         o.ServerName,
	}
	var err error
	if cfg.MinVersion, err = parseVersion(o.MinVersion); err != nil {
		return nil, fmt.Errorf("tls.min_version: %w", err)
	}
	if cfg.MaxVersion, err = parseVersion(o.MaxVersion); err != nil {
		return nil, fmt.Errorf("tls.max_version: %w", err)
	}
	if o.CACert != "" {
		pem, err := os.ReadFile(o.CACert)
		if err != nil {
			return nil, fmt.Errorf("tls.ca_cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.ca_cert: no certificates found in %s", o.CACert)
		}
		cfg.RootCAs = pool
	}
	if (o.ClientCert == "") != (o.ClientKey == "") {
		return nil, fmt.Errorf("tls: client_cert and client_key must be set together")
	}
	if o.ClientCert != "" {
		pair, err := tls.LoadX509KeyPair(o.ClientCert, o.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("tls: client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// Observed returns a copy of cfg that reports the peer's state to seen before
// verifying it, so an expired certificate is still measured. Verification runs
// against serverName rather than the SNI, which may be empty for an address.
func Observed(cfg *tls.Config, serverName string, seen func(tls.ConnectionState)) *tls.Config {
	out := cfg.Clone()
	out.ServerName = serverName
	verify := !out.InsecureSkipVerify
	out.InsecureSkipVerify = true //nolint:gosec // verified in VerifyConnection below
	out.VerifyConnection = func(cs tls.ConnectionState) error {
		seen(cs)
		switch {
		case !verify:
			return nil
		case serverName == "":
			return errors.New("tls: no server name to verify the certificate against")
		case len(cs.PeerCertificates) == 0:
			return errors.New("tls: the server presented no certificate")
		}
		opts := x509.VerifyOptions{
			Roots:         out.RootCAs,
			DNSName:       serverName,
			Intermediates: x509.NewCertPool(),
		}
		for _, c := range cs.PeerCertificates[1:] {
			opts.Intermediates.AddCert(c)
		}
		_, err := cs.PeerCertificates[0].Verify(opts)
		return err
	}
	return out
}

var versions = map[string]uint16{
	"":    0,
	"1.0": tls.VersionTLS10,
	"1.1": tls.VersionTLS11,
	"1.2": tls.VersionTLS12,
	"1.3": tls.VersionTLS13,
}

func parseVersion(s string) (uint16, error) {
	v, ok := versions[strings.TrimPrefix(strings.TrimSpace(s), "TLS")]
	if !ok {
		return 0, fmt.Errorf("want 1.0|1.1|1.2|1.3, got %q", s)
	}
	return v, nil
}

// fingerprint is the SHA-256 of the DER, the value other TLS tools print.
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func Record(rec *metrics.Recorder, st *tls.ConnectionState, now time.Time) {
	if st == nil {
		return
	}
	rec.Gauge("tls_enabled", 1)
	rec.Info("tls_version_info", versionName(st.Version))
	rec.Info("tls_cipher_info", tls.CipherSuiteName(st.CipherSuite))
	rec.Gauge("tls_handshake_resumed", metrics.Bool(st.DidResume))
	rec.Gauge("tls_ocsp_stapled", metrics.Bool(len(st.OCSPResponse) > 0))
	if st.NegotiatedProtocol != "" {
		rec.Info("tls_alpn_info", st.NegotiatedProtocol)
	}
	rec.Gauge("tls_chain_length", float64(len(st.PeerCertificates)))

	if len(st.PeerCertificates) == 0 {
		return
	}
	leaf := st.PeerCertificates[0]
	rec.Gauge("tls_cert_not_after_seconds", float64(leaf.NotAfter.Unix()))
	rec.Gauge("tls_cert_not_before_seconds", float64(leaf.NotBefore.Unix()))
	rec.Gauge("tls_cert_expiry_days", leaf.NotAfter.Sub(now).Hours()/24)
	rec.Gauge("tls_cert_valid", metrics.Bool(now.After(leaf.NotBefore) && now.Before(leaf.NotAfter)))
	rec.Info("tls_cert_issuer_info", leaf.Issuer.CommonName)
	rec.Info("tls_cert_subject_info", leaf.Subject.CommonName)
	rec.Gauge("tls_cert_san_count", float64(len(leaf.DNSNames)))
	rec.Info("tls_cert_fingerprint_info", fingerprint(leaf.Raw))
	rec.Info("tls_cert_serial_info", leaf.SerialNumber.Text(16))

	// An intermediate expires independently of the leaf and breaks the chain.
	earliest := leaf.NotAfter
	for _, c := range st.PeerCertificates[1:] {
		if c.NotAfter.Before(earliest) {
			earliest = c.NotAfter
		}
	}
	rec.Gauge("tls_chain_expiry_days", earliest.Sub(now).Hours()/24)
	rec.Gauge("tls_chain_not_after_seconds", float64(earliest.Unix()))
}

func versionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS1.0"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS13:
		return "TLS1.3"
	}
	return fmt.Sprintf("0x%04x", v)
}
