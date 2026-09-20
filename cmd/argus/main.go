// Command argus is an active monitoring agent for domains.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/logging"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/server"
	"github.com/tonyamdfrost-cmd/Argus/internal/surfacer"

	// Probers register themselves, so adding a type costs one blank import.
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/dnsprobe"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/domain"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/external"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/grpc"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/http"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/icmp"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/tcp"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsprobe"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/udp"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/unixprobe"
	_ "github.com/tonyamdfrost-cmd/Argus/internal/probe/websocket"
)

// version is the release, plus the commit when the build carries no tag.
var version = app.VersionString()

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "argus:", err)
		os.Exit(1)
	}
}

func usage() string {
	return strings.TrimSpace(`
argus ` + version + ` - active monitoring for domains

  argus run       -config argus.yaml -probes probes.d/     run the node
  argus run       -config argus.yaml -state probes.yaml    run the node on what the API pushes
  argus validate  -config argus.yaml -probes probes.d/     check the configuration
  argus oneshot   -probes probes.d/ [-probe name]          run every probe once and print metrics
  argus kinds                                              list probe types
  argus version

-state names the file a configuration accepted through the API is written to.
It outlives a restart and wins over -probes, so a pushed node comes back with
what it was pushed and not with what is on disk.

SIGHUP reloads the check configuration in place.
`)
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage())
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return cmdRun(rest)
	case "validate":
		return cmdValidate(rest)
	case "oneshot":
		return cmdOneshot(rest)
	case "kinds":
		fmt.Println(strings.Join(probe.Kinds(), "\n"))
		return nil
	case "version":
		fmt.Printf("argus %s %s\n", version, runtime.Version())
		return nil
	case "-h", "--help", "help":
		fmt.Println(usage())
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage())
	}
}

type flags struct {
	server string
	probes multiFlag
	state  string
	probe  string
	level  string
	format string
}

