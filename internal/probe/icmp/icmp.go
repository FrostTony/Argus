// Package icmp pings one backend and reports RTT, loss and jitter.
//
// It uses an unprivileged datagram socket (udp4/udp6) and falls back to a raw
// socket only when that is not permitted.
package icmp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func init() { probe.Register("icmp", New) }

const tokenLen = 8

// Config is the `icmp:` block.
type Config struct {
	Packets     int             `yaml:"packets"`
	Interval    config.Duration `yaml:"interval"`
	PayloadSize int             `yaml:"payload_size"`
	// MaxLossPercent fails the check above this loss; 0 means any reply is enough.
	MaxLossPercent float64 `yaml:"max_loss_percent"`
	// Privileged switches to a raw socket where datagram ping is not permitted.
	Privileged bool `yaml:"privileged"`
	TTL        int  `yaml:"ttl"`
	// TOS is the IPv4 type-of-service byte, or the IPv6 traffic class: the same
	// DSCP marking the traffic under test carries, so the ping shares its queue.
	TOS int `yaml:"tos"`
}

type Prober struct{ cfg Config }

func New(pc config.Probe) (probe.Prober, error) {
	cfg := Config{Packets: 3, Interval: config.Duration(100 * time.Millisecond), PayloadSize: 56}
	if err := pc.DecodeOptions(&cfg); err != nil {
		return nil, err
	}
	if cfg.Packets <= 0 {
		return nil, fmt.Errorf("icmp.packets must be positive")
	}
	if cfg.TOS < 0 || cfg.TOS > 255 {
		return nil, fmt.Errorf("icmp.tos must be 0-255, got %d", cfg.TOS)
	}
	if cfg.PayloadSize < tokenLen {
		// The payload must fit the token that identifies our replies.
		cfg.PayloadSize = tokenLen
	}
	return &Prober{cfg: cfg}, nil
}

func (p *Prober) Probe(ctx context.Context, req probe.Request, rec *metrics.Recorder) probe.Result {
	var res probe.Result

	addr, err := address(ctx, req)
	if err != nil {
		res.Err = probe.Wrap(probe.ReasonDNS, err)
		return res
	}
	conn, proto, err := p.listen(req, addr)
	if err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "icmp socket: %v", err)
		return res
	}
	defer conn.Close()

	dst := destination(addr, p.cfg.Privileged)
	var token [tokenLen]byte
	if _, err := rand.Read(token[:]); err != nil {
		res.Err = probe.Fail(probe.ReasonInternal, "icmp token: %v", err)
		return res
	}
	id := int(binary.BigEndian.Uint16(token[:2]))
	ech := &echoes{token: token, answered: make([]bool, p.cfg.Packets)}
	rtts := make([]time.Duration, 0, p.cfg.Packets)
	read := readerFor(conn)
	hopLimit := 0
	buf := make([]byte, 1500)

	sent := 0
	for seq := 0; seq < p.cfg.Packets; seq++ {
		if seq > 0 {
			// The gap is the probe pacing itself, so it is not target latency.
			res.Overhead += p.cfg.Interval.D()
			select {
			case <-time.After(p.cfg.Interval.D()):
			case <-ctx.Done():
				if hopLimit > 0 {
					rec.Gauge("icmp_reply_hop_limit", float64(hopLimit))
				}
				return p.finish(&res, rec, rtts, sent, ech.dups)
			}
		}
		deadline, ok := packetWindow(ctx, time.Now(), p.cfg.Packets-seq, p.cfg.Interval.D())
		if !ok {
			break
		}
		sent++
		rtt, hops, err := p.exchange(conn, read, buf, proto, dst, id, seq, ech, deadline)
		if err == nil {
			rtts = append(rtts, rtt)
			hopLimit = hops
		}
	}
	if hopLimit > 0 {
		rec.Gauge("icmp_reply_hop_limit", float64(hopLimit))
	}
	return p.finish(&res, rec, rtts, sent, ech.dups)
}

// echoes is one run's bookkeeping: the token that marks our packets, which
// sequence numbers have been answered, and how many replies arrived for a
// sequence already answered.
type echoes struct {
	token    [tokenLen]byte
	answered []bool
	dups     int
}

// address is the backend the runner picked, or the resolved target host.
func address(ctx context.Context, req probe.Request) (netip.Addr, error) {
	if req.Backend.Valid() {
		return req.Backend.Addr, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", req.Target.Host)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("%s resolved to no address", req.Target.Host)
	}
	return addrs[0].Unmap(), nil
}

