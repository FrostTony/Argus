package websocket

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // the handshake is defined in terms of sha1
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tonyamdfrost-cmd/Argus/internal/config"
	"github.com/tonyamdfrost-cmd/Argus/internal/metrics"
	"github.com/tonyamdfrost-cmd/Argus/internal/probe"
)

func build(t *testing.T, options string) probe.Prober {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(options), &node); err != nil {
		t.Fatal(err)
	}
	pc := config.Probe{Name: "test", Type: "websocket"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	p, err := New(pc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func request(t *testing.T, srv *httptest.Server) probe.Request {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	addr, _ := netip.ParseAddr(u.Hostname())
	return probe.Request{
		Target:  probe.Target{Name: u.Host, Host: u.Hostname(), Port: port, URL: u},
		Backend: probe.Backend{Addr: addr, Port: port},
	}
}

func run(t *testing.T, p probe.Prober, req probe.Request) (probe.Result, *metrics.Recorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec := metrics.NewRecorder(nil)
	return p.Probe(ctx, req, rec), rec
}

// upgrader answers the handshake and then hands the raw connection to talk.
func upgrader(t *testing.T, subprotocol string, talk func(conn net.Conn, r *bufio.Reader)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		if key == "" {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot hijack")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		sum := sha1.Sum([]byte(key + acceptGUID)) //nolint:gosec // as above
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n"
		if subprotocol != "" {
			resp += "Sec-WebSocket-Protocol: " + subprotocol + "\r\n"
		}
		if _, err := rw.WriteString(resp + "\r\n"); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}
		if talk != nil {
			talk(conn, rw.Reader)
		}
	}))
}

func TestHandshakeAlone(t *testing.T) {
	srv := upgrader(t, "", nil)
	defer srv.Close()

	res, rec := run(t, build(t, ""), request(t, srv))
	if res.Err != nil {
		t.Fatalf("handshake failed: %v", res.Err)
	}
	for _, s := range rec.Samples() {
		if s.Name == "websocket_handshake_status_code" {
			if got := float64(s.Value.(metrics.Gauge)); got != 101 {
				t.Errorf("status = %v, want 101", got)
			}
			return
		}
	}
	t.Error("websocket_handshake_status_code was not recorded")
}

// The point of the prober: a proxy that forwards HTTP and drops Upgrade looks
// perfectly healthy to every other check.
func TestPlainAnswerIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res, _ := run(t, build(t, ""), request(t, srv))
	if res.Err == nil {
		t.Fatal("a 200 to an upgrade request was accepted")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonStatus {
		t.Errorf("reason = %q, want status", got)
	}
}

func TestWrongAcceptTokenIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Upgrade", "websocket")
		h.Set("Connection", "Upgrade")
		h.Set("Sec-WebSocket-Accept", "not-the-token")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer srv.Close()

	res, _ := run(t, build(t, ""), request(t, srv))
	if res.Err == nil {
		t.Fatal("a 101 with the wrong accept token was accepted")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonProtocol {
		t.Errorf("reason = %q, want protocol", got)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	srv := upgrader(t, "", func(conn net.Conn, r *bufio.Reader) {
		f, err := readFrame(r, 4096)
		if err != nil {
			return
		}
		_ = writeFrame(conn, opText, append([]byte("echo:"), f.payload...))
	})
	defer srv.Close()

	p := build(t, "send: hello\nexpect: '^echo:(?P<said>.+)$'\n")
	res, rec := run(t, p, request(t, srv))
	if res.Err != nil {
		t.Fatalf("round trip failed: %v", res.Err)
	}
	for _, s := range rec.Samples() {
		if s.Name == probe.SeriesExpect {
			if got := s.Labels.Get("said"); got != "hello" {
				t.Errorf("said = %q, want hello", got)
			}
			return
		}
	}
	t.Error("the capture group was not exported")
}

func TestPingPong(t *testing.T) {
	srv := upgrader(t, "", func(conn net.Conn, r *bufio.Reader) {
		f, err := readFrame(r, 4096)
		if err != nil || f.op != opPing {
			return
		}
		_ = writeFrame(conn, opPong, f.payload)
	})
	defer srv.Close()

	if res, _ := run(t, build(t, "ping: true\n"), request(t, srv)); res.Err != nil {
		t.Fatalf("ping/pong failed: %v", res.Err)
	}
}

func TestSubprotocolMustBeChosen(t *testing.T) {
	srv := upgrader(t, "", nil)
	defer srv.Close()
	res, _ := run(t, build(t, "subprotocols: [mqtt]\n"), request(t, srv))
	if res.Err == nil {
		t.Error("a server that chose no subprotocol was accepted")
	}

	ok := upgrader(t, "mqtt", nil)
	defer ok.Close()
	if res, _ := run(t, build(t, "subprotocols: [mqtt]\n"), request(t, ok)); res.Err != nil {
		t.Errorf("a server that chose the subprotocol was refused: %v", res.Err)
	}
}

func TestCloseFrameEndsTheWait(t *testing.T) {
	srv := upgrader(t, "", func(conn net.Conn, r *bufio.Reader) {
		if _, err := readFrame(r, 4096); err != nil {
			return
		}
		_ = writeFrame(conn, opClose, []byte{0x03, 0xf3, 'b', 'y', 'e'})
	})
	defer srv.Close()

	res, _ := run(t, build(t, "send: hi\nexpect: anything\n"), request(t, srv))
	if res.Err == nil {
		t.Fatal("a close frame was read as the answer")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonProtocol {
		t.Errorf("reason = %q, want protocol", got)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("short"), make([]byte, 200), make([]byte, 70000)} {
		var buf writerReader
		if err := writeFrame(&buf, opBinary, payload); err != nil {
			t.Fatal(err)
		}
		f, err := readFrame(bufio.NewReader(&buf), 1<<20)
		if err != nil {
			t.Fatalf("payload of %d bytes: %v", len(payload), err)
		}
		if f.op != opBinary || !f.final || len(f.payload) != len(payload) {
			t.Errorf("payload of %d bytes came back as %v/%d", len(payload), f.op, len(f.payload))
		}
	}
}

// A frame claiming more than the limit is refused before it is allocated.
func TestFrameLimit(t *testing.T) {
	var buf writerReader
	if err := writeFrame(&buf, opText, make([]byte, 5000)); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(bufio.NewReader(&buf), 1024); err == nil {
		t.Error("an oversized frame was read")
	}
}

// writerReader is a byte queue: whatever is written can then be read.
type writerReader struct{ b []byte }

func (w *writerReader) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }

func (w *writerReader) Read(p []byte) (int, error) {
	if len(w.b) == 0 {
		return 0, net.ErrClosed
	}
	n := copy(p, w.b)
	w.b = w.b[n:]
	return n, nil
}

// A proxy that flushes per chunk fragments everything; matching only the first
// fragment fails a perfectly healthy service.
func TestFragmentedMessageIsJoined(t *testing.T) {
	srv := upgrader(t, "", func(conn net.Conn, r *bufio.Reader) {
		if _, err := readFrame(r, 4096); err != nil {
			return
		}
		_ = writeFragment(conn, opText, []byte("he"), false)
		_ = writeFrame(conn, opPing, []byte("mid"))
		_ = writeFragment(conn, opContinuation, []byte("llo"), true)
	})
	defer srv.Close()

	p := build(t, "send: hi\nexpect: '^hello$'\n")
	if res, _ := run(t, p, request(t, srv)); res.Err != nil {
		t.Fatalf("a fragmented reply was not joined: %v", res.Err)
	}
}

// A shorthand target carries the port; the URL is nil.
func TestShorthandTargetKeepsItsPort(t *testing.T) {
	srv := upgrader(t, "", nil)
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	addr, _ := netip.ParseAddr(u.Hostname())
	req := probe.Request{
		Target:  probe.Target{Name: u.Host, Host: u.Hostname(), Port: port},
		Backend: probe.Backend{Addr: addr, Port: port},
	}
	if res, _ := run(t, build(t, ""), req); res.Err != nil {
		t.Fatalf("a target given as host:port was not dialled there: %v", res.Err)
	}
}

// Offering a subprotocol is how a gateway that routes elsewhere is caught.
func TestUnofferedSubprotocolIsRefused(t *testing.T) {
	srv := upgrader(t, "amqp", nil)
	defer srv.Close()

	res, _ := run(t, build(t, "subprotocols: [mqtt]\n"), request(t, srv))
	if res.Err == nil {
		t.Fatal("the server chose a subprotocol that was never offered, and was accepted")
	}
	if got := probe.ReasonOf(res.Err); got != probe.ReasonProtocol {
		t.Errorf("reason = %q, want protocol", got)
	}
}

func TestReservedBitIsRefused(t *testing.T) {
	var buf writerReader
	if err := writeFrame(&buf, opText, []byte("x")); err != nil {
		t.Fatal(err)
	}
	buf.b[0] |= 0x40 // RSV2, as permessage-deflate would set RSV1
	if _, err := readFrame(bufio.NewReader(&buf), 4096); err == nil {
		t.Error("a frame with a reserved bit set was read as plaintext")
	}
}

func TestOversizedControlFrameIsRefused(t *testing.T) {
	var buf writerReader
	if err := writeFrame(&buf, opPing, make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(bufio.NewReader(&buf), 4096); err == nil {
		t.Error("a 200-byte ping was accepted; RFC 6455 caps a control frame at 125")
	}
}

func TestExchangesAreExclusive(t *testing.T) {
	for _, options := range []string{
		"ping: true\nsend: hi\n",
		"expect: hello\n",
	} {
		if _, err := options_(options); err == nil {
			t.Errorf("accepted a contradictory exchange:\n%s", options)
		}
	}
}

// options_ builds a prober and returns the error instead of failing the test.
func options_(s string) (probe.Prober, error) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(s), &node); err != nil {
		return nil, err
	}
	pc := config.Probe{Name: "test", Type: "websocket"}
	if len(node.Content) > 0 {
		pc.Options = *node.Content[0]
	}
	return New(pc)
}

// writeFragment is writeFrame without the FIN bit forced on, for tests that
// need a server to split a message.
func writeFragment(w io.Writer, op opcode, payload []byte, final bool) error {
	var head []byte
	b := byte(op)
	if final {
		b |= 0x80
	}
	head = append(head, b, byte(len(payload)))
	if _, err := w.Write(head); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
