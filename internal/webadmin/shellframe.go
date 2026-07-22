package webadmin

// The node-to-node hop of a proxied shell session (this node relaying to a
// managed peer) rides a raw hijacked TCP/TLS stream, not a real WebSocket —
// both ends are gravinet's own code, so there's no need to pay for a second
// WebSocket handshake and framing layer just to satisfy a spec neither side
// needs satisfied. This file is the tiny length-prefixed framing that raw
// stream uses instead: a byte stream has no message boundaries of its own,
// so something has to mark them. The browser-facing hop (ws.go) doesn't need
// this — a WebSocket message already is one unit — and the shell relay code
// (shell.go) converts between the two: one WS message in, one shellFrame out
// the other side, and back.

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	shellFrameData   = 0x01 // payload = raw bytes: keystrokes (in) or PTY output (out)
	shellFrameResize = 0x02 // payload = 4 bytes: uint16 rows, uint16 cols
	shellFrameExit   = 0x03 // payload = 4 bytes: int32 exit code. Sent once, then the stream closes.
)

// shellFrameMaxPayload bounds a single frame. PTY I/O is chunked in modest
// pieces in practice (see shellCopyBufSize); this is a backstop against a
// corrupted length prefix turning into an unbounded read.
const shellFrameMaxPayload = 4 << 20 // 4 MiB

// writeShellFrame writes one frame: 1 byte type + 4 byte big-endian length +
// payload.
func writeShellFrame(w io.Writer, typ byte, payload []byte) error {
	var head [5]byte
	head[0] = typ
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// readShellFrame reads one frame written by writeShellFrame.
func readShellFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > shellFrameMaxPayload {
		return 0, nil, fmt.Errorf("shell frame length %d exceeds %d", n, shellFrameMaxPayload)
	}
	payload = make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return head[0], payload, nil
}

// encodeResize/decodeResize convert a rows/cols pair to and from the 4-byte
// payload a shellFrameResize frame carries.
func encodeResize(rows, cols int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], uint16(rows))
	binary.BigEndian.PutUint16(b[2:4], uint16(cols))
	return b
}

func decodeResize(b []byte) (rows, cols int, ok bool) {
	if len(b) != 4 {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint16(b[0:2])), int(binary.BigEndian.Uint16(b[2:4])), true
}

func encodeExit(code int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(code)))
	return b
}

func decodeExit(b []byte) (code int, ok bool) {
	if len(b) != 4 {
		return 0, false
	}
	return int(int32(binary.BigEndian.Uint32(b))), true
}