func (p *Prober) listen(req probe.Request, addr netip.Addr) (*icmp.PacketConn, int, error) {
	v6 := addr.Is6() && !addr.Is4In6()
	network, proto := "udp4", ipv4.ICMPTypeEcho.Protocol()
	if v6 {
		network, proto = "udp6", ipv6.ICMPTypeEchoRequest.Protocol()
	}
	if p.cfg.Privileged {
		network = map[string]string{"udp4": "ip4:icmp", "udp6": "ip6:ipv6-icmp"}[network]
	}
	conn, err := icmp.ListenPacket(network, req.LocalUDPAddr())
	if err != nil {
		if !p.cfg.Privileged && errors.Is(err, os.ErrPermission) {
			return nil, proto, ErrUnprivileged
		}
		return nil, proto, err
	}
	if err := p.applySocketOptions(conn, v6); err != nil {
		conn.Close()
		return nil, proto, err
	}
	return conn, proto, nil
}

func (p *Prober) applySocketOptions(conn *icmp.PacketConn, v6 bool) error {
	if v6 {
		c := conn.IPv6PacketConn()
		if c == nil {
			return nil
		}
		if p.cfg.TTL > 0 {
			if err := c.SetHopLimit(p.cfg.TTL); err != nil {
				return err
			}
		}
		if p.cfg.TOS > 0 {
			if err := c.SetTrafficClass(p.cfg.TOS); err != nil {
				return err
			}
		}
		// Best effort: a datagram socket need not deliver the control message.
		_ = c.SetControlMessage(ipv6.FlagHopLimit, true)
		return nil
	}

	c := conn.IPv4PacketConn()
	if c == nil {
		return nil
	}
	if p.cfg.TTL > 0 {
		if err := c.SetTTL(p.cfg.TTL); err != nil {
			return err
		}
	}
	if p.cfg.TOS > 0 {
		if err := c.SetTOS(p.cfg.TOS); err != nil {
			return err
		}
	}
	_ = c.SetControlMessage(ipv4.FlagTTL, true)
	return nil
}

// replyReader reads one packet and reports its hop limit, or zero if unknown.
type replyReader func(buf []byte) (n, hops int, err error)

func readerFor(conn *icmp.PacketConn) replyReader {
	if c := conn.IPv4PacketConn(); c != nil {
		return func(buf []byte) (int, int, error) {
			n, cm, _, err := c.ReadFrom(buf)
			if cm == nil {
				return n, 0, err
			}
			return n, cm.TTL, err
		}
	}
	if c := conn.IPv6PacketConn(); c != nil {
		return func(buf []byte) (int, int, error) {
			n, cm, _, err := c.ReadFrom(buf)
			if cm == nil {
				return n, 0, err
			}
			return n, cm.HopLimit, err
		}
	}
	return func(buf []byte) (int, int, error) {
		n, _, err := conn.ReadFrom(buf)
		return n, 0, err
	}
}

// destination wraps the address: a datagram ICMP socket addresses with UDPAddr.
func destination(addr netip.Addr, privileged bool) net.Addr {
	ip := net.IP(addr.AsSlice())
	if privileged {
		return &net.IPAddr{IP: ip}
	}
	return &net.UDPAddr{IP: ip}
}

func (p *Prober) exchange(conn *icmp.PacketConn, read replyReader, buf []byte, proto int, dst net.Addr, id, seq int, ech *echoes, deadline time.Time) (time.Duration, int, error) {
	body := &icmp.Echo{ID: id, Seq: seq, Data: p.payload(ech.token)}
	var msg icmp.Message
	if proto == ipv4.ICMPTypeEcho.Protocol() {
		msg = icmp.Message{Type: ipv4.ICMPTypeEcho, Body: body}
	} else {
		msg = icmp.Message{Type: ipv6.ICMPTypeEchoRequest, Body: body}
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return 0, 0, err
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return 0, 0, err
	}

	start := time.Now()
	if _, err := conn.WriteTo(wire, dst); err != nil {
		return 0, 0, err
	}

	for {
		n, hops, err := read(buf)
		if err != nil {
			return 0, 0, err
		}
		reply, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		got, ok := echoOf(reply, ech.token[:])
		if !ok {
			continue
		}
		if got != seq {
			// A reply to a packet we have moved on from. The first one is
			// simply late; a second is the path duplicating it, which a
			// broadcast address or an anycast set does.
			if got >= 0 && got < len(ech.answered) {
				if ech.answered[got] {
					ech.dups++
				}
				ech.answered[got] = true
			}
			continue
		}
		ech.answered[seq] = true
		return time.Since(start), hops, nil
	}
}

