// Package tlsprobe checks the TLS handshake and certificate of one backend.
package tlsprobe

import (
	"cmp"
	"context"
	"crypto/tls"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func init() { probe.Register("tls", New) }

// Config is the `tls:` block.
type Config struct {
	Port int `yaml:"port"`
	// MinDaysLeft fails the check when the certificate expires sooner than this.
	MinDaysLeft int             `yaml:"min_days_left"`
	Options     tlsinfo.Options `yaml:",inline"`
}

type Prober struct {
	cfg  Config
	base *tls.Config
}

func New(pc config.Probe) (probe.Prober, error) {
	// Port stays zero so an explicit target or backend port wins; 443 is the
	// fallback applied last in Address.
	var cfg Config
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	base, err := tlsinfo.Build(cfg.Options)
	if err != nil {
		return nil, err
	}
	return &Prober{cfg: cfg, base: base}, nil
}

// Files implements probe.FileBacked.
func (p *Prober) Files() []string { return p.cfg.Options.Files() }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	addr, err := req.Address(p.cfg.Port, req.Backend.Port, req.Target.Port, 443)
	if err != nil {
		res.Err = err
		return res
	}

	// SNI is the domain, not the backend address: a certificate names a host.
	var seen *tls.ConnectionState
	cfg := tlsinfo.Observed(p.base, cmp.Or(p.base.ServerName, req.ServerName()),
		func(cs tls.ConnectionState) { seen = &cs })

	start := time.Now()
	conn, err := req.Dialer("tcp").DialContext(ctx, "tcp", addr)
	res.Add("connect", time.Since(start))
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonConnect, err)
		return res
	}
	defer conn.Close()

	tconn := tls.Client(conn, cfg)
	hs := time.Now()
	err = tconn.HandshakeContext(ctx)
	res.Add("tls", time.Since(hs))

	// Recorded whether or not the certificate was accepted.
	now := time.Now()
	over, revoked := tlsinfo.Inspect(ctx, rec, seen, p.cfg.Options, now)
	res.Overhead += over
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonTLS, err)
		return res
	}
	if revoked != nil {
		res.Err = probe.Fail(probe.ReasonTLS, "%v", revoked)
		return res
	}
	st := tconn.ConnectionState()

	if p.cfg.MinDaysLeft > 0 && len(st.PeerCertificates) > 0 {
		left := st.PeerCertificates[0].NotAfter.Sub(now).Hours() / 24
		if left < float64(p.cfg.MinDaysLeft) {
			res.Err = probe.Fail(probe.ReasonTLS, "certificate has %.1f days left, want %d", left, p.cfg.MinDaysLeft)
		}
	}
	return res
}
