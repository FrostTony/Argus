package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func capture(t *testing.T, p probe.Prober, req probe.Request) map[string]probe.Artifact {
	t.Helper()
	_, out := captureWith(t, p, req, &probe.Capture{})
	return out
}

func captureWith(t *testing.T, p probe.Prober, req probe.Request, c *probe.Capture) (probe.Result, map[string]probe.Artifact) {
	t.Helper()
	req.Capture = c
	res := p.Probe(context.Background(), req, metrics.NewRecorder(metrics.Labels{}))
	out := map[string]probe.Artifact{}
	for _, a := range res.Artifacts {
		out[a.Name] = a
	}
	return res, out
}

func TestCaptureFollowsTheRedirectToThePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/blocked", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
		_, _ = w.Write([]byte("<h1>\xc4\xee\xf1\xf2\xf3\xef \xee\xe3\xf0\xe0\xed\xe8\xf7\xe5\xed</h1>"))
	}))
	defer srv.Close()

	// The status fails the check; the page is what explains why.
	got := capture(t, newProber(t, ``), request(t, srv))

	body := got["body"]
	if string(body.Data) != "<h1>\xc4\xee\xf1\xf2\xf3\xef \xee\xe3\xf0\xe0\xed\xe8\xf7\xe5\xed</h1>" {
		t.Errorf("body = %q, want the bytes as served", body.Data)
	}
	if body.MediaType != "text/html; charset=windows-1251" || !strings.HasSuffix(body.URL, "/blocked") || body.Truncated {
		t.Errorf("body = %+v", body)
	}

	headers := string(got["headers"].Data)
	for _, want := range []string{"> GET " + srv.URL + "\n", "< HTTP/1.1 302 Found\n", "Location: /blocked\n",
		"> GET " + srv.URL + "/blocked\n", "< HTTP/1.1 451 Unavailable For Legal Reasons\n"} {
		if !strings.Contains(headers, want) {
			t.Errorf("headers lack %q:\n%s", want, headers)
		}
	}
	if strings.Index(headers, "302 Found") > strings.Index(headers, "451 Unavailable") {
		t.Errorf("hops out of order:\n%s", headers)
	}
}

func TestCaptureMarksACutBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 100)))
	}))
	defer srv.Close()

	for _, options := range []string{`max_body_bytes: 10`, "read_body: false\nmax_body_bytes: 10"} {
		body := capture(t, newProber(t, options), request(t, srv))["body"]
		if len(body.Data) != 10 || !body.Truncated {
			t.Errorf("%q: %d bytes, truncated=%v", options, len(body.Data), body.Truncated)
		}
	}
	whole := capture(t, newProber(t, `max_body_bytes: 100`), request(t, srv))["body"]
	if len(whole.Data) != 100 || whole.Truncated {
		t.Errorf("a body that fits: %d bytes, truncated=%v", len(whole.Data), whole.Truncated)
	}
	if whole.MediaType != "text/plain; charset=utf-8" {
		t.Errorf("media type = %q", whole.MediaType)
	}
}

func TestNoCaptureNoArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	res := newProber(t, ``).Probe(context.Background(), request(t, srv), metrics.NewRecorder(metrics.Labels{}))
	if res.Artifacts != nil {
		t.Errorf("a scheduled run kept %d artifacts", len(res.Artifacts))
	}
}

// chunks hands out its data a few bytes at a time, ending with or after the last.
type chunks struct {
	data    []byte
	step    int
	eofWith bool
}

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.data[:min(len(c.data), c.step)])
	c.data = c.data[n:]
	if c.eofWith && len(c.data) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func TestReadBody(t *testing.T) {
	page := bytes.Repeat([]byte("0123456789"), 10_000) // 100 kB
	for _, tc := range []struct {
		name            string
		limit, declared int64
		want            int
	}{
		{"declared", 1 << 20, int64(len(page)), len(page)},
		{"undeclared", 1 << 20, -1, len(page)},
		{"cut", 1000, -1, 1000},
		{"cut, declared", 1000, int64(len(page)), 1000},
		{"declared short", 1 << 20, 10, len(page)},
		{"declared long", 1 << 20, 1 << 30, len(page)},
		{"declared empty", 1 << 20, 0, len(page)},
		{"declared past int64", 1 << 20, 1<<63 - 1, len(page)},
		{"limit past int64", 1<<63 - 1, 1<<63 - 1, len(page)},
	} {
		for _, eofWith := range []bool{false, true} {
			got, err := readBody(&chunks{data: page, step: 4096, eofWith: eofWith}, tc.limit, tc.declared)
			if err != nil || !bytes.Equal(got, page[:tc.want]) {
				t.Errorf("%s (eof with data %v): %d bytes, %v", tc.name, eofWith, len(got), err)
			}
			if int64(cap(got)) > trustDeclared+bytes.MinRead {
				t.Errorf("%s: cap %d for limit %d", tc.name, cap(got), tc.limit)
			}
		}
	}
}

// A declared length is one allocation, the read that meets EOF included.
func TestReadBodyAllocatesOnceForADeclaredLength(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts under the race detector")
	}
	page := bytes.Repeat([]byte("x"), 1<<20)
	allocs := testing.AllocsPerRun(20, func() {
		_, _ = readBody(&chunks{data: page, step: 32 << 10}, 2<<20, int64(len(page)))
	})
	if allocs > 2 { // the buffer, and the reader handed to the interface
		t.Errorf("%v allocations", allocs)
	}
}

// A pattern is matched against either form without copying it.
func TestMatchesAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts under the race detector")
	}
	re := regexp.MustCompile(`ok$`)
	page := bytes.Repeat([]byte("x"), 64<<10)
	page = append(page, "ok"...)
	text := string(page)
	for name, f := range map[string]func() bool{
		"bytes":  func() bool { return matches(re, page) },
		"string": func() bool { return matches(re, text) },
	} {
		if !f() {
			t.Fatalf("%s: no match", name)
		}
		if n := testing.AllocsPerRun(50, func() { f() }); n > 0 {
			t.Errorf("%s: %v allocations", name, n)
		}
	}
}
