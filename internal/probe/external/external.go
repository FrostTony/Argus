// Package external runs a command and turns its "name value" output into
// metrics.
package external

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func init() { probe.Register("external", New) }

// Config is the `external:` block.
type Config struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// Env entries are "KEY=value", added to the ARGUS_* variables.
	Env []string `yaml:"env"`
	// Mode is exit_code (exit status only) or output (stdout is parsed too).
	Mode string `yaml:"mode"`
	// MetricPrefix keeps command-supplied names from colliding with built-ins.
	MetricPrefix string `yaml:"metric_prefix"`
	WorkDir      string `yaml:"work_dir"`
	// SelfAddressed skips resolution: the command addresses the target itself.
	SelfAddressed bool `yaml:"self_addressed"`
}

type Prober struct {
	cfg      Config
	parseOut bool
	environ  []string
}

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{Mode: "output", MetricPrefix: "external_"}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if cfg.Command == "" {
		return nil, fmt.Errorf("external.command is required")
	}
	if cfg.Mode != "output" && cfg.Mode != "exit_code" {
		return nil, fmt.Errorf("external.mode: want output|exit_code, got %q", cfg.Mode)
	}
	return &Prober{cfg: cfg, parseOut: cfg.Mode == "output", environ: os.Environ()}, nil
}

// waitDelay bounds the read of a killed command's output.
const waitDelay = 2 * time.Second

// maxOutput bounds the stdout kept for parsing; a command printing more is
// stopped. Only the first line of stderr is reported, so less is kept of it.
const (
	maxOutput = 1 << 20
	maxStderr = 4 << 10
)

func (p *Prober) SelfAddressed() bool { return p.cfg.SelfAddressed }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	// Cancelled on its own when the output overflows, which kills the
	// command like the deadline does.
	cmdCtx, stop := context.WithCancel(ctx)
	defer stop()

	cmd := exec.CommandContext(cmdCtx, p.cfg.Command, p.expandArgs(req)...)
	cmd.Dir = p.cfg.WorkDir
	cmd.Env = append(slices.Clip(p.environ), p.env(req)...)
	// The deadline must reach everything the command started, not only the
	// command; a surviving grandchild also keeps the inherited stdout open.
	ownGroup(cmd)
	cmd.WaitDelay = waitDelay

	stdout := &limitedBuffer{limit: maxOutput, overflow: stop}
	stderr := &limitedBuffer{limit: maxStderr}
	// Piped even when unread, so a grandchild holding it open is waited for.
	cmd.Stdout, cmd.Stderr = io.Discard, stderr
	if p.parseOut {
		cmd.Stdout = stdout
	}

	start := time.Now()
	err := cmd.Run()
	res.Add("command", time.Since(start))

	rec.Gauge("external_exit_code", float64(cmd.ProcessState.ExitCode()))
	// A command killed at the deadline returns an ordinary exit error.
	if ctx.Err() != nil {
		res.Err = probe.Wrap(probe.ReasonTimeout, fmt.Errorf("the command did not finish: %w", ctx.Err()))
		return res
	}
	if stdout.exceeded {
		res.Err = probe.Fail(probe.ReasonContent, "the command printed more than %d bytes", maxOutput)
		return res
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Err = probe.Fail(probe.ReasonProtocol, "exit %d: %s",
				ee.ExitCode(), firstLine(stderr.buf.String()))
			return res
		}
		res.Err = probe.Wrap(probe.ReasonInternal, err)
		return res
	}

	if p.parseOut {
		n, err := p.parse(rec, stdout.buf.String())
		if err != nil {
			res.Err = probe.Fail(probe.ReasonContent, "%v", err)
			return res
		}
		rec.Gauge("external_metrics_parsed", float64(n))
	}
	return res
}

