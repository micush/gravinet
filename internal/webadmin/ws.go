package webadmin

// A minimal RFC 6455 WebSocket server implementation. gravinet has zero
// third-party Go dependencies (see go.mod) and the shell feature is the only
// thing in the whole admin surface that genuinely needs a full-duplex,
// low-latency, long-lived connection — everything else fits request/response
// or the request/response proxy in cluster.go. Pulling in a WebSocket
// library for that one feature seemed like a worse trade than about 250
// lines of protocol code we can read end to end. This implements exactly
// what the browser's real WebSocket object requires and nothing more:
// the opening handshake, text/binary/close/ping/pong frames, continuation
// frames (a large PTY-output message can legitimately be fragmented by some
// clients), and correct masking direction (client frames masked, server
// frames not). It does not implement any extension (permessage-deflate is
// never negotiated) or the pathological cases a public-internet-facing
// WebSocket server would need to harden against — this endpoint is only
// ever reachable by an already-authenticated admin session.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xa
)

// wsMaxMessage caps a single (possibly reassembled from continuation frames)
// message. PTY output is chunked well under this in practice; this is a
// backstop against a misbehaving or hostile peer forcing unbounded buffering.
const wsMaxMessage = 4 << 20 // 4 MiB

// wsGUID is the fixed handshake magic string from RFC 6455 §1.3.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsConn is an upgraded connection. Not safe for concurrent writes from
// multiple goroutines (callers serialize their own writes); reads are only
// ever done from one goroutine per connection in this codebase too.
type wsConn struct {
	rw   *bufio.ReadWriter
	conn net.Conn
}

// upgradeWebSocket performs the opening handshake and hijacks the underlying
// connection. On success, the caller owns conn from this point on — no more
// use of w/r is valid. On failure, it has already written an HTTP error
// response and the caller should just return.
func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "expected a WebSocket upgrade request", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return nil, errors.New("ResponseWriter does not support hijacking")
	}
	accept := wsAcceptKey(key)
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack: %w", err)
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writing handshake response: %w", err)
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("flushing handshake response: %w", err)
	}
	return &wsConn{rw: rw, conn: conn}, nil
}

// wsAcceptKey computes Sec-WebSocket-Accept per RFC 6455 §1.3: base64 of the
// SHA-1 of the client's key concatenated with the fixed GUID.
func wsAcceptKey(clientKey string) string {
	h := sha1.New()
	h.Write([]byte(clientKey))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func (c *wsConn) Close() error { return c.conn.Close() }

// ReadMessage returns the next complete message (reassembling continuation
// frames), its opcode (wsOpText or wsOpBinary), and any error. Ping/pong/close
// frames are handled internally (a pong is sent automatically for a ping; a
// close frame returns io.EOF after echoing a close back).
func (c *wsConn) ReadMessage() (opcode int, payload []byte, err error) {
	var msg []byte
	msgOp := -1
	for {
		fin, op, frame, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case wsOpPing:
			if err := c.writeFrame(true, wsOpPong, frame); err != nil {
				return 0, nil, err
			}
			continue
		case wsOpPong:
			continue // nothing to do; we don't send unsolicited pings
		case wsOpClose:
			_ = c.writeFrame(true, wsOpClose, nil)
			return 0, nil, io.EOF
		case wsOpContinuation:
			if msgOp < 0 {
				return 0, nil, errors.New("continuation frame with no prior message")
			}
		default: // text or binary: starts a new message
			if msgOp >= 0 {
				return 0, nil, errors.New("new message started before prior one finished")
			}
			msgOp = op
		}
		if len(msg)+len(frame) > wsMaxMessage {
			return 0, nil, fmt.Errorf("message exceeds %d bytes", wsMaxMessage)
		}
		msg = append(msg, frame...)
		if fin {
			return msgOp, msg, nil
		}
	}
}

// readFrame reads exactly one frame off the wire and unmasks it if needed
// (client->server frames are always masked per spec; this rejects an
// unmasked client frame rather than silently accepting it).
func (c *wsConn) readFrame() (fin bool, opcode int, payload []byte, err error) {
	var head [2]byte
	if _, err := io.ReadFull(c.rw, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin = head[0]&0x80 != 0
	opcode = int(head[0] & 0x0f)
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.rw, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.rw, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > wsMaxMessage {
		return false, 0, nil, fmt.Errorf("frame length %d exceeds %d", length, wsMaxMessage)
	}
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(c.rw, maskKey[:]); err != nil {
			return false, 0, nil, err
		}
	} else if opcode == wsOpText || opcode == wsOpBinary || opcode == wsOpContinuation {
		// RFC 6455 §5.1: a server MUST close the connection if it receives an
		// unmasked data frame from a client. Control frames (ping/pong/close)
		// are exempt from carrying app data we'd need to unmask correctly, but
		// a real browser always masks everything, so being strict here only
		// ever rejects a non-browser/malformed client.
		return false, 0, nil, errors.New("received unmasked client frame")
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.rw, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return fin, opcode, payload, nil
}

// WriteMessage sends payload as a single, unfragmented frame of the given
// opcode (wsOpText or wsOpBinary). Server frames are never masked.
func (c *wsConn) WriteMessage(opcode int, payload []byte) error {
	return c.writeFrame(true, opcode, payload)
}

func (c *wsConn) writeFrame(fin bool, opcode int, payload []byte) error {
	var head [10]byte
	n := 1
	if fin {
		head[0] = 0x80
	}
	head[0] |= byte(opcode)
	switch {
	case len(payload) <= 125:
		head[1] = byte(len(payload))
		n = 2
	case len(payload) <= 0xffff:
		head[1] = 126
		binary.BigEndian.PutUint16(head[2:4], uint16(len(payload)))
		n = 4
	default:
		head[1] = 127
		binary.BigEndian.PutUint64(head[2:10], uint64(len(payload)))
		n = 10
	}
	if _, err := c.rw.Write(head[:n]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.rw.Write(payload); err != nil {
			return err
		}
	}
	return c.rw.Flush()
}
