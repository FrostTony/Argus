package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// flaky fails with "timeout" first and with "connect" from then on.
type flaky struct{ calls *atomic.Int64 }

func (f flaky) Probe(context.Context, probe.Request, *metrics.Recorder) probe.Result {
	if f.calls.Add(1) == 1 {
		return probe.Result{Err: probe.Fail(probe.ReasonTimeout, "i/o timeout")}
	}
	return probe.Result{Err: probe.Fail(probe.ReasonConnect, "connection refused")}
}

// hang blocks until its context ends.
type hang struct{}

func (hang) Probe(ctx context.Context, _ probe.Request, _ *metrics.Recorder) probe.Result {
	<-ctx.Done()
	return probe.Result{Err: probe.Wrap(probe.ReasonTimeout, ctx.Err())}
}

func init() {
	probe.Register("flaky", func(config.Probe) (probe.Prober, error) { return flaky{new(atomic.Int64)}, nil })
	probe.Register("hang", func(config.Probe) (probe.Prober, error) { return hang{}, nil })
}

func reload(t *testing.T, a interface{ Reload(config.Probes) error }, doc string) {
	t.Helper()
	var p config.Probes
	if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(p); err != nil {
		t.Fatal(err)
	}
}

func TestStatusShowsTheCurrentFailureReason(t *testing.T) {
	a := newApp(t)
	reload(t, a, "probes:\n  - {name: f, type: flaky, targets: [\"1.1.1.1\"]}\n")
	for i := 0; i < 2; i++ { // 1st run: timeout, 2nd run: connect
		if err := a.RunOnce(context.Background(), "f"); err != nil {
			t.Fatal(err)
		}
	}
	page, err := newStatusPage(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range page.state().Probes {
		for _, tg := range p.Targets {
			for _, b := range tg.Backends {
				if b.Reason != "connect" {
					t.Errorf("current failure is %q, page says reason=%q", b.Error, b.Reason)
				}
			}
		}
	}
}

// The write deadline runs from when the headers were read.
func TestCheckAnswersInsideTheWriteDeadline(t *testing.T) {
	a := newAppWith(t, func(c *config.Server) { c.HTTP.Timeout = config.Duration(time.Second) })
	reload(t, a, "probes:\n  - {name: h, type: hang, targets: [\"1.1.1.1\"]}\n")

	srv, err := New(a, Files{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/api/check?probe=h", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no answer at all: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("status %d, body %q", resp.StatusCode, body)
	}
}

// A backend that left DNS keeps stale totals, which must not become a row.
func TestStatusHidesRetiredBackends(t *testing.T) {
	a := newApp(t)
	gone := metrics.L("probe", "one", "target", "1.1.1.1", "backend", "9.9.9.9:443")
	live := gone.With("backend", "1.1.1.1")
	up := metrics.NewRecorder(live)
	up.Gauge(probe.SeriesUp, 1)
	a.Store.Write(context.Background(), up.Batch(time.Now()))

	rec := metrics.NewRecorder(gone)
	rec.Gauge(probe.SeriesUp, 0)
	rec.Count(probe.SeriesTotal, 1)
	b := rec.Batch(time.Now())
	b.Scope = gone
	a.Store.Write(context.Background(), b)
	a.Store.Write(context.Background(), metrics.Retire(gone, time.Now()))

	page, err := newStatusPage(a)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, p := range page.state().Probes {
		for _, tg := range p.Targets {
			for _, bd := range tg.Backends {
				rows++
				if bd.Backend == "9.9.9.9:443" {
					t.Fatal("a retired backend is still on the page")
				}
			}
		}
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want the live backend alone", rows)
	}
}
