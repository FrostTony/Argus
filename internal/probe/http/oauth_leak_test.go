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
)

// The token endpoint is asked once per token, not once per probe.
func TestTokenFetchLeavesNoConnection(t *testing.T) {
	var opened, closed atomic.Int64
	tokens := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"t","expires_in":1}`))
	}))
	tokens.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			opened.Add(1)
		case http.StateClosed:
			closed.Add(1)
		}
	}
	tokens.Start()
	defer tokens.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	p := newProber(t, `
auth:
  oauth2: {client_id: id, client_secret: s, token_url: "`+tokens.URL+`"}
`)
	for i := 0; i < 3; i++ {
		res := p.Probe(context.Background(), request(t, srv), metrics.NewRecorder(metrics.L("probe", "t")))
		if !res.OK() {
			t.Fatal(res.Err)
		}
		time.Sleep(time.Second) // the 1s token expires; the next probe refreshes it
	}
	if o, c := opened.Load(), closed.Load(); c < o {
		t.Fatalf("token endpoint: %d connections opened, %d closed — %d left open by discarded clients", o, c, o-c)
	}
}
