package tlsprobe

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func checkHost(t *testing.T, opts string, host string, port int) probe.Result {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(opts), &node); err != nil {
		t.Fatal(err)
	}
	p, err := New(config.Probe{Name: "t", Type: "tls", Options: *node.Content[0]})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return p.Probe(ctx, probe.Request{
		Target:  probe.Target{Name: host, Host: host, Port: port},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}, metrics.NewRecorder(metrics.Labels{}))
}

// An IP literal target demands an IP SAN, as in the stdlib.
func TestIPLiteralTargetStillChecksName(t *testing.T) {
	port, ca := serve(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	res := checkHost(t, "ca_cert: "+ca, "127.0.0.1", port)
	if res.OK() {
		t.Fatal("a certificate for site.test (no IP SAN) was accepted for target 127.0.0.1")
	}
}

func TestIPLiteralServerNameStillChecksName(t *testing.T) {
	port, ca := serve(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	res := checkHost(t, "{ca_cert: "+ca+", server_name: 10.1.2.3}", "site.test", port)
	if res.OK() {
		t.Fatal("a certificate for site.test was accepted for server_name 10.1.2.3")
	}
}

// With no name to verify against, no certificate is accepted.
func TestEmptyServerNameIsNotTrusted(t *testing.T) {
	port, ca := serve(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	res := checkHost(t, "ca_cert: "+ca, "", port)
	if res.OK() {
		t.Fatal("a certificate was accepted with no server name to check it against")
	}
}
