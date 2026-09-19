package http

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// An HTTP/2 stream aborted by the deadline is torn down asynchronously.
func TestTimedOutH2ConnectionIsClosed(t *testing.T) {
	var opened, closed atomic.Int64
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			opened.Add(1)
		case http.StateClosed:
			closed.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	p := newProber(t, `{keep_alive: true, tls: {insecure_skip_verify: true}}`)
	req := request(t, srv)
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		res := p.Probe(ctx, req, metrics.NewRecorder(metrics.L("probe", "t")))
		cancel()
		if got := probe.ReasonOf(res.Err); got != probe.ReasonTimeout {
			t.Fatalf("reason = %q, want timeout (%v)", got, res.Err)
		}
	}
	time.Sleep(time.Second)
	if o, c := opened.Load(), closed.Load(); c < o {
		t.Fatalf("%d connections opened, %d closed: %d still open a second after every probe returned", o, c, o-c)
	}
}
