// Package tcp checks a TCP connection to one backend, optionally holding a
// protocol conversation on it.
package tcp

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/dialog"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func init() { probe.Register("tcp", New) }

// Config is the `tcp:` block.
type Config struct {
	Port int `yaml:"port"`
	// TLS wraps the connection before the first step.
	TLS        bool            `yaml:"tls"`
	TLSOptions tlsinfo.Options `yaml:"tls_config"`

	// Send and Expect are the shorthand for a one-step conversation.
	Send   string        `yaml:"send"`
	Expect string        `yaml:"expect"`
	Steps  []dialog.Step `yaml:"steps"`

	ReadBytes int `yaml:"read_bytes"`
}

type Prober struct {
	cfg    Config
	script *dialog.Script
	tls    *tls.Config
}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{ReadBytes: dialog.DefaultReadBytes}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	script, err := dialog.Compile("tcp", cfg.Send, cfg.Expect, cfg.Steps, cfg.ReadBytes, "tcp_response_size_bytes")
	if err != nil {
		return nil, err
	}
	tc, err := tlsinfo.Build(cfg.TLSOptions)
	if err != nil {
		return nil, err
	}
	return &Prober{cfg: cfg, script: script, tls: tc}, nil
}

// Files implements probe.FileBacked.
func (p *Prober) Files() []string { return p.cfg.TLSOptions.Files() }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	addr, err := req.Address(p.cfg.Port, req.Backend.Port, req.Target.Port)
	if err != nil {
		res.Err = err
		return res
	}

	start := time.Now()
	conn, err := req.Dialer("tcp").DialContext(ctx, "tcp", addr)
	res.Add("connect", time.Since(start))
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonConnect, err)
		return res
	}
	// The closure, not the value: STARTTLS replaces conn, and closing the raw
	// one would skip close_notify.
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	upgrade := func(ctx context.Context, c net.Conn) (net.Conn, error) {
		return p.upgrade(ctx, c, req, rec, &res)
	}
	if p.cfg.TLS {
		if conn, err = upgrade(ctx, conn); err != nil {
			res.Err = probe.Wrap(probe.ReasonTLS, err)
			return res
		}
	}

	conn, res.Err = p.script.Run(ctx, conn, rec, &res, upgrade)
	return res
}

// upgrade performs the TLS handshake and records what the peer presented.
func (p *Prober) upgrade(ctx context.Context, conn net.Conn, req probe.Request, rec *metrics.Recorder, res *probe.Result) (net.Conn, error) {
	cfg := p.tls.Clone()
	if cfg.ServerName == "" {
		// The certificate is issued to the name, not the pinned address.
		cfg.ServerName = req.ServerName()
	}
	tconn := tls.Client(conn, cfg)

	start := time.Now()
	err := tconn.HandshakeContext(ctx)
	res.Add("tls", time.Since(start))
	if err != nil {
		return nil, err
	}
	state := tconn.ConnectionState()
	over, err := tlsinfo.Inspect(ctx, rec, &state, p.cfg.TLSOptions, time.Now())
	res.Overhead += over
	if err != nil {
		return nil, err
	}
	return tconn, nil
}
