package transport

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// The TCP/TLS fallback carries the same datagrams the UDP underlay does, framed
// on the stream as a 2-byte big-endian length followed by that many bytes. A
// mesh frame is always smaller than a UDP datagram (well under 1500 bytes after
// PMTU), so 16 bits of length is plenty and bounds per-frame allocation.

const maxFrameLen = 1 << 16 // 65535, the 2-byte length ceiling

// frameConn wraps a stream connection with a write mutex so concurrent senders
// can't interleave each other's frames on the wire.
type frameConn struct {
	w  io.Writer
	mu sync.Mutex
}

// writeFrame writes one length-prefixed frame. The 2-byte header and payload go
// out under the lock as a single buffered write so frames never interleave.
func (fc *frameConn) writeFrame(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) > maxFrameLen-1 {
		return fmt.Errorf("transport: frame too large (%d bytes)", len(payload))
	}
	buf := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(payload)))
	copy(buf[2:], payload)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	_, err := fc.w.Write(buf)
	return err
}

// readFrame reads one length-prefixed frame into a freshly allocated slice.
// Returns io.EOF when the stream ends cleanly between frames.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 {
		return nil, fmt.Errorf("transport: zero-length frame")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
