package probe

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/resolve"
)

// The semaphore must bound live goroutines, not just in-flight requests.
func TestRunOnceBoundsGoroutines(t *testing.T) {
	const targets, backends = 500, 8

	addrs := make(resolve.Static, 0, backends)
	for i := 0; i < backends; i++ {
		addrs = append(addrs, netip.AddrFrom4([4]byte{10, 0, 0, byte(i + 1)}))
	}
	list := make(StaticTargets, 0, targets)
	for i := 0; i < targets; i++ {
		list = append(list, Target{Name: string(rune('a'+i%26)) + itoa(i), Host: "h.example", Port: 80})
	}

	var peak atomic.Int64
	slow := proberFunc(func(ctx context.Context, _ Request, _ *metrics.Recorder) Result {
		if n := int64(runtime.NumGoroutine()); n > peak.Load() {
			peak.Store(n)
		}
		select {
		case <-time.After(2 * time.Millisecond):
		case <-ctx.Done():
		}
		return Result{}
	})

	r := &Runner{
		Name: "many", Kind: "fake", Prober: slow, Source: list,
		Interval: time.Hour, Timeout: time.Second,
		Resolver: addrs, Family: resolve.Both, MaxBackends: backends, PerBackend: true,
		Sem: make(chan struct{}, 16),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	before := runtime.NumGoroutine()
	r.RunOnce(context.Background(), &collector{})

	if got := peak.Load() - int64(before); got > 1000 {
		t.Fatalf("%d goroutines alive at peak for %d probes with a limit of 16", got, targets*backends)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
