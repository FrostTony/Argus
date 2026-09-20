package tlsinfo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/ocsp"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// Revocation is the question an expiry date cannot answer. A certificate whose
// key leaked is revoked and stays valid-looking for the rest of its life: every
// other check on it passes, and the browser is the only thing that notices.

const (
	// ocspMaxBody bounds what a responder may send back.
	ocspMaxBody = 1 << 20
	// ocspTimeout bounds one trip to the responder. It is a third party on the
	// far side of the internet, not the target, and it must not be able to
	// spend the target's whole deadline.
	ocspTimeout = 5 * time.Second
	// ocspMaxAge is how long an answer is reused even when it claims to be
	// current for longer. Responders publish for days; noticing a revocation
	// within the hour is the trade we want.
	ocspMaxAge = time.Hour
	// ocspRetryAfter is how long a responder that could not be reached is left
	// alone. Asking a broken responder every run helps nobody.
	ocspRetryAfter = 5 * time.Minute
)

// Inspect records the connection and, when the probe asked for it, the
// certificate's revocation status.
//
// It returns the time spent talking to the OCSP responder, which is a third
// party rather than the target: the prober adds it to Result.Overhead so that
// it does not land in probe_duration_seconds.
//
// The error is only for a certificate the responder says is revoked. A
// responder that cannot be reached is an answer about the responder, and
// failing the check on it would make every service depend on a third party's
// uptime.
func Inspect(ctx context.Context, rec *metrics.Recorder, st *tls.ConnectionState, opts Options, now time.Time) (time.Duration, error) {
	Record(rec, st, now)
	if !opts.CheckRevoked || st == nil || len(st.PeerCertificates) == 0 {
		return 0, nil
	}

	resp, spent, err := revocation(ctx, st, now)
	// A revocation is not undone by the answer being old, so it outranks the
	// freshness complaint that may have come with it.
	revoked := resp != nil && resp.Status == ocsp.Revoked
	// Written whatever the answer: an alert on a series that appears only once
	// the certificate is already revoked never fires.
	rec.Gauge("tls_cert_revoked", metrics.Bool(revoked))
	if resp != nil && !resp.NextUpdate.IsZero() {
		rec.Gauge("tls_ocsp_next_update_seconds", float64(resp.NextUpdate.Unix()))
	}
	switch {
	case revoked:
		rec.Info("tls_ocsp_status_info", statusName(ocsp.Revoked))
		return spent, fmt.Errorf("the certificate was revoked on %s (%s)",
			resp.RevokedAt.UTC().Format(time.RFC3339), revocationReason(resp.RevocationReason))
	case err != nil:
		rec.Info("tls_ocsp_status_info", reasonName(err))
	default:
		rec.Info("tls_ocsp_status_info", statusName(resp.Status))
	}
	return spent, nil
}

// errStale marks an answer that is no longer current. Replaying an old "good"
// is exactly what a revoked certificate's owner would like us to accept.
var errStale = errors.New("the responder's answer is no longer current")

func reasonName(err error) string {
	if errors.Is(err, errStale) {
		return "stale"
	}
	return "unavailable"
}

// revocation returns the OCSP answer and what asking for it cost. A stapled
// response costs nothing and is what the server wants us to see; going to the
// responder is the fallback, and its answer is cached.
func revocation(ctx context.Context, st *tls.ConnectionState, now time.Time) (*ocsp.Response, time.Duration, error) {
	leaf := st.PeerCertificates[0]
	issuer := issuerOf(st)
	if issuer == nil {
		return nil, 0, fmt.Errorf("no issuer certificate to check against")
	}

	if len(st.OCSPResponse) > 0 {
		resp, err := ocsp.ParseResponseForCert(st.OCSPResponse, leaf, issuer)
		if err != nil {
			return nil, 0, err
		}
		return resp, 0, freshness(resp, now)
	}
	if len(leaf.OCSPServer) == 0 {
		return nil, 0, fmt.Errorf("the certificate names no OCSP responder")
	}

	key := fingerprintOf(leaf.Raw)
	if resp, err, ok := answers.get(key, now); ok {
		return resp, 0, err
	}
	start := time.Now()
	resp, err := askResponder(ctx, leaf, issuer, leaf.OCSPServer[0])
	spent := time.Since(start)
	if err == nil {
		err = freshness(resp, now)
	}
	answers.put(key, resp, err, now)
	return resp, spent, err
}

