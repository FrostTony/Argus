// Package external runs a command and turns its "name value" output into
// metrics.
package external

import (
	"bufio"
	"context"
	"errors"
	"fmt"
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

func (p *Prober) SelfAddressed() bool { return p.cfg.SelfAddressed }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	cmd := exec.CommandContext(ctx, p.cfg.Command, p.expandArgs(req)...)
	cmd.Dir = p.cfg.WorkDir
	cmd.Env = append(slices.Clip(p.environ), p.env(req)...)
	// The deadline must reach everything the command started, not only the
	// command; a surviving grandchild also keeps the inherited stdout open.
	ownGroup(cmd)
	cmd.WaitDelay = waitDelay

	start := time.Now()
	out, err := cmd.Output()
	res.Add("command", time.Since(start))

	rec.Gauge("external_exit_code", float64(cmd.ProcessState.ExitCode()))
	// A command killed at the deadline returns an ordinary exit error.
	if ctx.Err() != nil {
		res.Err = probe.Wrap(probe.ReasonTimeout, fmt.Errorf("the command did not finish: %w", ctx.Err()))
		return res
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Err = probe.Fail(probe.ReasonProtocol, "exit %d: %s",
				ee.ExitCode(), firstLine(string(ee.Stderr)))
			return res
		}
		res.Err = probe.Wrap(probe.ReasonInternal, err)
		return res
	}

	if p.parseOut {
		n, err := p.parse(rec, string(out))
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
		name, rest, ok := strings.Cut(line, " ")
		if !ok {
			return n, fmt.Errorf("line %q is not \"name value\"", line)
		}
		labels, err := splitLabels(&name)
		if err != nil {
			return n, err
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return n, fmt.Errorf("value of %q: %w", name, err)
		}
		// The name is untrusted: one invalid character breaks the whole /metrics page.
		rec.Gauge(metrics.SanitizeName(p.cfg.MetricPrefix+name), value, labels...)
		n++
	}
	return n, sc.Err()
}

// splitLabels strips a {a="1"} suffix off name and returns key-value pairs.
func splitLabels(name *string) ([]string, error) {
	open := strings.Index(*name, "{")
	if open < 0 {
		return nil, nil
	}
	if !strings.HasSuffix(*name, "}") {
		return nil, fmt.Errorf("unclosed label set in %q", *name)
	}
	body := (*name)[open+1 : len(*name)-1]
	*name = (*name)[:open]

	var out []string
	for _, pair := range strings.Split(body, ",") {
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("label %q is not key=value", pair)
		}
		k = strings.TrimSpace(k)
		if !metrics.ValidLabelName(k) {
			return nil, fmt.Errorf("label name %q is not usable", k)
		}
		out = append(out, k, strings.Trim(strings.TrimSpace(v), `"`))
	}
	return out, nil
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