// checks is the check configuration the node starts with, plus where it came
// from: the same two facts /status reports.
type checks struct {
	probes config.Probes
	source string
	hash   string
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func parseFlags(name string, args []string) (*flags, error) {
	f := &flags{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&f.server, "config", "", "path to the node configuration (argus.yaml)")
	fs.Var(&f.probes, "probes", "file or directory with checks; repeatable")
	fs.StringVar(&f.state, "state", "", "file a configuration accepted through the API is saved to; it wins over -probes at startup")
	fs.StringVar(&f.probe, "probe", "", "run only the probe with this name (oneshot)")
	fs.StringVar(&f.level, "log-level", "", "debug|info|warn|error")
	fs.StringVar(&f.format, "log-format", "", "json|text")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if len(f.probes) == 0 && f.state == "" {
		return nil, fmt.Errorf("-probes or -state is required")
	}
	return f, nil
}

func load(f *flags) (config.Server, checks, *slog.Logger, error) {
	srv, err := config.LoadServer(f.server)
	if err != nil {
		return srv, checks{}, nil, err
	}
	if f.level != "" {
		srv.Logging.Level = f.level
	}
	if f.format != "" {
		srv.Logging.Format = f.format
	}
	log, err := newLogger(srv)
	if err != nil {
		return srv, checks{}, nil, err
	}
	ck, err := loadChecks(f, log)
	return srv, ck, log, err
}

// loadChecks reads the state file first: what the API last accepted is what the
// node was told to run, and the files are only the fallback. A state file that
// cannot be used is reported and left on disk as evidence - refusing to start
// would take the node down over a file the operator never wrote.
func loadChecks(f *flags, log *slog.Logger) (checks, error) {
	if f.state != "" {
		data, err := os.ReadFile(f.state)
		switch {
		case err == nil && len(bytes.TrimSpace(data)) > 0:
			probes, perr := config.ParseProbes(data)
			if perr == nil {
				return checks{probes: probes, source: app.SourceAPI, hash: config.Hash(data)}, nil
			}
			log.Error("the saved configuration is unusable, falling back to the files",
				"path", f.state, "err", perr)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			log.Error("cannot read the saved configuration, falling back to the files",
				"path", f.state, "err", err)
		}
	}
	if len(f.probes) == 0 {
		// A node that has never been pushed to checks nothing until it is.
		return checks{source: app.SourceNone}, nil
	}
	probes, err := config.LoadProbes(f.probes...)
	if err != nil {
		// With a state file the files are only a fallback, and a node waiting
		// for its first push must not crash-loop over a directory nobody filled.
		if f.state == "" {
			return checks{}, err
		}
		log.Error("no usable check files, waiting for a configuration through the API", "err", err)
		return checks{source: app.SourceNone}, nil
	}
	// Files hash differently from a pushed document, so a backend comparing
	// hashes sees a mismatch and pushes once. That is the intended answer.
	hash, err := config.Fingerprint(f.probes...)
	if err != nil {
		return checks{}, err
	}
	return checks{probes: probes, source: app.SourceFile, hash: hash}, nil
}

func newLogger(c config.Server) (*slog.Logger, error) {
	fields := []any{"version", version}
	if c.Node.Name != "" {
		fields = append(fields, "node", c.Node.Name)
	}
	return logging.New(logging.Options{
		Level:  c.Logging.Level,
		Format: c.Logging.Format,
		Source: c.Logging.Source,
		Fields: fields,
	}, os.Stderr)
}

func cmdRun(args []string) error {
	f, err := parseFlags("run", args)
	if err != nil {
		return err
	}
	srvCfg, ck, log, err := load(f)
	if err != nil {
		return err
	}
	a, err := app.Build(srvCfg, ck.probes, log)
	if err != nil {
		return err
	}
	defer a.Close()
	a.SetConfigMeta(ck.source, ck.hash)

	// Reloading re-reads the same sources, state file included, so SIGHUP and
	// the API behave alike.
	reload := func() error {
		next, err := loadChecks(f, log)
		if err != nil {
			return err
		}
		if err := a.Reload(next.probes); err != nil {
			return err
		}
		a.SetConfigMeta(next.source, next.hash)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go watchHUP(ctx, a, reload, log)
	// Nothing to watch when the checks come from the API alone.
	if every := srvCfg.Probing.WatchConfig.D(); every > 0 && len(f.probes) > 0 {
		go watchFiles(ctx, f.probes, every, reload, log)
	}

	files := server.Files{Reload: reload, Persist: persister(f)}
	if files.Persist == nil {
		log.Info("checks come from a directory or several files, so the API cannot save changes back; " +
			"edit the files and reload instead")
	}
	httpSrv, err := server.New(a, files)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() {
		err := server.Serve(ctx, httpSrv, srvCfg.HTTP.TLS)
		if err != nil {
			stop()
		}
		errc <- err
	}()

	log.Info("argus started",
		"probes", len(a.Runners()),
		"config_source", ck.source,
		"targets", countTargets(a),
		"listen", srvCfg.HTTP.Listen,
		"tls", srvCfg.HTTP.TLS != nil,
		"api", srvCfg.HTTP.API.Enabled,
		"resolver", srvCfg.Resolver.Mode,
		"interval", srvCfg.Probing.Interval.String(),
		"max_concurrent", srvCfg.Probing.MaxConcurrent)

	// Blocks until a signal stops the context and every probe has finished.
	a.Start(ctx)
	log.Info("argus stopped", "uptime", a.Status().Uptime)
	return <-errc
}

// persister decides where a configuration accepted through the API is written:
// the state file when one was given, otherwise the single file the checks came
// from. A directory has no unambiguous destination, so there the API refuses.
func persister(f *flags) server.Persister {
	if f.state != "" {
		path := f.state
		return func(raw []byte) error {
			if dir := filepath.Dir(path); dir != "" && dir != "." {
				// The state directory is a volume on a fresh node: it may be
				// there, empty, or not there at all.
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return fmt.Errorf("save %s: %w", path, err)
				}
			}
			return config.SaveProbes(path, raw)
		}
	}
	if len(f.probes) != 1 {
		return nil
	}
	path := f.probes[0]
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return nil
	}
	return func(raw []byte) error { return config.SaveProbes(path, raw) }
}

func countTargets(a *app.App) int {
	n := 0
	for _, p := range a.Status().Probes {
		n += len(p.Targets)
	}
	return n
}

