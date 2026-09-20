package websocket

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

// The frame layer is written out here rather than taken from a library: a probe
// opens one connection, says one thing and reads one answer, and that is a
// hundred lines of RFC 6455 against a dependency this project does not have.

type opcode byte

const (
	opContinuation opcode = 0x0
	opText         opcode = 0x1
	opBinary       opcode = 0x2
	opClose        opcode = 0x8
	opPing         opcode = 0x9
	opPong         opcode = 0xA
)

func (o opcode) String() string {
	switch o {
	case opContinuation:
		return "continuation"
	case opText:
		return "text"
	case opBinary:
		return "binary"
	case opClose:
		return "close"
	case opPing:
		return "ping"
	case opPong:
		return "pong"
	}
	return fmt.Sprintf("0x%x", byte(o))
}

// frame is one message frame, unmasked.
type frame struct {
	op      opcode
	final   bool
	payload []byte
}

// writeFrame sends one masked frame, which is what a client must always send.
func writeFrame(w io.Writer, op opcode, payload []byte) error {
	var head []byte
	head = append(head, 0x80|byte(op)) // FIN set: the probe never fragments

	n := len(payload)
	switch {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n <= 0xFFFF:
		head = append(head, 0x80|126)
		head = binary.BigEndian.AppendUint16(head, uint16(n))
	default:
		head = append(head, 0x80|127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	head = append(head, mask[:]...)

	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

// maxFrame caps what one reply may cost us, whatever the peer claims to be
// sending.
const maxFrame = 1 << 20

// readFrame reads one frame. A server must not mask, but a frame that is masked
// is unmasked anyway rather than refused: the probe reports what was said, and
// conformance is not what it is checking.
func readFrame(r io.Reader, limit int) (frame, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return frame{}, err
	}
	f := frame{op: opcode(head[0] & 0x0F), final: head[0]&0x80 != 0}

	// RSV with no negotiated extension means the payload is not what it looks
	// like — permessage-deflate turned on unilaterally, most often. Matching a
	// compressed body as plaintext fails for the wrong reason (RFC 6455 §5.2).
	if head[0]&0x70 != 0 {
		return f, fmt.Errorf("%s frame sets a reserved bit, but no extension was negotiated", f.op)
	}

	masked := head[1]&0x80 != 0
	n := uint64(head[1] & 0x7F)
	// A control frame carries its reason, not a payload, and is never split
	// (RFC 6455 §5.5).
	if f.op >= opClose {
		if !f.final {
			return f, fmt.Errorf("fragmented %s frame", f.op)
		}
		if n > 125 {
			return f, fmt.Errorf("%s frame of %d bytes, over the 125-byte limit", f.op, n)
		}
	}
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return f, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return f, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if limit <= 0 || limit > maxFrame {
		limit = maxFrame
	}
	if n > uint64(limit) {
		return f, fmt.Errorf("frame of %d bytes is over the %d-byte limit", n, limit)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return f, err
		}
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return f, err
	}
	if masked {
		for i := range f.payload {
			f.payload[i] ^= mask[i%4]
		}
	}
	return f, nil
}
