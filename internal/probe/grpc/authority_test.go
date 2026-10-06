package grpc

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
)

func TestIPv6AuthorityIsBracketed(t *testing.T) {
	var authority string
	req := h2server(t, func(w http.ResponseWriter, r *http.Request) {
		authority = r.Host
		w.Header().Set("grpc-status", "0")
		_, _ = w.Write([]byte{0, 0, 0, 0, 2, 8, 1}) // SERVING
	}, nil)
	req.Target.Host = "2001:db8::1"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := newProber(t, `{tls: {insecure_skip_verify: true}}`).Probe(ctx, req, metrics.NewRecorder(metrics.Labels{}))
	if !res.OK() {
		t.Fatal(res.Err)
	}
	if authority != "[2001:db8::1]" {
		t.Fatalf(":authority = %q, want [2001:db8::1]", authority)
	}
}

func TestHostOfAuthority(t *testing.T) {
	for authority, want := range map[string]string{
		"example.com":        "example.com",
		"example.com:443":    "example.com",
		"[2001:db8::1]":      "2001:db8::1",
		"[2001:db8::1]:8443": "2001:db8::1",
	} {
		if got := hostOf(authority); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", authority, got, want)
		}
	}
}
