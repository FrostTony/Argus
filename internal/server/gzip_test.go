package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccepts(t *testing.T) {
	for h, want := range map[string]bool{
		"": false, "gzip": true, "deflate, gzip;q=1.0, *;q=0.5": true, "gzip;q=0": false,
		"br": false, "gzip; q=0.001": true, "x-gzip": false,
	} {
		if got := acceptsGzip(h); got != want {
			t.Errorf("%q: got %v", h, got)
		}
	}
}

func TestCompressed(t *testing.T) {
	body := strings.Repeat(`probe_phase_duration_seconds_bucket{backend="81.19.75.0:443",le="0.004"} 2`+"\n", 5000)
	h := compressed(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "123")
		io.WriteString(w, body)
	})
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Header().Get("Content-Encoding") != "gzip" || rr.Header().Get("Content-Length") != "" {
		t.Fatal(rr.Header())
	}
	zr, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatal(err)
	}
	n := rr.Body.Len()
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Fatal("body mismatch")
	}
	t.Logf("%d -> %d bytes", len(body), n)

	// empty body with a status is still valid gzip
	h2 := compressed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	rr = httptest.NewRecorder()
	h2(rr, req)
	if _, err := gzip.NewReader(rr.Body); err != nil {
		t.Fatal(err)
	}
	// no gzip requested
	rr = httptest.NewRecorder()
	h(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Body.String() != body || rr.Header().Get("Content-Encoding") != "" {
		t.Fatal("plain")
	}
	// real client through a server: transparent decompression
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != body || !resp.Uncompressed {
		t.Fatal("client", resp.Uncompressed, len(b))
	}
}
