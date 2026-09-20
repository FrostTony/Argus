// Package dialog holds the send/expect conversation the stream probers share.
// An open port proves only that the kernel accepted the connection; what the
// service says when spoken to is the check.
package dialog

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// Step is one exchange. Expect is a regular expression whose capture groups
// later steps reference as ${1}; named groups become probe_expect_info labels.
type Step struct {
	Send   string `yaml:"send"`
	Expect string `yaml:"expect"`
	// StartTLS upgrades the connection after this step.
	StartTLS bool `yaml:"starttls"`

	expect *regexp.Regexp
}

// Script is a compiled conversation.
type Script struct {
	steps []Step
	// readBytes caps one reply; a service that keeps talking is not a reason to
	// keep reading.
	readBytes int
	// size names the series the bytes read are reported under, which is the
	// prober's own — the conversation is shared, the vocabulary is not.
	size string
}

// DefaultReadBytes is one reply's ceiling when the probe names none.
const DefaultReadBytes = 4096

// Compile builds the script from a probe's configuration. The shorthand pair
// and the step list are two spellings of the same thing, so only one may be
// given; kind names the probe in the errors.
func Compile(kind, send, expect string, steps []Step, readBytes int, sizeSeries string) (*Script, error) {
	if send != "" || expect != "" {
		if len(steps) > 0 {
			return nil, fmt.Errorf("%s: use send/expect or steps, not both", kind)
		}
		steps = []Step{{Send: send, Expect: expect}}
	}
	// Copied: the configuration is data other generations may still be reading.
	steps = append([]Step(nil), steps...)
	for i := range steps {
		if steps[i].Expect == "" {
			continue
		}
		re, err := regexp.Compile(steps[i].Expect)
		if err != nil {
			return nil, fmt.Errorf("%s.steps[%d].expect: %w", kind, i, err)
		}
		steps[i].expect = re
	}
	if readBytes <= 0 {
		readBytes = DefaultReadBytes
	}
	return &Script{steps: steps, readBytes: readBytes, size: sizeSeries}, nil
}

// Empty reports whether there is anything to say.
func (s *Script) Empty() bool { return s == nil || len(s.steps) == 0 }

// Upgrade starts TLS on an open connection. A prober that has no TLS to offer
// passes nil, and a script that asks for STARTTLS then fails rather than
// carrying on in the clear.
type Upgrade func(ctx context.Context, conn net.Conn) (net.Conn, error)

// Run walks the steps, carrying capture groups forward. It returns the
// connection it ended on, which STARTTLS replaces.
func (s *Script) Run(ctx context.Context, conn net.Conn, rec *metrics.Recorder, res *probe.Result, up Upgrade) (net.Conn, error) {
	if s.Empty() {
		return conn, nil
	}
	reader := bufio.NewReader(conn)
	var captures []string
	var read int64
	// Recorded however the conversation ends, success or failure.
	defer func() { rec.Gauge(s.size, float64(read)) }()

	for i, step := range s.steps {
		if step.Send != "" {
			t := time.Now()
			if _, err := io.WriteString(conn, expand(step.Send, captures)); err != nil {
				return conn, probe.Wrap(probe.ReasonProtocol, err)
			}
			res.Add("write", time.Since(t))
		}

		if step.expect != nil {
			t := time.Now()
			data, match, err := s.readReply(ctx, conn, reader, step.expect)
			res.Add("read", time.Since(t))
			read += int64(len(data))
			if err != nil && len(data) == 0 {
				return conn, probe.Wrap(probe.ReasonProtocol, err)
			}
			if match == nil {
				return conn, probe.FailRegex("step %d: %q does not match %q", i+1, probe.Excerpt(data), step.Expect)
			}
			captures = match
			probe.ExpectInfo(rec, step.expect, match, "step", strconv.Itoa(i+1))
		}

		if step.StartTLS {
			if up == nil {
				return conn, probe.Fail(probe.ReasonInternal, "step %d: starttls, but this probe has no TLS to start", i+1)
			}
			upgraded, err := up(ctx, conn)
			if err != nil {
				return conn, probe.Wrap(probe.ReasonTLS, err)
			}
			conn = upgraded
			reader = bufio.NewReader(conn)
		}
	}
	return conn, nil
}

// settle bounds the wait for the rest of a reply: a stream keeps no message
// boundaries, so one read is not one reply.
const settle = 250 * time.Millisecond

// readReply reads until the expectation matches, read_bytes have arrived, or
// nothing more comes. The first byte may take the whole deadline, later bytes
// settle.
func (s *Script) readReply(ctx context.Context, conn net.Conn, r *bufio.Reader, expect *regexp.Regexp) (string, []string, error) {
	deadline, _ := ctx.Deadline()
	defer conn.SetReadDeadline(deadline) //nolint:errcheck // the connection is about to be closed anyway

	buf := make([]byte, 0, s.readBytes)
	for len(buf) < cap(buf) {
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		// Matched on the bytes: a reply that arrives in pieces would otherwise
		// be copied into a fresh string on every read.
		if idx := expect.FindSubmatchIndex(buf); idx != nil {
			return string(buf), captures(buf, idx), nil
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

// captures turns submatch indices into the strings later steps substitute,
// laid out as FindStringSubmatch would: index 0 is the whole match, and a
// group that did not participate is empty.
func captures(buf []byte, idx []int) []string {
	out := make([]string, len(idx)/2)
	for i := range out {
		if lo, hi := idx[2*i], idx[2*i+1]; lo >= 0 {
			out[i] = string(buf[lo:hi])
		}
	}
	return out
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