// freshness rejects an answer that is not current. ParseResponseForCert checks
// the signature and the serial, and nothing else.
func freshness(resp *ocsp.Response, now time.Time) error {
	if !resp.NextUpdate.IsZero() && now.After(resp.NextUpdate) {
		return errStale
	}
	if resp.ThisUpdate.After(now.Add(time.Minute)) {
		return errStale
	}
	return nil
}

// answers is the responder cache. Without it a probe on a 15-second interval
// asks a third party 5760 times a day about something that changes a few times
// a decade — and responders rate-limit.
var answers = &cache{by: map[string]answer{}}

type answer struct {
	resp  *ocsp.Response
	err   error
	until time.Time
}

type cache struct {
	mu sync.Mutex
	by map[string]answer
}

// cacheMax bounds the map by roughly one entry per certificate under check.
const cacheMax = 4096

func (c *cache) get(key string, now time.Time) (*ocsp.Response, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.by[key]
	if !ok || now.After(a.until) {
		return nil, nil, false
	}
	return a.resp, a.err, true
}

func (c *cache) put(key string, resp *ocsp.Response, err error, now time.Time) {
	until := now.Add(ocspRetryAfter)
	if err == nil {
		until = now.Add(ocspMaxAge)
		if !resp.NextUpdate.IsZero() && resp.NextUpdate.Before(until) {
			until = resp.NextUpdate
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.by) >= cacheMax {
		for k, a := range c.by {
			if now.After(a.until) {
				delete(c.by, k)
			}
		}
		// Still full: the fleet outgrew the cache, and holding stale entries
		// helps less than answering the certificates being checked now.
		if len(c.by) >= cacheMax {
			clear(c.by)
		}
	}
	c.by[key] = answer{resp: resp, err: err, until: until}
}

// issuerOf is the certificate that signed the leaf: the next one the server
// presented, or the one the verified chain settled on.
func issuerOf(st *tls.ConnectionState) *x509.Certificate {
	if len(st.PeerCertificates) > 1 {
		return st.PeerCertificates[1]
	}
	for _, chain := range st.VerifiedChains {
		if len(chain) > 1 {
			return chain[1]
		}
	}
	return nil
}

func askResponder(ctx context.Context, leaf, issuer *x509.Certificate, url string) (*ocsp.Response, error) {
	body, err := ocsp.CreateRequest(leaf, issuer, nil)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ocspTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/ocsp-request")
	req.Header.Set("Accept", "application/ocsp-response")

	// Not pinned and not pooled: reached however the host reaches the internet.
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()

	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the responder answered %d", resp.StatusCode)
	}
	der, err := io.ReadAll(io.LimitReader(resp.Body, ocspMaxBody))
	if err != nil {
		return nil, err
	}
	return ocsp.ParseResponseForCert(der, leaf, issuer)
}

// fingerprintOf is the cache key: the certificate itself, so a rotation asks
// again rather than reading the old answer.
func fingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func statusName(status int) string {
	switch status {
	case ocsp.Good:
		return "good"
	case ocsp.Revoked:
		return "revoked"
	case ocsp.Unknown:
		return "unknown"
	}
	return fmt.Sprintf("status %d", status)
}

// revocationReason is RFC 5280's CRLReason, in the words the RFC uses.
func revocationReason(code int) string {
	names := []string{
		"unspecified", "key compromise", "CA compromise", "affiliation changed",
		"superseded", "cessation of operation", "certificate hold", "",
		"remove from CRL", "privilege withdrawn", "AA compromise",
	}
	if code >= 0 && code < len(names) && names[code] != "" {
		return names[code]
	}
	return fmt.Sprintf("reason %d", code)
}
