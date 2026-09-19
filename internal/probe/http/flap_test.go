package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
)

// The set of gauges and infos must be identical across healthy runs.
func TestHTTPSeriesSetIsStable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = w.Write([]byte("<title>ok</title>"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	for _, opts := range []string{
		"{tls: {insecure_skip_verify: true}}",
		"{keep_alive: true, tls: {insecure_skip_verify: true}}",
	} {
		r := &probe.Runner{
			Name: "h", Kind: "http", Prober: newProber(t, opts),
			Source:   probe.StaticTargets{{Name: "t", Host: u.Hostname(), Port: port, URL: u}},
			Interval: time.Hour, Timeout: 5 * time.Second, Requests: 2,
			Resolver: resolve.Static{}, Family: resolve.Both, PerBackend: true,
			Buckets: metrics.Buckets{0.1, 1},
			Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		store := metrics.NewStore(time.Hour, 0)
		var want string
		for i := 0; i < 4; i++ {
			r.RunOnce(context.Background(), store)
			var set []string
			for _, s := range store.Snapshot() {
				switch s.Value.(type) {
				case metrics.Gauge, metrics.Info:
					set = append(set, s.Name+s.Labels.String())
				}
			}
			sort.Strings(set)
			got := strings.Join(set, "\n")
			if i == 0 {
				want = got
				if !strings.Contains(got, "probe_up") || !strings.Contains(got, "tls_cert") {
					t.Fatalf("first run wrote:\n%s", got)
				}
				continue
			}
			if got != want {
				t.Fatalf("%s: run %d changed the series set:\nwant\n%s\ngot\n%s", opts, i, want, got)
			}
		}
	}
}
