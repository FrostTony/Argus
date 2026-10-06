package domain

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// flaky is an RDAP registry that counts lookups and fails while down is set.
type flaky struct {
	url   string
	hits  atomic.Int32
	down  atomic.Int32 // an HTTP status to answer with, 0 for the document
	delay chan struct{}
}

func serveFlaky(t *testing.T, doc map[string]any) *flaky {
	t.Helper()
	f := &flaky{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.delay != nil {
			<-f.delay
		}
		if code := f.down.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func healthy() map[string]any {
	return answer(time.Now().Add(400*day), time.Now().Add(-400*day))
}

func TestAnAnswerIsReusedAcrossRunsAndTargets(t *testing.T) {
	reg := serveFlaky(t, healthy())
	p := newProber(t, "server: "+reg.url)

	for _, h := range []string{"example.com", "www.example.com", "api.example.com"} {
		res, samples := run(t, p, host(h))
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		if gauge(t, samples, "domain_expiry_days") < 399 {
			t.Errorf("%s: a cached answer lost its expiry", h)
		}
	}
	if n := reg.hits.Load(); n != 1 {
		t.Errorf("the registry was asked %d times for one domain", n)
	}
}

// A reload builds new probers; the registry must not notice.
func TestTheCacheOutlivesAReload(t *testing.T) {
	reg := serveFlaky(t, healthy())
	_ = newProber(t, "")
	for range 3 {
		p, err := New(probeConfig(t, "server: "+reg.url))
		if err != nil {
			t.Fatal(err)
		}
		if res, _ := run(t, p, host("example.com")); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	if n := reg.hits.Load(); n != 1 {
		t.Errorf("three generations asked %d times", n)
	}
}

func TestConcurrentLookupsShareOneRequest(t *testing.T) {
	reg := serveFlaky(t, healthy())
	reg.delay = make(chan struct{})
	p := newProber(t, "server: "+reg.url)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := run(t, p, host("example.com"))
			errs <- res.Err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(reg.delay)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := reg.hits.Load(); n != 1 {
		t.Errorf("8 concurrent probes made %d requests", n)
	}
}

func TestAnswersExpireAfterRefresh(t *testing.T) {
	reg := serveFlaky(t, healthy())
	p := newProber(t, "server: "+reg.url+"\nrefresh: 50ms")

	run(t, p, host("example.com"))
	time.Sleep(80 * time.Millisecond)
	_, samples := run(t, p, host("example.com"))
	if n := reg.hits.Load(); n != 2 {
		t.Errorf("asked %d times across a refresh, want 2", n)
	}
	if age := time.Since(time.Unix(int64(gauge(t, samples, "domain_fetched_seconds")), 0)); age > 2*time.Second {
		t.Errorf("domain_fetched_seconds is %v old after a fresh lookup", age)
	}
}

// A registry that went down has not changed the registration.
func TestAnOutageServesTheLastAnswer(t *testing.T) {
	reg := serveFlaky(t, healthy())
	p := newProber(t, "server: "+reg.url+"\nrefresh: 10ms")

	_, first := run(t, p, host("example.com"))
	reg.down.Store(http.StatusTooManyRequests)
	time.Sleep(20 * time.Millisecond)

	res, samples := run(t, p, host("example.com"))
	if res.Err != nil {
		t.Fatalf("a rate limit failed a known registration: %v", res.Err)
	}
	if got, want := gauge(t, samples, "domain_fetched_seconds"), gauge(t, first, "domain_fetched_seconds"); got != want {
		t.Errorf("domain_fetched_seconds = %v, want the original %v: staleness must show", got, want)
	}
}

// "Not registered" is an answer about the name, not an outage.
func TestARemovedDomainIsNotHiddenByTheCache(t *testing.T) {
	reg := serveFlaky(t, healthy())
	p := newProber(t, "server: "+reg.url+"\nrefresh: 10ms")

	run(t, p, host("example.com"))
	reg.down.Store(http.StatusNotFound)
	time.Sleep(20 * time.Millisecond)

	if res, _ := run(t, p, host("example.com")); probe.ReasonOf(res.Err) != probe.ReasonContent {
		t.Fatalf("want the 404 reported, got %v", res.Err)
	}
}

// Without a good answer to fall back on, a failure is remembered briefly, not forever.
func TestAFailureIsRetried(t *testing.T) {
	reg := serveFlaky(t, healthy())
	reg.down.Store(http.StatusInternalServerError)
	p := newProber(t, "server: "+reg.url+"\nrefresh: 30ms")

	if res, _ := run(t, p, host("example.com")); res.Err == nil {
		t.Fatal("a 500 passed")
	}
	if res, _ := run(t, p, host("example.com")); res.Err == nil {
		t.Fatal("the cached failure was forgotten at once")
	}
	reg.down.Store(0)
	time.Sleep(40 * time.Millisecond)
	if res, _ := run(t, p, host("example.com")); res.Err != nil {
		t.Fatalf("the failure outlived its retry: %v", res.Err)
	}
	if n := reg.hits.Load(); n != 2 {
		t.Errorf("asked %d times, want 2", n)
	}
}

func TestACancelledLookupIsNotRemembered(t *testing.T) {
	reg := serveFlaky(t, healthy())
	reg.delay = make(chan struct{})
	p := newProber(t, "server: "+reg.url)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if res := p.Probe(ctx, probe.Request{Target: host("example.com")}, metrics.NewRecorder(metrics.Labels{})); res.Err == nil {
		t.Fatal("a cancelled lookup succeeded")
	}
	close(reg.delay)
	if res, _ := run(t, p, host("example.com")); res.Err != nil {
		t.Fatalf("the cancellation was cached: %v", res.Err)
	}
}

func TestARegistrationNearItsEndIsRecheckedSooner(t *testing.T) {
	p := newProber(t, "min_days: 30\nrefresh: 6h").(*Prober)
	cases := []struct {
		left time.Duration
		want time.Duration
	}{
		{400 * day, 6 * time.Hour},
		{20 * day, retryAfter},
		{-day, retryAfter},
	}
	for _, c := range cases {
		if got := p.ttl(&record{expiry: time.Now().Add(c.left)}); got != c.want {
			t.Errorf("%v left: ttl %v, want %v", c.left, got, c.want)
		}
	}
	if got := p.ttl(nil); got != retryAfter {
		t.Errorf("a failure is believed for %v", got)
	}
}

func TestRefreshMustBePositive(t *testing.T) {
	if _, err := New(probeConfig(t, "refresh: 0s")); err == nil {
		t.Error("refresh: 0s was accepted")
	}
}

// Queries to one WHOIS server go one at a time.
func TestWHOISQueriesAreSerialised(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var open, peak atomic.Int32
	body := tciAnswer(time.Now().Add(300*day), time.Now().Add(-day))
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = bufio.NewReader(conn).ReadString('\n')
				n := open.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(20 * time.Millisecond)
				open.Add(-1)
				conn.Write([]byte(body))
			}()
		}
	}()

	p := newProber(t, "whois: {ru: "+ln.Addr().String()+"}")
	var wg sync.WaitGroup
	for _, name := range []string{"a.ru", "b.ru", "c.ru", "d.ru"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res, _ := run(t, p, host(name)); res.Err != nil {
				t.Error(res.Err)
			}
		}()
	}
	wg.Wait()
	if n := peak.Load(); n != 1 {
		t.Errorf("%d connections were open at once", n)
	}
}

// IDNA rejects underscores; a name DNS serves must still reduce to its domain.
func TestUnderscoreNamesAreAccepted(t *testing.T) {
	name, err := registrable(probe.Target{Name: "x", Host: "_dmarc.example.com"})
	if err != nil || name != "example.com" {
		t.Errorf("got %q, %v", name, err)
	}
}
