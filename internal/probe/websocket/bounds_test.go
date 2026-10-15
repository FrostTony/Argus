package websocket

import (
	"bufio"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"testing"

	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

// The server writes the handshake's headers; the node reads only so much.
func TestHandshakeAnswerIsBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		line := "X-Pad: " + strings.Repeat("v", 4000) + "\r\n"
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n"))
		for range 16 << 10 { // 64 MiB, if anyone reads it all
			if _, err := conn.Write([]byte(line)); err != nil {
				return
			}
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	req := probe.Request{
		Target:  probe.Target{Name: "ws", Host: "127.0.0.1", Port: port},
		Backend: probe.Backend{Addr: netip.MustParseAddr("127.0.0.1"), Port: port},
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res, _ := run(t, build(t, ``), req)
	runtime.ReadMemStats(&after)

	if res.Err == nil {
		t.Fatal("an endless handshake answer was accepted")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Errorf("reading the handshake cost %d MiB", grew>>20)
	}
}