// parse reads "name value" lines, ignoring blanks and #-comments. Labels may be
// appended in the Prometheus style: name{a="1"} value.
func (p *Prober) parse(rec *metrics.Recorder, out string) (int, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, rest, err := parseLine(line)
		if err != nil {
			return n, err
		}
		value, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return n, fmt.Errorf("value of %q: %w", name, err)
		}
		// The name is untrusted: one invalid character breaks the whole /metrics page.
		rec.Gauge(metrics.SanitizeName(p.cfg.MetricPrefix+name), value, labels...)
		n++
	}
	return n, sc.Err()
}

// parseLine splits a line into the name, its label pairs and the value text.
func parseLine(line string) (name string, labels []string, rest string, err error) {
	i := strings.IndexAny(line, "{ \t")
	if i < 0 {
		return "", nil, "", fmt.Errorf("line %q is not \"name value\"", line)
	}
	name, rest = line[:i], line[i:]
	if rest[0] == '{' {
		if labels, rest, err = parseLabels(rest[1:]); err != nil {
			return "", nil, "", fmt.Errorf("labels of %q: %w", name, err)
		}
	}
	return name, labels, strings.TrimSpace(rest), nil
}

var errUnclosed = errors.New("unclosed label set")

// parseLabels reads key="value" pairs up to the closing brace and returns
// them with the text after it.
func parseLabels(s string) (labels []string, rest string, err error) {
	for {
		s = strings.TrimLeft(s, " \t")
		if strings.HasPrefix(s, "}") {
			return labels, s[1:], nil
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, "", fmt.Errorf("%q is not key=value", s)
		}
		k = strings.TrimSpace(k)
		if !metrics.ValidLabelName(k) {
			return nil, "", fmt.Errorf("label name %q is not usable", k)
		}
		var value string
		if value, s, err = labelValue(strings.TrimLeft(v, " \t")); err != nil {
			return nil, "", err
		}
		labels = append(labels, k, value)
		s = strings.TrimLeft(s, " \t")
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case !strings.HasPrefix(s, "}"):
			return nil, "", errUnclosed
		}
	}
}

// labelValue reads one value off the front of s. A quoted value takes the
// text format's escapes (\\, \" and \n); a bare one runs to the next comma
// or brace.
func labelValue(s string) (value, rest string, err error) {
	if !strings.HasPrefix(s, `"`) {
		i := strings.IndexAny(s, ",}")
		if i < 0 {
			return "", "", errUnclosed
		}
		return strings.TrimSpace(s[:i]), s[i:], nil
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return b.String(), s[i+1:], nil
		case c == '\\' && i+1 < len(s):
			i++
			if s[i] == 'n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", fmt.Errorf("unterminated value %s", s)
}

// limitedBuffer keeps the first limit bytes written to it and calls overflow,
// once, when more arrive. The excess is accepted and dropped so the copy from
// the command's pipe goes on until the command is stopped.
type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow func()
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		if !b.exceeded && b.overflow != nil {
			b.overflow()
		}
		b.exceeded = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// env exposes the target and backend to the command as ARGUS_* variables.
func (p *Prober) env(req probe.Request) []string {
	env := []string{
		"ARGUS_TARGET=" + req.Target.Name,
		"ARGUS_HOST=" + req.Target.Host,
		"ARGUS_BACKEND=" + req.Backend.String(),
	}
	if req.Backend.Valid() {
		env = append(env, "ARGUS_BACKEND_IP="+req.Backend.Addr.String())
	}
	if req.Target.URL != nil {
		env = append(env, "ARGUS_URL="+req.Target.URL.String())
	}
	return append(env, p.cfg.Env...)
}

// expandArgs substitutes the same values into @placeholder@ arguments.
func (p *Prober) expandArgs(req probe.Request) []string {
	r := strings.NewReplacer(
		"@target@", req.Target.Name,
		"@host@", req.Target.Host,
		"@backend@", req.Backend.String(),
		"@backend_ip@", req.Backend.Addr.String(),
		"@url@", urlOf(req),
	)
	out := make([]string, 0, len(p.cfg.Args))
	for _, a := range p.cfg.Args {
		out = append(out, r.Replace(a))
	}
	return out
}

func urlOf(req probe.Request) string {
	if req.Target.URL == nil {
		return ""
	}
	return req.Target.URL.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