// defaultPacketWait applies when the run carries no deadline of its own.
const defaultPacketWait = time.Second

// packetWindow gives one packet its share of what is left of the run, reserving
// the gaps still to be slept, and reports whether there is budget to send it.
func packetWindow(ctx context.Context, now time.Time, remaining int, gap time.Duration) (time.Time, bool) {
	end, ok := ctx.Deadline()
	if !ok {
		return now.Add(defaultPacketWait), true
	}
	left := end.Sub(now)
	if left <= 0 {
		return end, false
	}
	if remaining <= 1 {
		return end, true
	}
	if gap > 0 {
		left -= time.Duration(remaining-1) * gap
	}
	window := left / time.Duration(remaining)
	if window <= 0 {
		// The gaps alone exhaust the run: spend the rest on this one packet.
		return end, true
	}
	return now.Add(window), true
}

// echoOf returns the sequence number of a reply to one of our echoes. The
// payload token is what identifies it: a raw socket sees every ICMP reply on
// the host, and the kernel rewrites a datagram socket's echo id.
func echoOf(m *icmp.Message, token []byte) (int, bool) {
	if m.Type != ipv4.ICMPTypeEchoReply && m.Type != ipv6.ICMPTypeEchoReply {
		return 0, false
	}
	echo, ok := m.Body.(*icmp.Echo)
	if !ok || !bytes.HasPrefix(echo.Data, token) {
		return 0, false
	}
	return echo.Seq, true
}

func (p *Prober) payload(token [tokenLen]byte) []byte {
	b := make([]byte, p.cfg.PayloadSize)
	copy(b, token[:])
	return b
}

// finish reports loss and RTT over the packets actually sent.
// A duplicate of the last packet arrives after the run has stopped reading, so
// dups undercounts by design rather than holding the socket open for it.
func (p *Prober) finish(res *probe.Result, rec *metrics.Recorder, rtts []time.Duration, sent, dups int) probe.Result {
	got := len(rtts)
	if sent == 0 {
		res.Err = probe.Fail(probe.ReasonTimeout, "no time left to send a packet")
		return *res
	}
	loss := float64(sent-got) / float64(sent) * 100

	rec.Gauge("icmp_packets_sent", float64(sent))
	rec.Gauge("icmp_packets_received", float64(got))
	rec.Gauge("icmp_packet_loss_percent", loss)
	rec.Gauge("icmp_duplicates", float64(dups))

	if got == 0 {
		res.Err = probe.Fail(probe.ReasonConnect, "no reply to %d packet(s)", sent)
		return *res
	}

	minRTT, maxRTT, sum := rtts[0], rtts[0], time.Duration(0)
	for _, r := range rtts {
		sum += r
		minRTT = min(minRTT, r)
		maxRTT = max(maxRTT, r)
	}
	avg := sum / time.Duration(got)

	rec.Gauge("icmp_rtt_min_seconds", minRTT.Seconds())
	rec.Gauge("icmp_rtt_max_seconds", maxRTT.Seconds())
	rec.Gauge("icmp_rtt_avg_seconds", avg.Seconds())
	rec.Gauge("icmp_rtt_jitter_seconds", jitter(rtts, avg))
	res.Add("rtt", avg)

	if p.cfg.MaxLossPercent > 0 && loss > p.cfg.MaxLossPercent {
		res.Err = probe.Fail(probe.ReasonConnect, "packet loss %.0f%% above %.0f%%", loss, p.cfg.MaxLossPercent)
	}
	return *res
}

// jitter is the mean absolute deviation from the average RTT.
func jitter(rtts []time.Duration, avg time.Duration) float64 {
	if len(rtts) < 2 {
		return 0
	}
	var sum float64
	for _, r := range rtts {
		sum += math.Abs((r - avg).Seconds())
	}
	return sum / float64(len(rtts))
}

// ErrUnprivileged reports that an unprivileged datagram ping is not permitted.
var ErrUnprivileged = errors.New(
	"icmp: datagram ping not permitted; allow it with net.ipv4.ping_group_range or set icmp.privileged: true")
