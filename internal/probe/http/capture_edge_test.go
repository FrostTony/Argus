package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// notLoopback2 plays the public-only policy where every test server is local:
// 127.0.0.1 stands in for the internet, 127.0.0.2 for the node's own network.
func notLoopback2(a netip.Addr) bool { return a != netip.MustParseAddr("127.0.0.2") }

func serveOn(t *testing.T, addr string, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// A redirect off the domain is not pinned; the site under test must not be
// able to send the capture into the node's network.
func TestCaptureCannotBeRedirectedInward(t *testing.T) {
	secret := serveOn(t, "127.0.0.2:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("AWS_SECRET_ACCESS_KEY=hunter2"))
	}))
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secret.URL+"/latest/meta-data/", http.StatusFound)
	}))
	defer evil.Close()

	res, got := captureWith(t, newProber(t, ``), request(t, evil), &probe.Capture{Allow: notLoopback2})
	if probe.ReasonOf(res.Err) != probe.ReasonConnect || !strings.Contains(res.Err.Error(), "127.0.0.2") {
		t.Errorf("err = %v, want the inward dial refused", res.Err)
	}
	for _, a := range got {
		if bytes.Contains(a.Data, []byte("hunter2")) {
			t.Fatalf("the secret leaked into %s", a.Name)
		}
	}
	// The same redirect without capture is what monitoring always did.
	if res := newProber(t, ``).Probe(context.Background(), request(t, evil), metrics.NewRecorder(metrics.Labels{})); res.Err != nil {
		t.Errorf("a scheduled run was vetted: %v", res.Err)
	}
}

func TestCaptureCannotGoThroughAnInwardProxy(t *testing.T) {
	proxy := serveOn(t, "127.0.0.2:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("internal"))
	}))
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	res, got := captureWith(t, newProber(t, `proxy_url: `+proxy.URL), request(t, target), &probe.Capture{Allow: notLoopback2})
	if res.Err == nil || len(got) != 0 {
		t.Errorf("capture went through the proxy: err=%v artifacts=%d", res.Err, len(got))
	}
}

// Compression is undone before the limit applies, so a small bomb stays small.
func TestCaptureSurvivesAGzipBomb(t *testing.T) {
	var bomb bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&bomb, gzip.BestCompression)
	_, _ = zw.Write(make([]byte, 64<<20))
	_ = zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(bomb.Bytes())
	}))
	defer srv.Close()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body := capture(t, newProber(t, "compression: true\nmax_body_bytes: 1099511627776"), request(t, srv))["body"]
	runtime.ReadMemStats(&after)

	if len(body.Data) != probe.CaptureLimit || !body.Truncated {
		t.Errorf("kept %d bytes, truncated=%v; want the capture limit, cut", len(body.Data), body.Truncated)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
		t.Errorf("a %d KiB bomb cost %d MiB", bomb.Len()>>10, grew>>20)
	}
}

func TestCaptureLimitHoldsWhateverTheProbeAllows(t *testing.T) {
	page := bytes.Repeat([]byte("x"), probe.CaptureLimit+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(page) }))
	defer srv.Close()

	// A body validator keeps all it needs; the artifact still stops at the limit.
	for _, options := range []string{
		`max_body_bytes: 1099511627776`,
		"max_body_bytes: 1099511627776\nvalidators: [{name: v, body_regex: 'x$'}]",
	} {
		res, got := captureWith(t, newProber(t, options), request(t, srv), &probe.Capture{})
		if res.Err != nil {
			t.Errorf("%q: %v", options, res.Err)
		}
		if b := got["body"]; len(b.Data) != probe.CaptureLimit || !b.Truncated {
			t.Errorf("%q: kept %d, truncated=%v", options, len(b.Data), b.Truncated)
		}
	}
}

func TestCaptureBoundsTheHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := range 200 {
			w.Header().Set(fmt.Sprintf("X-Pad-%03d", i), strings.Repeat("v", 1000))
		}
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/next", http.StatusFound)
		}
	}))
	defer srv.Close()

	h := capture(t, newProber(t, ``), request(t, srv))["headers"]
	if len(h.Data) > exchangeLimit || !h.Truncated {
		t.Fatalf("%d bytes, truncated=%v", len(h.Data), h.Truncated)
	}
	if !bytes.HasSuffix(h.Data, []byte("\n")) || bytes.Count(h.Data, []byte("X-Pad-")) == 0 {
		t.Errorf("cut mid-line or empty:\n%.200s", h.Data)
	}
}

