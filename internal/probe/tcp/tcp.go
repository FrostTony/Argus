// Package tcp checks a TCP connection to one backend, optionally holding a
// protocol conversation on it.
package tcp

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
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
	Send   string `yaml:"send"`
	Expect string `yaml:"expect"`
	Steps  []Step `yaml:"steps"`

	ReadBytes int `yaml:"read_bytes"`
}

// Step is one exchange. Expect is a regular expression whose capture groups
// later steps reference as ${1}.
type Step struct {
	Send   string `yaml:"send"`
	Expect string `yaml:"expect"`
	// StartTLS upgrades the connection after this step.
	StartTLS bool `yaml:"starttls"`

	expect *regexp.Regexp
}

type Prober struct {
	cfg   Config
	steps []Step
	tls   *tls.Config
}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{ReadBytes: 4096}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if cfg.ReadBytes <= 0 {
		cfg.ReadBytes = 4096
	}

	steps := cfg.Steps
	if cfg.Send != "" || cfg.Expect != "" {
		if len(steps) > 0 {
			return nil, fmt.Errorf("tcp: use send/expect or steps, not both")
		}
		steps = []Step{{Send: cfg.Send, Expect: cfg.Expect}}
	}
	for i := range steps {
		if steps[i].Expect == "" {
			continue
		}
		re, err := regexp.Compile(steps[i].Expect)
		if err != nil {
			return nil, fmt.Errorf("tcp.steps[%d].expect: %w", i, err)
		}
		steps[i].expect = re
	}

	tc, err := tlsinfo.Build(cfg.TLSOptions)
	if err != nil {
		return nil, err
	}
	return &Prober{cfg: cfg, steps: steps, tls: tc}, nil
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
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if p.cfg.TLS {
		conn, err = p.upgrade(ctx, conn, req, rec, &res)
		if err != nil {
			res.Err = probe.Wrap(probe.ReasonTLS, err)
			return res
		}
	}

	res.Err = p.converse(ctx, conn, req, rec, &res)
	return res
}

// converse walks the steps, carrying capture groups forward.
func (p *Prober) converse(ctx context.Context, conn net.Conn, req probe.Request, rec *metrics.Recorder, res *probe.Result) error {
	if len(p.steps) == 0 {
		return nil
	}
	reader := bufio.NewReader(conn)
	var captures []string
	var read int64
	// Recorded however the conversation ends, success or failure.
	defer func() { rec.Gauge("tcp_response_size_bytes", float64(read)) }()

	for i, step := range p.steps {
		if step.Send != "" {
			t := time.Now()
			if _, err := io.WriteString(conn, expand(step.Send, captures)); err != nil {
				return probe.Wrap(probe.ReasonProtocol, err)
			}
			res.Add("write", time.Since(t))
		}

		if step.expect != nil {
			t := time.Now()
			data, match, err := p.readReply(ctx, conn, reader, step.expect)
			res.Add("read", time.Since(t))
			read += int64(len(data))
			if err != nil && len(data) == 0 {
				return probe.Wrap(probe.ReasonProtocol, err)
			}
			if match == nil {
				return probe.Fail(probe.ReasonContent, "step %d: %q does not match %q", i+1, trim(data), step.Expect)
			}
			captures = match
		}

		if step.StartTLS {
			upgraded, err := p.upgrade(ctx, conn, req, rec, res)
			if err != nil {
				return probe.Wrap(probe.ReasonTLS, err)
			}
			conn = upgraded
			reader = bufio.NewReader(conn)
		}
	}
	return nil
}

// upgrade performs the TLS handshake and records what the peer presented.
func (p *Prober) upgrade(ctx context.Context, conn net.Conn, req probe.Request, rec *metrics.Recorder, res *probe.Result) (net.Conn, error) {
	cfg := p.tls.Clone()
	if cfg.ServerName == "" {
		// The certificate is issued to the name, not the pinned address.
		cfg.ServerName = req.Target.Host
	}
	tconn := tls.Client(conn, cfg)

	start := time.Now()
	err := tconn.HandshakeContext(ctx)
	res.Add("tls", time.Since(start))
	if err != nil {
		return nil, err
	}
	state := tconn.ConnectionState()
	tlsinfo.Record(rec, &state, time.Now())
	return tconn, nil
}

// settle bounds the wait for the rest of a reply: TCP keeps no message
// boundaries, so one read is not one reply.
const settle = 250 * time.Millisecond

// readReply reads until the expectation matches, read_bytes have arrived, or
// nothing more comes. The first byte may take the whole deadline, later bytes settle.
func (p *Prober) readReply(ctx context.Context, conn net.Conn, r *bufio.Reader, expect *regexp.Regexp) (string, []string, error) {
	deadline, _ := ctx.Deadline()
	defer conn.SetReadDeadline(deadline)

	buf := make([]byte, 0, p.cfg.ReadBytes)
	for len(buf) < cap(buf) {
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if match := expect.FindStringSubmatch(string(buf)); match != nil {
			return string(buf), match, nil
		}
		if err != nil {
			return string(buf), nil, err
		}
		wait := time.Now().Add(settle)
		if !deadline.IsZero() && deadline.Before(wait) {
			wait = deadline
		}
		_ = conn.SetReadDeadline(wait)
	}
	return string(buf), nil, nil
}

// expand substitutes ${n} with the capture groups of the previous expect.
func expand(s string, captures []string) string {
	if len(captures) == 0 || !strings.Contains(s, "${") {
		return s
	}
	for i, c := range captures {
		s = strings.ReplaceAll(s, "${"+strconv.Itoa(i)+"}", c)
	}
	return s
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