// watchFiles reloads when the check files change on disk. Off unless asked for.
func watchFiles(ctx context.Context, paths []string, every time.Duration, reload func() error, log *slog.Logger) {
	applied, err := config.Fingerprint(paths...)
	if err != nil {
		log.Error("cannot watch the check files", "err", err)
		return
	}
	seen := applied

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now, err := config.Fingerprint(paths...)
			if err != nil {
				// A file being written is not a file that changed.
				log.Warn("cannot read the check files", "err", err)
				continue
			}
			// A change has to hold still for one tick before it counts, so a
			// save in progress is not read as half a configuration.
			if now != seen {
				seen = now
				continue
			}
			if now == applied {
				continue
			}
			// Recorded before the reload: a rejected configuration is not retried.
			applied = now
			log.Info("check files changed, reloading")
			if err := reload(); err != nil {
				log.Error("reload failed, keeping the running configuration", "err", err)
			}
		}
	}
}

// watchHUP reloads the checks and reopens log files on SIGHUP.
func watchHUP(ctx context.Context, a *app.App, reload func() error, log *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			log.Info("SIGHUP received, reloading")
			if err := a.Reopen(); err != nil {
				log.Error("reopening log files failed", "err", err)
			}
			if err := reload(); err != nil {
				log.Error("reload failed, keeping the running configuration", "err", err)
			}
		}
	}
}

func cmdValidate(args []string) error {
	f, err := parseFlags("validate", args)
	if err != nil {
		return err
	}
	srvCfg, ck, log, err := load(f)
	if err != nil {
		return err
	}
	// Same build as a real run, so probers get to reject their own parameters.
	a, err := app.Build(srvCfg, ck.probes, log)
	if err != nil {
		return err
	}
	defer a.Close()

	st := a.Status()
	idle, err := checkIdle(ck.probes, srvCfg.Node.Name, st)
	if err != nil {
		return err
	}
	// A running node may legally check nothing, but a configuration handed to
	// validate is meant to do something here: say so instead of printing zero.
	if len(st.Probes) == 0 {
		return fmt.Errorf("no probe runs on node %q (%d disabled or meant for other nodes)",
			srvCfg.Node.Name, len(idle))
	}

	targets := 0
	for _, p := range st.Probes {
		targets += len(p.Targets)
	}
	fmt.Printf("configuration is valid: %d probe(s), %d target(s)\n", len(st.Probes), targets)
	for _, p := range st.Probes {
		fmt.Printf("  %-24s %-8s interval=%-6s timeout=%-6s targets=%d\n",
			p.Name, p.Type, p.Interval, p.Timeout, len(p.Targets))
	}
	for _, line := range idle {
		fmt.Println(line)
	}
	return nil
}

// checkIdle builds the probes this node will not run and describes them, so a
// disabled or foreign probe is still validated.
func checkIdle(probes config.Probes, node string, st app.Status) ([]string, error) {
	running := make(map[string]bool, len(st.Probes))
	for _, p := range st.Probes {
		running[p.Name] = true
	}
	list, err := probes.Resolve()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, pc := range list {
		if running[pc.Name] {
			continue
		}
		if _, err := probe.New(pc); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("  %-24s %-8s idle: %s", pc.Name, pc.Type, idleReason(pc, node)))
	}
	return out, nil
}

func idleReason(pc config.Probe, node string) string {
	if config.Enabled(pc.Disabled, false) {
		return "disabled"
	}
	return fmt.Sprintf("run_on %q does not match node %q", pc.RunOn, node)
}

func cmdOneshot(args []string) error {
	f, err := parseFlags("oneshot", args)
	if err != nil {
		return err
	}
	srvCfg, ck, log, err := load(f)
	if err != nil {
		return err
	}
	// A debugging run keeps no sink that would ship its measurements anywhere.
	srvCfg.Surfacers = slices.DeleteFunc(srvCfg.Surfacers, func(sf config.Surfacer) bool {
		return sf.Type != "prometheus"
	})
	a, err := app.Build(srvCfg, ck.probes, log)
	if err != nil {
		return err
	}
	defer a.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := a.RunOnce(ctx, f.probe); err != nil {
		return err
	}

	prom := &surfacer.Prometheus{Store: a.Store, Prefix: a.Cfg.Exposition().Prefix}
	_, err = prom.WriteTo(os.Stdout)
	return err
}