func TestCaptureOfAnswersWithoutABody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.Method == http.MethodHead || r.URL.Path == "/empty" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}))
	defer srv.Close()

	for _, options := range []string{`method: HEAD`, `path: /empty`} {
		res, got := captureWith(t, newProber(t, options+"\nvalidators: [{name: s, status_code: ['204']}]"), request(t, srv), &probe.Capture{})
		if res.Err != nil {
			t.Fatalf("%s: %v", options, res.Err)
		}
		if b := got["body"]; len(b.Data) != 0 || b.Truncated {
			t.Errorf("%s: body %+v", options, b)
		}
		if !bytes.Contains(got["headers"].Data, []byte("204 No Content")) {
			t.Errorf("%s: headers %s", options, got["headers"].Data)
		}
	}
}

// What was read before the connection broke is the most useful part.
func TestCaptureKeepsWhatArrivedBeforeTheCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("<h1>partial"))
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()

	res, got := captureWith(t, newProber(t, ``), request(t, srv), &probe.Capture{})
	if probe.ReasonOf(res.Err) != probe.ReasonContent {
		t.Errorf("err = %v", res.Err)
	}
	if string(got["body"].Data) != "<h1>partial" {
		t.Errorf("body = %q", got["body"].Data)
	}
}

func TestCaptureGuessesAnUndeclaredType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil // net/http would sniff one in
		_, _ = w.Write(png)
	}))
	defer srv.Close()

	b := capture(t, newProber(t, ``), request(t, srv))["body"]
	if b.MediaType != "image/png" || !bytes.Equal(b.Data, png) {
		t.Errorf("body = %+v", b)
	}
}

func TestCaptureOverHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("over h2"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	got := capture(t, newProber(t, "tls: {insecure_skip_verify: true}"), request(t, srv))
	if string(got["body"].Data) != "over h2" || !bytes.Contains(got["headers"].Data, []byte("< HTTP/2.0 200 OK")) {
		t.Errorf("body %q, headers:\n%s", got["body"].Data, got["headers"].Data)
	}
}

// One prober serves every backend of a target at once.
func TestCaptureIsSafeConcurrently(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Query().Get("n")))
	}))
	defer srv.Close()

	p := newProber(t, ``)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := request(t, srv)
			u := *req.Target.URL
			u.RawQuery = fmt.Sprintf("n=%d", i)
			req.Target.URL = &u
			_, got := captureWith(t, p, req, &probe.Capture{})
			if want := fmt.Sprint(i); string(got["body"].Data) != want {
				t.Errorf("request %d got %q", i, got["body"].Data)
			}
		}()
	}
	wg.Wait()
}

func FuzzExchangeStaysBounded(f *testing.F) {
	f.Add("X-A", "b", 3, "GET", "/p")
	f.Fuzz(func(t *testing.T, key, value string, hops int, method, path string) {
		var resp *http.Response
		for i := range hops % 50 {
			req := &http.Request{Method: method, URL: &url.URL{Scheme: "http", Host: "h", Path: path}, Response: resp}
			resp = &http.Response{Proto: "HTTP/1.1", Status: "200 OK", Request: req,
				Header: http.Header{key: {strings.Repeat(value, 1+i)}}}
		}
		if resp == nil {
			return
		}
		data, cut := exchange(resp)
		if len(data) > exchangeLimit {
			t.Fatalf("%d bytes", len(data))
		}
		if !cut && len(data) > 0 && !bytes.HasSuffix(data, []byte("\n\n")) {
			t.Fatalf("an uncut exchange ends mid-hop: %q", data)
		}
	})
}

// The length is the server's word; neither a lie nor an overflow costs the node.
func TestCaptureDistrustsTheDeclaredLength(t *testing.T) {
	for _, declared := range []string{"9223372036854775807", "4294967296"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", declared)
			_, _ = w.Write([]byte("tiny"))
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		}))
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, got := captureWith(t, newProber(t, "max_body_bytes: 1099511627776\nvalidators: [{name: v, body_regex: x}]"),
			request(t, srv), &probe.Capture{})
		runtime.ReadMemStats(&after)
		srv.Close()

		if string(got["body"].Data) != "tiny" || res.Err == nil {
			t.Errorf("Content-Length %s: body %q, err %v", declared, got["body"].Data, res.Err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
			t.Errorf("Content-Length %s cost %d MiB", declared, grew>>20)
		}
	}
}

func TestCaptureRedactsCredentialsInURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	req := request(t, srv)
	u := *req.Target.URL
	u.User = url.UserPassword("monitor", "s3cret")
	req.Target.URL = &u
	for name, a := range capture(t, newProber(t, ``), req) {
		if bytes.Contains(a.Data, []byte("s3cret")) || strings.Contains(a.URL, "s3cret") {
			t.Errorf("%s carries the password: %s %s", name, a.URL, a.Data)
		}
	}
}
