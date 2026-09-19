package http

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// The check reads the timings while the dial goroutine still writes them.
func TestTimingsSurviveADialThatOutlivesTheDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, say nothing
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	u, _ := url.Parse("https://blackhole.test/")
	req := probe.Request{
		Target:  probe.Target{Name: "t", Host: "blackhole.test", URL: u},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
		Buckets: metrics.Buckets{0.1, 1},
	}
	u.Host = net.JoinHostPort("blackhole.test", itoa(port))

	p := newProber(t, ``)
	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		res := p.Probe(ctx, req, metrics.NewRecorder(metrics.L("probe", "t")))
		cancel()
		if res.OK() {
			t.Fatal("expected a failure")
		}
		if got := probe.ReasonOf(res.Err); got != probe.ReasonTimeout {
			t.Fatalf("reason = %s, want timeout (%v)", got, res.Err)
		}
	}
	time.Sleep(50 * time.Millisecond) // let the dial goroutines finish under the detector
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}
