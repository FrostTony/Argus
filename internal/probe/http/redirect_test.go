package http

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// asInternet makes 127.0.0.1 stand for the internet and the rest of loopback
// for the node's own network.
func asInternet(t *testing.T) {
	t.Helper()
	saved := public
	public = func(a netip.Addr) bool { return a == netip.MustParseAddr("127.0.0.1") }
	t.Cleanup(func() { public = saved })
}

func TestPublicTargetCannotRedirectInward(t *testing.T) {
	asInternet(t)
	var reached atomic.Int64
	inside := serveOn(t, "127.0.0.2:0", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inside.URL+"/", http.StatusFound)
	}))
	defer site.Close()

	res, _ := captureWith(t, newProber(t, ``), request(t, site), nil)
	if probe.ReasonOf(res.Err) != probe.ReasonConnect || !strings.Contains(res.Err.Error(), "redirect_to_private") || reached.Load() != 0 {
		t.Errorf("err %v, inside reached %d times", res.Err, reached.Load())
	}
	if res, _ := captureWith(t, newProber(t, `redirect_to_private: true`), request(t, site), nil); res.Err != nil || reached.Load() != 1 {
		t.Errorf("opted in: err %v, inside reached %d times", res.Err, reached.Load())
	}
}

// An internal service redirecting within its own network is what it always did.
func TestPrivateTargetRedirectsWhereItLikes(t *testing.T) {
	asInternet(t)
	inside := serveOn(t, "127.0.0.2:0", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	portal := serveOn(t, "127.0.0.3:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inside.URL+"/", http.StatusFound)
	}))
	if res, _ := captureWith(t, newProber(t, ``), request(t, portal), nil); res.Err != nil {
		t.Errorf("an internal redirect was refused: %v", res.Err)
	}
}

// A site that sets a cookie and redirects to itself (consent, geo, a bot check)
// is a loop for a client without cookies and one more hop for a browser.
func TestCookiesCarryAcrossRedirects(t *testing.T) {
	var hops atomic.Int64
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		if c, err := r.Cookie("seen"); err != nil || c.Value != "1" {
			http.SetCookie(w, &http.Cookie{Name: "seen", Value: "1", Path: "/"})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer site.Close()

	if res, _ := captureWith(t, newProber(t, ``), request(t, site), nil); res.Err != nil || hops.Load() != 2 {
		t.Errorf("with cookies: err %v after %d hops", res.Err, hops.Load())
	}
	hops.Store(0)
	res, _ := captureWith(t, newProber(t, `cookies: false`), request(t, site), nil)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "redirect limit") {
		t.Errorf("without cookies the loop must hit the limit, got %v after %d hops", res.Err, hops.Load())
	}
	// a cookie one run was given is not there for the next
	hops.Store(0)
	p := newProber(t, ``)
	captureWith(t, p, request(t, site), nil)
	captureWith(t, p, request(t, site), nil)
	if hops.Load() != 4 {
		t.Errorf("two runs took %d hops, want 2 each: the jar outlived its run", hops.Load())
	}
}

// A chain that never ends is shown up to where it was stopped.
func TestCaptureKeepsTheChainOfARedirectLoop(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	}))
	defer site.Close()
	res, _ := captureWith(t, newProber(t, `max_redirects: 3`), request(t, site), &probe.Capture{})
	if res.Err == nil || len(res.Artifacts) != 1 || res.Artifacts[0].Name != "headers" {
		t.Fatalf("err %v, artifacts %+v", res.Err, res.Artifacts)
	}
	if hops := strings.Count(string(res.Artifacts[0].Data), "< HTTP/1.1 302"); hops != 3 {
		t.Errorf("%d hops in the exchange, want 3:\n%s", hops, res.Artifacts[0].Data)
	}
}
