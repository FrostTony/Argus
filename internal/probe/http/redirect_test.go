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
