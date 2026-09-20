// Package server exposes metrics, node status and the check-configuration API.
package server

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/app"
	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/surfacer"
)

// Reloader reloads the check configuration from its original files.
type Reloader func() error

// Persister writes an accepted configuration back to the file it came from.
type Persister func(raw []byte) error

// Files reaches the check configuration on disk; either hook may be nil.
type Files struct {
	Reload  Reloader
	Persist Persister
}

// New builds the node's HTTP surface without listening on anything.
func New(a *app.App, files Files) (*http.Server, error) {
	mux := http.NewServeMux()

	exposition := a.Cfg.Exposition()
	prom := &surfacer.Prometheus{
		Store:      a.Store,
		Prefix:     exposition.Prefix,
		Timestamps: exposition.IncludeTimestamps,
	}
	mux.HandleFunc(a.Cfg.HTTP.MetricsPath, func(w http.ResponseWriter, _ *http.Request) {
		a.RefreshSelf()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		bw := bufio.NewWriterSize(w, 64<<10)
		_, _ = prom.WriteTo(bw)
		_ = bw.Flush()
	})

	mux.HandleFunc("/logo.png", serveLogo)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/status.json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, a.Status())
	})

	page, err := newStatusPage(a)
	if err != nil {
		return nil, err
	}
	mux.HandleFunc("/", page.handle)
	mux.HandleFunc("/status", page.handle)
	mux.HandleFunc("/status/data", page.data)

	if cfg := a.Cfg.HTTP.API; cfg.Enabled {
		api := &api{app: a, files: files, cfg: cfg, timeout: a.Cfg.HTTP.Timeout.D()}
		// config and check pick their permission per request; the rest are writes.
		mux.Handle("/api/config", http.HandlerFunc(api.config))
		mux.Handle("/api/reload", api.write(api.reloadHandler))
		mux.Handle("/api/probes", api.write(api.probes))
		mux.Handle("/api/check", http.HandlerFunc(api.check))
		if cfg.ProbeEndpoint {
			mux.Handle("/probe", http.HandlerFunc(api.probeExposition))
		}

		if len(cfg.WriteTokens()) == 0 {
			a.Log.Warn("config API accepts unauthenticated writes: anyone who can reach this port can repoint the node",
				"listen", a.Cfg.HTTP.Listen)
		}
	}

	srv := &http.Server{
		Addr:    a.Cfg.HTTP.Listen,
		Handler: mux,
		// WriteTimeout bounds nothing on the way in, so the read deadlines matter.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       max(30*time.Second, a.Cfg.HTTP.Timeout.D()),
		WriteTimeout:      a.Cfg.HTTP.Timeout.D(),
		IdleTimeout:       60 * time.Second,
	}
	if t := a.Cfg.HTTP.TLS; t != nil {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if t.ClientCAFile != "" {
			pool, err := loadCA(t.ClientCAFile)
			if err != nil {
				return nil, err
			}
			srv.TLSConfig.ClientCAs = pool
			srv.TLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}
	return srv, nil
}

func loadCA(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("http.tls.client_ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("http.tls.client_ca_file: no certificates in %s", path)
	}
	return pool, nil
}

const maxConfigBody = 8 << 20

type api struct {
	app     *app.App
	files   Files
	cfg     config.API
	timeout time.Duration
}

// answerMargin is held back from a request's time for writing the answer.
const answerMargin = time.Second

// checkTimeout keeps an on-demand run inside the window the connection has.
func (s *api) checkTimeout(r *http.Request) time.Duration {
	limit := cmp.Or(s.timeout, time.Minute)
	// Compared as seconds: a value past what a Duration holds goes negative.
	if secs, err := strconv.ParseFloat(r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds"), 64); err == nil && secs > 0 && secs < limit.Seconds() {
		limit = time.Duration(secs * float64(time.Second))
	}
	return limit - min(answerMargin, limit/2)
}

