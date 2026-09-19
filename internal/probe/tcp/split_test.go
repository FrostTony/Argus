package tcp

import (
	"net"
	"testing"
	"time"
)

// A reply arriving in two segments is one reply, not two.
func TestReplySplitAcrossSegments(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.(*net.TCPConn).SetNoDelay(true)
				_, _ = conn.Write([]byte("220 mail.test ESMTP\r\n"))
				buf := make([]byte, 512)
				if _, err := conn.Read(buf); err != nil { // EHLO
					return
				}
				_, _ = conn.Write([]byte("250-mail.test\r\n250-SIZE 1000\r\n"))
				time.Sleep(100 * time.Millisecond) // the second segment of one answer
				_, _ = conn.Write([]byte("250 STARTTLS\r\n"))
				if _, err := conn.Read(buf); err != nil { // QUIT
					return
				}
				_, _ = conn.Write([]byte("221 bye\r\n"))
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	p := newProber(t, `
steps:
  - expect: "^220 "
  - send: "EHLO argus\r\n"
    expect: "250 STARTTLS"
  - send: "QUIT\r\n"
    expect: "^221 "
`)
	res, _ := run(t, p, "127.0.0.1", port)
	if res.Err != nil {
		t.Fatalf("healthy server, answer split in two segments: %v", res.Err)
	}
}
