package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

// The transport dials the IDNA ASCII form of a Unicode host.
func TestUnicodeHostIsPinned(t *testing.T) {
	var host string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	req := request(t, srv) // backend = the test server
	su, _ := url.Parse(srv.URL)
	u, err := url.Parse("http://bücher.test:" + su.Port() + "/")
	if err != nil {
		t.Fatal(err)
	}
	req.Target.URL, req.Target.Host = u, "xn--bcher-kva.test"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := newProber(t, ``).Probe(ctx, req, metrics.NewRecorder(metrics.L("probe", "t")))
	if !res.OK() {
		t.Fatalf("the pinned backend was never dialled (server saw Host %q): %v", host, res.Err)
	}
}
