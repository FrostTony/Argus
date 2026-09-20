// Package unixprobe checks a unix-domain socket: that it accepts a connection,
// and optionally what the service on it says when spoken to.
//
// The target is the socket path — there is nothing to resolve and no backend to
// fan out over, so the probe addresses itself.
package unixprobe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/dialog"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func init() { probe.Register("unix", New) }

const sizeSeries = "unix_response_size_bytes"

// maxDatagram is the largest a unix datagram can be, so nothing is lost to a
// buffer that was too small.
const maxDatagram = 64 << 10

// Config is the `unix:` block.
type Config struct {
	// Network is unix (stream, the default), unixpacket or unixgram.
	Network string `yaml:"network"`
	// Path pins the probe to one socket, for a probe whose targets name
	// something else — a service rather than a file.
	Path string `yaml:"path"`

	// TLS wraps the connection before the first step. Rare on a local socket,
	// and the reason it is here is the socket that fronts a remote service.
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
	expect *regexp.Regexp // unixgram: one datagram, one pattern
	tls    *tls.Config
}

var networks = map[string]bool{"unix": true, "unixgram": true, "unixpacket": true}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{Network: "unix", ReadBytes: dialog.DefaultReadBytes}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if !networks[cfg.Network] {
		return nil, fmt.Errorf("unix.network: want unix|unixgram|unixpacket, got %q", cfg.Network)
	}
	// Built for every network: a malformed tls_config is a configuration error
	// whether or not this probe will get as far as using it, and Files() names
	// those paths to the reload watcher either way.
	tc, err := tlsinfo.Build(cfg.TLSOptions)
	if err != nil {
		return nil, err
	}
	if cfg.ReadBytes <= 0 {
		cfg.ReadBytes = dialog.DefaultReadBytes
	}
	p := &Prober{cfg: cfg, tls: tc}

	if cfg.Network == "unixgram" {
		// A datagram is one message: there is no conversation to walk and no
		// stream to upgrade, so the shorthand pair is the whole vocabulary.
		if len(cfg.Steps) > 0 {
			return nil, fmt.Errorf("unix: steps need a stream; network %q carries one datagram, so use send/expect", cfg.Network)
		}
		if cfg.TLS {
			return nil, fmt.Errorf("unix: tls needs a stream, not %q", cfg.Network)
		}
		if cfg.Expect != "" {
			re, err := regexp.Compile(cfg.Expect)
			if err != nil {
				return nil, fmt.Errorf("unix.expect: %w", err)
			}
			p.expect = re
		}
		return p, nil
	}

	script, err := dialog.Compile("unix", cfg.Send, cfg.Expect, cfg.Steps, cfg.ReadBytes, sizeSeries)
	if err != nil {
		return nil, err
	}
	p.script = script
	return p, nil
}

// SelfAddressed implements probe.Addressing: a path is not a name to resolve.
func (p *Prober) SelfAddressed() bool { return true }

// Files implements probe.FileBacked.
func (p *Prober) Files() []string { return p.cfg.TLSOptions.Files() }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	path := p.cfg.Path
	if path == "" {
		path = req.Target.Host
	}
	if path == "" {
		res.Err = probe.Fail(probe.ReasonInternal, "unix: no socket path; give one as the target or as unix.path")
		return res
	}
	if p.cfg.Network == "unixgram" {
		return p.datagram(ctx, path, rec)
	}
	return p.stream(ctx, path, req, rec)
}

func (p *Prober) stream(ctx context.Context, path string, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(ctx, p.cfg.Network, path)
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

// datagram sends one message and, when something is expected back, waits for
// one. The local end is bound to a socket of our own: an unbound unixgram
// socket has no address for the peer to answer.
func (p *Prober) datagram(ctx context.Context, path string, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	local, cleanup, err := localSocket()
	if err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "unix: %v", err)
		return res
	}
	defer cleanup()

	start := time.Now()
	conn, err := net.DialUnix("unixgram", local, &net.UnixAddr{Name: path, Net: "unixgram"})
	res.Add("connect", time.Since(start))
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonConnect, err)
		return res
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	start = time.Now()
	_, err = conn.Write([]byte(p.cfg.Send))
	res.Add("write", time.Since(start))
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonProtocol, err)
		return res
	}
	if p.expect == nil {
		// Nothing to wait for: a datagram socket that accepted the write is all
		// the answer there is.
		rec.Gauge(sizeSeries, 0)
		return res
	}

	// Read whole: a datagram short of the buffer is truncated by the kernel and
	// the rest is gone, so the size metric would lie and the pattern would be
	// matched against a prefix. read_bytes bounds what is matched, not what is
	// received.
	buf := make([]byte, maxDatagram)
	start = time.Now()
	n, err := conn.Read(buf)
	res.Add("read", time.Since(start))
	rec.Gauge(sizeSeries, float64(n))
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonProtocol, err)
		return res
	}
	reply := buf[:min(n, p.cfg.ReadBytes)]
	match := p.expect.FindStringSubmatch(string(reply))
	if match == nil {
		res.Err = probe.FailRegex("reply %q does not match %q", probe.Excerpt(string(reply)), p.cfg.Expect)
		return res
	}
	probe.ExpectInfo(rec, p.expect, match)
	return res
}

// localSocket binds a socket of our own in a private directory, and returns
// what removes both.
func localSocket() (*net.UnixAddr, func(), error) {
	dir, err := os.MkdirTemp("", "argus-unixgram-")
	if err != nil {
		return nil, nil, err
	}
	return &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unixgram"},
		func() { _ = os.RemoveAll(dir) }, nil
}

// upgrade performs the TLS handshake and records what the peer presented.
func (p *Prober) upgrade(ctx context.Context, conn net.Conn, req probe.Request, rec *metrics.Recorder, res *probe.Result) (net.Conn, error) {
	cfg := p.tls.Clone()
	if cfg.ServerName == "" {
		// A socket path is not a name a certificate can be issued to, so the
		// probe has to be told which one to verify against.
		cfg.ServerName = req.Hostname
	}
	if cfg.ServerName == "" && !cfg.InsecureSkipVerify {
		return nil, probe.Fail(probe.ReasonInternal,
			"unix: tls over a socket has no name to verify against; set tls_config.server_name or the probe's hostname")
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
