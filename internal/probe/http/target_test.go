package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func TestTargetURLFromHostAndPort(t *testing.T) {
	p := newProber(t, `{}`).(*Prober)
	for _, tc := range []struct {
		host string
		port int
		want string
	}{
		{"example.com", 0, "https://example.com/"},
		{"example.com", 80, "http://example.com/"},
		{"example.com", 8443, "https://example.com:8443/"},
		{"2001:db8::1", 0, "https://[2001:db8::1]/"},
		{"2001:db8::1", 80, "http://[2001:db8::1]/"},
		{"2001:db8::1", 443, "https://[2001:db8::1]/"},
		{"2001:db8::1", 8443, "https://[2001:db8::1]:8443/"},
	} {
		u, err := p.targetURL(probe.Target{Host: tc.host, Port: tc.port})
		if err != nil {
			t.Fatalf("%s:%d: %v", tc.host, tc.port, err)
		}
		if got := u.String(); got != tc.want {
			t.Errorf("%s:%d: url = %q, want %q", tc.host, tc.port, got, tc.want)
		}
		if got := u.Hostname(); got != tc.host {
			t.Errorf("%s:%d: hostname = %q", tc.host, tc.port, got)
		}
	}
}

func TestPathKeepsItsQuery(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.URL.RequestURI()
	}))
	defer srv.Close()

	p := newProber(t, `path: "/health?full=1"`)
	if res := p.Probe(context.Background(), request(t, srv), metrics.NewRecorder(metrics.Labels{})); !res.OK() {
		t.Fatal(res.Err)
	}
	if got != "/health?full=1" {
		t.Fatalf("requested %q, want /health?full=1", got)
	}
}

func TestPathMustBeAPath(t *testing.T) {
	if _, err := options(`path: "https://elsewhere/x"`); err == nil {
		t.Fatal("a URL was accepted as the path")
	}
}

// net/http carries the Host override only across relative redirects; an
// absolute one back to the same name must keep presenting it.
func TestHostnameSurvivesAnAbsoluteSameOriginRedirect(t *testing.T) {
	var hosts []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		if r.URL.Path == "/" {
			http.Redirect(w, r, srv.URL+"/second", http.StatusFound)
		}
	}))
	defer srv.Close()

	req := request(t, srv)
	req.Hostname = "vhost.test"
	res := newProber(t, "follow_redirects: true\n").Probe(context.Background(), req, metrics.NewRecorder(metrics.Labels{}))
	if !res.OK() {
		t.Fatal(res.Err)
	}
	if len(hosts) != 2 || hosts[0] != "vhost.test" || hosts[1] != "vhost.test" {
		t.Fatalf("hosts = %q, want vhost.test on both hops", hosts)
	}
}
