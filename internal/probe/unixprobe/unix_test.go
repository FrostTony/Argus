package unixprobe

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func build(t *testing.T, options string) (probe.Prober, error) {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "unix"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	return New(pc)
}

func run(t *testing.T, p probe.Prober, path string) (probe.Result, *metrics.Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rec := metrics.NewRecorder(nil)
	return p.Probe(ctx, probe.Request{Target: probe.Target{Name: path, Host: path}}, rec), rec
}

// serveStream answers each connection with reply, after reading what was sent.
func serveStream(t *testing.T, reply string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write([]byte(reply))
			}()
		}
	}()
	return path
}

func TestStreamConversation(t *testing.T) {
	path := serveStream(t, "220 mail.test ESMTP Postfix\r\n")
	p, err := build(t, "expect: '^220 (?P<banner>.+) ESMTP'\n")
	if err != nil {
		t.Fatal(err)
	}
	res, rec := run(t, p, path)
	if res.Err != nil {
		t.Fatalf("probe failed: %v", res.Err)
	}
	var banner string
	for _, s := range rec.Samples() {
		if s.Name == probe.SeriesExpect {
			banner = s.Labels.Get("banner")
		}
	}
	if banner != "mail.test" {
		t.Errorf("banner = %q, want mail.test", banner)
	}
}

func TestStreamReportsAMissingSocket(t *testing.T) {
	p, err := build(t, "")
	if err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, p, filepath.Join(t.TempDir(), "absent"))
	if res.Err == nil {
		t.Fatal("a socket that does not exist was reported as up")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonConnect {
		t.Errorf("reason = %q, want connect", got)
	}
}

func TestExpectMismatchIsARegexFailure(t *testing.T) {
	path := serveStream(t, "500 go away\r\n")
	p, err := build(t, "expect: '^220 '\n")
	if err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, p, path)
	if res.Err == nil {
		t.Fatal("a reply that does not match was accepted")
	}
	if !probe.RegexFailure(res.Err) {
		t.Errorf("failure is not classified as a pattern failure: %v", res.Err)
	}
}

func TestDatagramExchange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d")
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	srv, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Skipf("unixgram unavailable: %v", err)
	}
	defer srv.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := srv.ReadFromUnix(buf)
			if err != nil {
				return
			}
			_, _ = srv.WriteToUnix([]byte("pong:"+string(buf[:n])), from)
		}
	}()

	p, err := build(t, "network: unixgram\nsend: ping\nexpect: '^pong:ping$'\n")
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := run(t, p, path); res.Err != nil {
		t.Fatalf("datagram probe failed: %v", res.Err)
	}
}

// A datagram is one message: the step machinery has no stream to walk.
func TestDatagramRefusesStreamOnlyOptions(t *testing.T) {
	for _, options := range []string{
		"network: unixgram\nsteps: [{send: a, expect: b}]\n",
		"network: unixgram\ntls: true\n",
	} {
		if _, err := build(t, options); err == nil {
			t.Errorf("accepted stream-only configuration:\n%s", options)
		}
	}
}

func TestNetworkIsChecked(t *testing.T) {
	_, err := build(t, "network: udp\n")
	if err == nil || !strings.Contains(err.Error(), "unix|unixgram|unixpacket") {
		t.Errorf("error = %v, want the list of networks", err)
	}
}

// A path is not a name: there is nothing to resolve and no backend to fan out.
func TestUnixAddressesItself(t *testing.T) {
	p, err := build(t, "")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := p.(probe.Addressing)
	if !ok || !a.SelfAddressed() {
		t.Error("the unix prober does not address itself, so the runner would resolve a file path")
	}
}

// serveDatagram answers each datagram with reply(what was sent).
func serveDatagram(t *testing.T, reply func([]byte) []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d")
	srv, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Skipf("unixgram unavailable: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := srv.ReadFromUnix(buf)
			if err != nil {
				return
			}
			_, _ = srv.WriteToUnix(reply(buf[:n]), from)
		}
	}()
	return path
}

// read_bytes bounds what is matched. A zero from the configuration used to mean
// "read nothing", which failed every check.
func TestDatagramReadBytesIsNormalized(t *testing.T) {
	path := serveDatagram(t, func([]byte) []byte { return []byte("pong") })
	for _, options := range []string{
		"network: unixgram\nsend: ping\nexpect: '^pong$'\nread_bytes: 0\n",
		"network: unixgram\nsend: ping\nexpect: '^pong$'\nread_bytes: -1\n",
	} {
		p, err := build(t, options)
		if err != nil {
			t.Fatalf("%s: %v", options, err)
		}
		if res, _ := run(t, p, path); res.Err != nil {
			t.Errorf("%s: %v", options, res.Err)
		}
	}
}

// A datagram is delivered whole or not at all: a buffer that was too small
// loses the rest of it, and the size metric would report the buffer.
func TestDatagramSizeIsTheDatagramsNotTheBuffers(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 9000)
	path := serveDatagram(t, func([]byte) []byte { return big })

	p, err := build(t, "network: unixgram\nsend: ping\nexpect: 'x'\nread_bytes: 16\n")
	if err != nil {
		t.Fatal(err)
	}
	res, rec := run(t, p, path)
	if res.Err != nil {
		t.Fatalf("probe failed: %v", res.Err)
	}
	for _, s := range rec.Samples() {
		if s.Name != sizeSeries {
			continue
		}
		if got := float64(s.Value.(metrics.Gauge)); got != float64(len(big)) {
			t.Errorf("%s = %v, want %d", sizeSeries, got, len(big))
		}
		return
	}
	t.Errorf("%s was not recorded", sizeSeries)
}

// A malformed tls_config is a configuration error whichever network is used.
func TestBadTLSConfigIsRejectedForEveryNetwork(t *testing.T) {
	for _, network := range []string{"unix", "unixgram"} {
		options := "network: " + network + "\ntls_config:\n  ca_cert: /no/such/ca.pem\n"
		if _, err := build(t, options); err == nil {
			t.Errorf("%s: a missing CA file was accepted", network)
		}
	}
}
