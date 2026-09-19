package icmp

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
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
	pc := config.Probe{Name: "test", Type: "icmp"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// pingable reports whether this machine permits opening an ICMP socket.
func pingable(t *testing.T, privileged bool) bool {
	t.Helper()
	network := "udp4"
	if privileged {
		network = "ip4:icmp"
	}
	c, err := icmp.ListenPacket(network, "")
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func request(addr string) probe.Request {
	a := netip.MustParseAddr(addr)
	return probe.Request{
		Target:  probe.Target{Name: addr, Host: addr},
		Backend: probe.Backend{Addr: a},
	}
}

func lossOf(rec *metrics.Recorder) float64 {
	for _, s := range rec.Samples() {
		if s.Name == "icmp_packet_loss_percent" {
			return float64(s.Value.(metrics.Gauge))
		}
	}
	return -1
}

func TestReplyMatchingNeedsTheToken(t *testing.T) {
	token := []byte("12345678")
	other := []byte("87654321")

	reply := func(body icmp.MessageBody) *icmp.Message {
		return &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Body: body}
	}
	if !ours(reply(&icmp.Echo{Seq: 2, Data: append(token, 0, 0)}), 2, token) {
		t.Error("our own reply was rejected")
	}
	if ours(reply(&icmp.Echo{Seq: 2, Data: append(other, 0, 0)}), 2, token) {
		t.Error("another probe's reply was accepted")
	}
	if ours(reply(&icmp.Echo{Seq: 1, Data: token}), 2, token) {
		t.Error("an earlier packet of ours was accepted")
	}
	if ours(reply(&icmp.DstUnreach{}), 2, token) {
		t.Error("a non-echo body was accepted")
	}
	// A raw socket pinging a local address sees its own request, token and all.
	request := &icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{Seq: 2, Data: token}}
	if ours(request, 2, token) {
		t.Error("our own echo request was accepted as the reply")
	}
}

func TestNameIsResolvedWhenThereIsNoBackend(t *testing.T) {
	addr, err := address(context.Background(), probe.Request{Target: probe.Target{Host: "localhost"}})
	if err != nil {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	if !addr.IsLoopback() {
		t.Fatalf("localhost resolved to %s", addr)
	}
	if dst := destination(addr, false).(*net.UDPAddr); dst.IP == nil {
		t.Fatal("the destination has no address")
	}
}

func TestLoopbackReplies(t *testing.T) {
	privileged := !pingable(t, false)
	if privileged && !pingable(t, true) {
		t.Skip("no permission to open an ICMP socket")
	}
	opts := "packets: 3\ninterval: 20ms\n"
	if privileged {
		opts += "privileged: true\n"
	}
	rec := metrics.NewRecorder(nil)
	res := newProber(t, opts).Probe(context.Background(), request("127.0.0.1"), rec)
	if !res.OK() {
		t.Fatalf("loopback did not answer: %v", res.Err)
	}
	if loss := lossOf(rec); loss != 0 {
		t.Fatalf("loopback loss %v%%, want 0", loss)
	}
}

func TestPayloadSizeFitsToken(t *testing.T) {
	p := newProber(t, "payload_size: 2").(*Prober)
	if p.cfg.PayloadSize < tokenLen {
		t.Fatalf("payload %d is too small for the token", p.cfg.PayloadSize)
	}
}

func TestJitterNeedsTwoSamples(t *testing.T) {
	if got := jitter([]time.Duration{5 * time.Millisecond}, 5*time.Millisecond); got != 0 {
		t.Fatalf("jitter of a single sample: %v", got)
	}
	rtts := []time.Duration{time.Millisecond, 3 * time.Millisecond}
	if got := jitter(rtts, 2*time.Millisecond); got != 0.001 {
		t.Fatalf("jitter = %v, want 0.001", got)
	}
}

func TestPacketWindowSharesTheRunBudget(t *testing.T) {
	now := time.Now()
	deadline, cancel := context.WithDeadline(context.Background(), now.Add(9*time.Second))
	defer cancel()

	got, ok := packetWindow(deadline, now, 3, 0)
	if !ok || got.Sub(now) != 3*time.Second {
		t.Errorf("first of three packets got %s (ok=%v), want 3s", got.Sub(now), ok)
	}
	if got, _ := packetWindow(deadline, now, 1, 0); got.Sub(now) != 9*time.Second {
		t.Errorf("last packet got %s, want 9s", got.Sub(now))
	}
	if got, _ := packetWindow(context.Background(), now, 3, 0); got.Sub(now) != defaultPacketWait {
		t.Errorf("without a deadline: %s, want %s", got.Sub(now), defaultPacketWait)
	}
}

// Every packet fits the run budget once the inter-packet gaps are reserved.
func TestPacketWindowReservesTheGapsBetweenPackets(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(time.Second))
	defer cancel()

	const gap = 200 * time.Millisecond
	at, sent := now, 0
	for remaining := 5; remaining > 0; remaining-- {
		deadline, ok := packetWindow(ctx, at, remaining, gap)
		if !ok {
			break
		}
		if deadline.Before(at) {
			t.Fatalf("packet %d was given a deadline %s in the past", 5-remaining, at.Sub(deadline))
		}
		sent++
		at = deadline
		if remaining > 1 {
			at = at.Add(gap) // no gap after the last packet
		}
	}
	if sent != 5 {
		t.Fatalf("only %d of 5 packets fitted in the budget", sent)
	}
	if at.After(now.Add(time.Second)) {
		t.Errorf("the run plans to overrun its timeout by %s", at.Sub(now.Add(time.Second)))
	}
}

func TestPacketWindowRefusesAnExpiredRun(t *testing.T) {
	now := time.Now()
	past, cancel := context.WithDeadline(context.Background(), now.Add(-time.Second))
	defer cancel()
	if _, ok := packetWindow(past, now, 3, 0); ok {
		t.Error("a packet was sent with no budget left")
	}
}

func TestReplyHopLimitIsReported(t *testing.T) {
	privileged := !pingable(t, false)
	if privileged && !pingable(t, true) {
		t.Skip("no permission to open an ICMP socket")
	}
	opts := "packets: 2\ninterval: 20ms\n"
	if privileged {
		opts += "privileged: true\n"
	}
	rec := metrics.NewRecorder(nil)
	if res := newProber(t, opts).Probe(context.Background(), request("127.0.0.1"), rec); !res.OK() {
		t.Fatalf("loopback did not answer: %v", res.Err)
	}

	for _, s := range rec.Samples() {
		if s.Name != "icmp_reply_hop_limit" {
			continue
		}
		if got := float64(s.Value.(metrics.Gauge)); got <= 0 || got > 255 {
			t.Fatalf("icmp_reply_hop_limit = %v, want a hop count", got)
		}
		return
	}
	// A datagram socket need not deliver the control message.
	t.Skip("this socket did not report the reply's hop limit")
}
