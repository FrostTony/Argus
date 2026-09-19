package udp

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func newProber(t *testing.T, options string) probe.Prober {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "udp"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func run(t *testing.T, p probe.Prober, port int) (probe.Result, []metrics.Sample) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec := metrics.NewRecorder(metrics.Labels{})
	req := probe.Request{
		Target:  probe.Target{Name: "local", Host: "127.0.0.1"},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}
	return p.Probe(ctx, req, rec), rec.Samples()
}

// echo answers every datagram with reply, or with nothing when reply is empty.
func echo(t *testing.T, reply string) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply != "" {
				_, _ = pc.WriteTo([]byte(reply), addr)
			}
			_ = n
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// closedPort is a port nothing listens on, which answers with ICMP unreachable.
func closedPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	return port
}

func TestExpectedReplyPasses(t *testing.T) {
	port := echo(t, "PONG")
	p := newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\nexpect: PONG\n")
	res, samples := run(t, p, port)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if n, ok := gauge(samples, "udp_response_size_bytes"); !ok || n != 4 {
		t.Errorf("udp_response_size_bytes = %v, want 4", n)
	}
}

func TestWrongReplyIsAContentFailure(t *testing.T) {
	port := echo(t, "NOPE")
	p := newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\nexpect: PONG\n")
	res, _ := run(t, p, port)
	if got := probe.ReasonOf(res.Err); got != probe.ReasonContent {
		t.Fatalf("reason = %q, want %q", got, probe.ReasonContent)
	}
}

func TestClosedPortFailsWithoutAnExpectedReply(t *testing.T) {
	port := closedPort(t)
	p := newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\n")
	res, _ := run(t, p, port)
	if res.Err == nil {
		t.Fatal("a closed port was reported as healthy")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonConnect {
		t.Fatalf("reason = %q, want %q", got, probe.ReasonConnect)
	}
}

func TestSilentPortPassesWithoutAnExpectedReply(t *testing.T) {
	port := echo(t, "")
	p := newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\n")
	start := time.Now()
	res, _ := run(t, p, port)
	if res.Err != nil {
		t.Fatalf("want success, got %v", res.Err)
	}
	if elapsed := time.Since(start); elapsed > 2*defaultUnreachableWait {
		t.Errorf("the check took %s, want about %s", elapsed, defaultUnreachableWait)
	}
}

func gauge(samples []metrics.Sample, name string) (float64, bool) {
	for _, s := range samples {
		if s.Name == name {
			if g, ok := s.Value.(metrics.Gauge); ok {
				return float64(g), true
			}
		}
	}
	return 0, false
}

func TestRefusedPortHasOneReasonWhateverIsConfigured(t *testing.T) {
	port := closedPort(t)
	bare, _ := run(t, newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\n"), port)
	expecting, _ := run(t, newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\nexpect: PONG\n"), port)

	if bare.Err == nil || expecting.Err == nil {
		t.Fatalf("a closed port passed: %v / %v", bare.Err, expecting.Err)
	}
	if a, b := probe.ReasonOf(bare.Err), probe.ReasonOf(expecting.Err); a != b {
		t.Fatalf("the same refusal was classified %q without expect and %q with it", a, b)
	}
}

func TestTheSilentWaitIsNotReportedAsLatency(t *testing.T) {
	port := echo(t, "")
	res, samples := run(t, newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\n"), port)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	for _, ph := range res.Phases {
		if ph.Name == "read" {
			t.Errorf("the wait was reported as a read phase of %s", ph.D)
		}
	}
	if _, ok := gauge(samples, "udp_response_size_bytes"); ok {
		t.Error("a response size was recorded although nothing answered")
	}
}

// An instant read timeout on an exhausted budget is not silence, so not health.
func TestAnExhaustedBudgetIsNotSuccess(t *testing.T) {
	port := echo(t, "")
	p := newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\n")

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond))
	defer cancel()
	time.Sleep(2 * time.Millisecond)

	req := probe.Request{
		Target:  probe.Target{Name: "local", Host: "127.0.0.1"},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}
	if res := p.Probe(ctx, req, metrics.NewRecorder(metrics.Labels{})); res.Err == nil {
		t.Fatal("a check that had no time to run reported the target healthy")
	}
}

func TestUnreachableWaitIsConfigurable(t *testing.T) {
	port := echo(t, "")
	p := newProber(t, "port: "+strconv.Itoa(port)+"\nsend: PING\nunreachable_wait: 30ms\n")
	start := time.Now()
	if res, _ := run(t, p, port); res.Err != nil {
		t.Fatal(res.Err)
	}
	if elapsed := time.Since(start); elapsed > defaultUnreachableWait {
		t.Fatalf("the check took %s; the configured 30ms window was ignored", elapsed)
	}
}