// check runs one probe once, storing nothing. Running a configured probe as
// configured is a read; a definition, a target or a hostname override is a write.
func (s *api) check(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{"use GET or POST"})
		return
	}
	req := app.CheckRequest{
		Probe:    r.URL.Query().Get("probe"),
		Target:   r.URL.Query().Get("target"),
		Hostname: r.URL.Query().Get("hostname"),
		Debug:    r.URL.Query().Get("debug") == "true",
	}

	// A body, when present, is a probe definition of its own.
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigBody))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
			return
		}
		if len(bytes.TrimSpace(body)) > 0 {
			spec, err := config.ParseProbe(body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
				return
			}
			req.Spec = &spec
		}
	}

	tokens := s.cfg.ReadTokens()
	if req.Spec != nil || req.Target != "" || req.Hostname != "" || req.Debug {
		tokens = s.cfg.WriteTokens()
	}
	if !s.authorized(r, tokens) {
		writeJSON(w, http.StatusUnauthorized, errorBody{"unauthorized"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.checkTimeout(r))
	defer cancel()

	res, err := s.app.Check(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
		return
	}
	// The HTTP status describes the API call, not the check's outcome.
	writeJSON(w, http.StatusOK, res)
}

// render writes one set of samples. The aliases go through a store of their own
// so that they can be written without this node's prefix.
func render(ctx context.Context, w io.Writer, samples []metrics.Sample, prefix string, timestamps bool) {
	if len(samples) == 0 {
		return
	}
	store := metrics.NewStore(0, 0)
	store.Write(ctx, metrics.Batch{Time: time.Now(), Samples: samples})
	prom := &surfacer.Prometheus{Store: store, Prefix: prefix, Timestamps: timestamps}
	_, _ = prom.WriteTo(w)
}

// comment folds a log record onto one line: anything that ends a line would end
// the comment with it and leave the rest to be parsed as a series.
func comment(line string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, line)
}

// probeExposition runs one check and renders it as a Prometheus exposition,
// storing nothing.
func (s *api) probeExposition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{"use GET"})
		return
	}
	// "module" is accepted as an alias for "probe".
	q := r.URL.Query()
	req := app.CheckRequest{
		Probe:    cmp.Or(q.Get("probe"), q.Get("module")),
		Target:   q.Get("target"),
		Hostname: q.Get("hostname"),
		Debug:    q.Get("debug") == "true",
	}
	// Choosing a target or the name presented to it sends the probe's
	// credentials somewhere it was not configured to send them: a write. So is
	// the trace, which carries what the target answered and what was asked of
	// it — more than the measurement a read token is for.
	tokens := s.cfg.ReadTokens()
	if req.Target != "" || req.Hostname != "" || req.Debug {
		tokens = s.cfg.WriteTokens()
	}
	if !s.authorized(r, tokens) {
		writeJSON(w, http.StatusUnauthorized, errorBody{"unauthorized"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.checkTimeout(r))
	defer cancel()

	exp, err := s.app.Samples(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
		return
	}
	exposition := s.app.Cfg.Exposition()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	bw := bufio.NewWriterSize(w, 16<<10)
	// The trace goes out as comments, so that a debug scrape is still an
	// exposition: whoever is holding curl reads the log, Prometheus ignores it.
	for _, line := range exp.Log {
		_, _ = fmt.Fprintf(bw, "# %s\n", comment(line))
	}
	render(ctx, bw, exp.Samples, exposition.Prefix, exposition.IncludeTimestamps)
	render(ctx, bw, exp.Aliases, "", exposition.IncludeTimestamps)
	_ = bw.Flush()
}

// write wraps a handler behind the write tokens.
func (s *api) write(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r, s.cfg.WriteTokens()) {
			writeJSON(w, http.StatusUnauthorized, errorBody{"unauthorized"})
			return
		}
		h(w, r)
	})
}

