// Package grpc runs the standard gRPC health check against one backend,
// speaking the unary call directly over HTTP/2.
package grpc

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe/tlsinfo"
)

func init() { probe.Register("grpc", New) }

const (
	// maxDrain bounds the read-to-end that trailers require.
	maxBody  = 1 << 20
	maxDrain = 8 << 20
)

const (
	healthMethod  = "/grpc.health.v1.Health/Check"
	statusServing = 1
)

// Config is the `grpc:` block.
type Config struct {
	Port int `yaml:"port"`
	// Service is the name to ask about; empty means the whole server.
	Service   string            `yaml:"service"`
	Method    string            `yaml:"method"`
	Plaintext bool              `yaml:"plaintext"`
	Authority string            `yaml:"authority"`
	Headers   map[string]string `yaml:"headers"`
	TLS       tlsinfo.Options   `yaml:"tls"`
}

type Prober struct {
	cfg Config
	tls *tls.Config
}

func New(pc config.Probe) (probe.Prober, error) {
	// Port stays zero so an explicit target or backend port wins; 443 is the
	// fallback applied last in Address.
	cfg := Config{Method: healthMethod}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	tc, err := tlsinfo.Build(cfg.TLS)
	if err != nil {
		return nil, err
	}
	return &Prober{cfg: cfg, tls: tc}, nil
}

// Files implements probe.FileBacked.
func (p *Prober) Files() []string { return p.cfg.TLS.Files() }

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	addr, err := req.Address(p.cfg.Port, req.Backend.Port, req.Target.Port, 443)
	if err != nil {
		res.Err = err
		return res
	}
	authority := cmp.Or(p.cfg.Authority, req.Target.Host, addr)

	// Recorder is not safe for concurrent writes, and the dial callback runs on a
	// transport-owned goroutine, so state is recorded after RoundTrip returns.
	var (
		connectD, tlsD time.Duration
		dialErr        error
		tlsErr         error
		state          *tls.ConnectionState
		raw            net.Conn
	)
	// CloseIdleConnections skips a connection whose stream the deadline aborted.
	defer func() {
		if raw != nil {
			_ = raw.Close()
		}
	}()
	tr := &http2.Transport{
		AllowHTTP: p.cfg.Plaintext,
		DialTLSContext: func(ctx context.Context, network, _ string, cfg *tls.Config) (net.Conn, error) {
			start := time.Now()
			conn, err := req.Dialer(network).DialContext(ctx, network, addr)
			connectD = time.Since(start)
			if err != nil {
				// Kept so stageOf can name the stage that failed.
				dialErr = err
				return nil, err
			}
			raw = conn
			if p.cfg.Plaintext {
				return conn, nil
			}
			tc := p.tls.Clone()
			if tc.ServerName == "" {
				// A certificate names a host; an :authority may carry a port.
				tc.ServerName = hostOf(authority)
			}
			// Without h2 in ALPN the server may answer HTTP/1.1 and no stream starts.
			tc.NextProtos = []string{"h2"}

			tconn := tls.Client(conn, tc)
			start = time.Now()
			if err := tconn.HandshakeContext(ctx); err != nil {
				tlsD, tlsErr = time.Since(start), err
				return nil, err
			}
			tlsD = time.Since(start)
			st := tconn.ConnectionState()
			state = &st
			return tconn, nil
		},
	}
	defer tr.CloseIdleConnections()

	scheme := "https"
	if p.cfg.Plaintext {
		scheme = "http"
	}
	httpReq, rerr := p.request(ctx, scheme, authority)
	err = rerr
	if err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "%v", err)
		return res
	}

	start := time.Now()
	resp, err := tr.RoundTrip(httpReq)
	res.Add("connect", connectD)
	res.Add("tls", tlsD)
	tlsinfo.Record(rec, state, time.Now())
	if err != nil {
		res.Err = probe.Wrap(stageOf(dialErr, tlsErr), err)
		return res
	}
	defer resp.Body.Close()
	res.Add("ttfb", time.Since(start)-connectD-tlsD)

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonProtocol, err)
		return res
	}
	// Trailers exist only once the body has been read to the end.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))

	// grpc-status arrives in trailers for a normal call, in headers when the
	// server fails the call outright.
	code := cmp.Or(resp.Trailer.Get("grpc-status"), resp.Header.Get("grpc-status"))
	if code == "" {
		// A missing status is not OK.
		res.Err = probe.Fail(probe.ReasonProtocol, "the answer carried no grpc-status")
		return res
	}
	rec.Info("grpc_status_info", code)
	if code != "0" {
		message := cmp.Or(resp.Trailer.Get("grpc-message"), resp.Header.Get("grpc-message"))
		res.Err = probe.Fail(probe.ReasonStatus, "grpc-status %s %s", code, message)
		return res
	}

	status, err := parseHealthResponse(body)
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonProtocol, err)
		return res
	}
	rec.Gauge("grpc_serving_status", float64(status))
	if status != statusServing {
		res.Err = probe.Fail(probe.ReasonStatus, "serving status %d, want SERVING", status)
	}
	return res
}

func (p *Prober) request(ctx context.Context, scheme, authority string) (*http.Request, error) {
	url := fmt.Sprintf("%s://%s%s", scheme, authority, cmp.Or(p.cfg.Method, healthMethod))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		bytes.NewReader(frame(healthRequest(p.cfg.Service))))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/grpc")
	req.Header.Set("te", "trailers")
	req.Header.Set("user-agent", "argus-grpc/0.1")
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// frame applies gRPC framing: a compression flag byte then a big-endian length.
func frame(msg []byte) []byte {
	out := make([]byte, 5, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:], uint32(len(msg)))
	return append(out, msg...)
}

// healthRequest encodes HealthCheckRequest{string service = 1}.
func healthRequest(service string) []byte {
	if service == "" {
		return nil
	}
	out := []byte{1<<3 | 2} // field 1, length-delimited
	out = binary.AppendUvarint(out, uint64(len(service)))
	return append(out, service...)
}

// parseHealthResponse reads HealthCheckResponse{ServingStatus status = 1}.
func parseHealthResponse(b []byte) (uint64, error) {
	if len(b) < 5 {
		return 0, fmt.Errorf("short gRPC frame (%d bytes)", len(b))
	}
	msg := b[5:]
	if len(msg) == 0 {
		// proto3 omits a zero field, so an empty message means UNKNOWN (0).
		return 0, nil
	}
	if msg[0] != 1<<3 { // field 1, varint
		return 0, fmt.Errorf("unexpected field 0x%02x in health response", msg[0])
	}
	status, n := binary.Uvarint(msg[1:])
	if n <= 0 {
		return 0, fmt.Errorf("malformed serving status")
	}
	return status, nil
}

// stageOf names the stage a failed call stopped at.
func stageOf(dialErr, tlsErr error) probe.FailureReason {
	switch {
	case dialErr != nil:
		return probe.ReasonConnect
	case tlsErr != nil:
		return probe.ReasonTLS
	}
	return probe.ReasonProtocol
}

func hostOf(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return host
	}
	return authority
}
