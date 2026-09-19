// Package udp sends a datagram to one backend and optionally checks the reply.
package udp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func init() { probe.Register("udp", New) }

// Config is the `udp:` block.
type Config struct {
	Port int    `yaml:"port"`
	Send string `yaml:"send"`
	// Expect is a substring the reply must contain.
	Expect    string `yaml:"expect"`
	ReadBytes int    `yaml:"read_bytes"`
	// UnreachableWait bounds the wait for an ICMP port-unreachable.
	UnreachableWait config.Duration `yaml:"unreachable_wait"`
}

type Prober struct{ cfg Config }

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{ReadBytes: 1500}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if cfg.ReadBytes <= 0 {
		cfg.ReadBytes = 1500
	}
	return &Prober{cfg: cfg}, nil
}

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	addr, err := req.Address(p.cfg.Port, req.Backend.Port, req.Target.Port)
	if err != nil {
		res.Err = err
		return res
	}

	start := time.Now()
	conn, err := req.Dialer("udp").DialContext(ctx, "udp", addr)
	res.Add("connect", time.Since(start))
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonConnect, err)
		return res
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	t := time.Now()
	if _, err := conn.Write([]byte(p.cfg.Send)); err != nil {
		res.Err = probe.Wrap(probe.ReasonProtocol, err)
		return res
	}
	res.Add("write", time.Since(t))

	// A connected UDP socket learns of an ICMP port-unreachable only on a later
	// read, so the read below exists to collect it even when nothing is expected.
	silenceIsSuccess := p.cfg.Expect == ""
	if silenceIsSuccess {
		deadline, ok := errorWindow(ctx, time.Now(), p.cfg.UnreachableWait.D())
		if !ok {
			res.Err = probe.Fail(probe.ReasonTimeout,
				"no time left to see whether %s is refusing datagrams", addr)
			return res
		}
		_ = conn.SetReadDeadline(deadline)
	}

	buf := make([]byte, p.cfg.ReadBytes)
	t = time.Now()
	n, err := conn.Read(buf)
	// The wait for an ICMP error is overhead, not service latency.
	if err == nil || !isTimeout(err) {
		res.Add("read", time.Since(t))
		rec.Gauge("udp_response_size_bytes", float64(n))
	} else if silenceIsSuccess {
		res.Overhead = time.Since(t)
	}
	switch {
	case err == nil:
	case silenceIsSuccess && isTimeout(err):
		return res
	default:
		// Classified by what happened, not by whether Expect was set.
		reason := probe.ReasonConnect
		if isTimeout(err) {
			reason = probe.ReasonTimeout
		}
		res.Err = probe.Wrap(reason, err)
		return res
	}
	if !silenceIsSuccess && !bytes.Contains(buf[:n], []byte(p.cfg.Expect)) {
		res.Err = probe.Fail(probe.ReasonContent, "response does not contain %q", p.cfg.Expect)
	}
	return res
}

// defaultUnreachableWait bounds the wait for an ICMP error when no reply is
// expected; it covers a wide-area round trip.
const defaultUnreachableWait = 250 * time.Millisecond

// errorWindow reports how long to wait for an ICMP error, and whether any
// budget is left to wait at all.
func errorWindow(ctx context.Context, now time.Time, wait time.Duration) (time.Time, bool) {
	if wait <= 0 {
		wait = defaultUnreachableWait
	}
	end := now.Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(end) {
		end = d
	}
	return end, end.After(now)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