// authorized checks the bearer token against the ones that grant this action;
// an empty list needs none. Every candidate is compared in constant time.
func (s *api) authorized(r *http.Request, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	got := []byte(r.Header.Get("Authorization"))
	ok := false
	for _, t := range tokens {
		if t == "" {
			continue
		}
		if subtle.ConstantTimeCompare(got, []byte("Bearer "+t)) == 1 {
			ok = true
		}
	}
	return ok
}

// config serves the running check configuration on GET and replaces it on PUT.
// A rejected configuration changes nothing.
func (s *api) config(w http.ResponseWriter, r *http.Request) {
	tokens := s.cfg.WriteTokens()
	if r.Method == http.MethodGet {
		tokens = s.cfg.ReadTokens()
	}
	if !s.authorized(r, tokens) {
		writeJSON(w, http.StatusUnauthorized, errorBody{"unauthorized"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		data, err := yaml.Marshal(s.app.Probes())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(data)

	case http.MethodPut, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigBody))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
			return
		}
		// Parsed the same strict way a file is, so a typo is refused here too.
		probes, err := config.ParseProbes(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
			return
		}
		// A change that cannot be written is refused, so nothing drifts.
		if s.files.Persist == nil {
			writeJSON(w, http.StatusConflict, errorBody{
				"this node has no single check file to write to, so a change through the API " +
					"would not survive a restart; edit the file and reload instead"})
			return
		}
		// Applied before it is written: an unrunnable config must not be stored.
		if err := s.app.Reload(probes); err != nil {
			s.app.Log.Warn("config rejected", "from", clientOf(r), "bytes", len(body), "err", err)
			writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
			return
		}
		if err := s.files.Persist(body); err != nil {
			s.app.Log.Error("config applied but not saved", "from", clientOf(r), "err", err)
			writeJSON(w, http.StatusInternalServerError, errorBody{
				"the node is running this configuration but could not save it, " +
					"so a restart will undo it: " + err.Error()})
			return
		}
		// Recorded only now: the hash must describe a configuration that is
		// both running and saved.
		s.app.SetConfigMeta(app.SourceAPI, config.Hash(body))
		s.app.Log.Info("config replaced via api",
			"from", clientOf(r), "bytes", len(body), "probes", len(probes.List),
			"hash", config.Hash(body))
		writeJSON(w, http.StatusOK, s.app.Status())

	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{"use GET or PUT"})
	}
}

func (s *api) reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{"use POST"})
		return
	}
	if s.files.Reload == nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"no configuration files to reload from"})
		return
	}
	if err := s.files.Reload(); err != nil {
		s.app.Log.Warn("reload rejected", "from", clientOf(r), "err", err)
		writeJSON(w, http.StatusBadRequest, errorBody{err.Error()})
		return
	}
	s.app.Log.Info("reloaded via api", "from", clientOf(r))
	writeJSON(w, http.StatusOK, s.app.Status())
}

// probes runs one probe on demand, without waiting for its interval.
func (s *api) probes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{"use POST"})
		return
	}
	name := r.URL.Query().Get("name")
	ctx, cancel := context.WithTimeout(r.Context(), s.checkTimeout(r))
	defer cancel()
	if err := s.app.RunOnce(ctx, name); err != nil {
		// 404 means unconfigured, so a busy probe must answer 409 instead.
		code := http.StatusNotFound
		if errors.Is(err, app.ErrBusy) {
			code = http.StatusConflict
		}
		writeJSON(w, code, errorBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ran": cmp.Or(name, "all")})
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// clientOf identifies the caller for the audit log; the proxy header is
// recorded as given, never acted on.
func clientOf(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return r.RemoteAddr + " (xff " + fwd + ")"
	}
	return r.RemoteAddr
}

// Serve starts the listener and shuts it down when the context is cancelled.
func Serve(ctx context.Context, srv *http.Server, tlsCfg *config.ServerTLS) error {
	errc := make(chan error, 1)
	go func() {
		var err error
		if tlsCfg != nil {
			err = srv.ListenAndServeTLS(tlsCfg.CertFile, tlsCfg.KeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			errc <- fmt.Errorf("http server: %w", err)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
