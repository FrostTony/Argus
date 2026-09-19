package grpc

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func newProber(t *testing.T, options string) probe.Prober {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "grpc"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func h2server(t *testing.T, h http.HandlerFunc, state func(net.Conn, http.ConnState)) probe.Request {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.Config.ConnState = state
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return probe.Request{
		Target:  probe.Target{Name: "t", Host: "127.0.0.1", Port: port},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
		Buckets: metrics.Buckets{0.1, 1},
	}
}

func TestFailureAfterTheHandshakeIsNotTLS(t *testing.T) {
	req := h2server(t, func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler) // RST_STREAM
	}, nil)
	p := newProber(t, `{tls: {insecure_skip_verify: true}}`)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := p.Probe(ctx, req, metrics.NewRecorder(metrics.Labels{}))
	if res.OK() {
		t.Fatal("expected a failure")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonProtocol {
		t.Fatalf("reason = %q, want %q (err: %v)", got, probe.ReasonProtocol, res.Err)
	}
}

func TestTimedOutConnectionIsClosed(t *testing.T) {
	var opened, closed atomic.Int64
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	req := h2server(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			opened.Add(1)
		case http.StateClosed, http.StateHijacked:
			closed.Add(1)
		}
	})
	p := newProber(t, `{tls: {insecure_skip_verify: true}}`)

	const runs = 10
	for i := 0; i < runs; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		res := p.Probe(ctx, req, metrics.NewRecorder(metrics.Labels{}))
		cancel()
		if got := probe.ReasonOf(res.Err); got != probe.ReasonTimeout {
			t.Fatalf("reason = %q, want timeout (%v)", got, res.Err)
		}
	}
	time.Sleep(time.Second)
	buf := make([]byte, 1<<20)
	stacks := string(buf[:runtime.Stack(buf, true)])
	readLoops := strings.Count(stacks, "http2.(*clientConnReadLoop).run")
	if o, c := opened.Load(), closed.Load(); c < o || readLoops > 0 {
		t.Fatalf("%d connections opened, only %d closed a second after every probe returned: %d leaked; "+
			"%d client readLoop goroutine(s) still alive",
			o, c, o-c, readLoops)
	}
}

// An :authority may carry a port; the certificate is verified against the host.
func TestAuthorityWithPortStillVerifies(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		_, _ = w.Write(frame([]byte{1 << 3, statusServing}))
		w.Header().Set("Grpc-Status", "0")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	req := probe.Request{
		Target:  probe.Target{Name: "t", Host: "example.com", Port: port},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Control: the same server with a bare authority.
	ok := newProber(t, `{authority: "example.com", tls: {ca_cert: "`+ca+`"}}`)
	if res := ok.Probe(ctx, req, metrics.NewRecorder(metrics.Labels{})); !res.OK() {
		t.Fatalf("control failed: %v", res.Err)
	}
	p := newProber(t, `{authority: "example.com:`+u.Port()+`", tls: {ca_cert: "`+ca+`"}}`)
	if res := p.Probe(ctx, req, metrics.NewRecorder(metrics.Labels{})); !res.OK() {
		t.Fatalf("authority with a port: %v", res.Err)
	}
}
