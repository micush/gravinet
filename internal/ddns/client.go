package ddns

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

// exchangeTimeout bounds one query or update. Short, because this runs on a
// timer and a wedged server must not hold the loop past its next tick; the
// retry is the next interval rather than a longer wait here.
const exchangeTimeout = 5 * time.Second

// dnsPort is appended to a server address that does not carry one, so an
// operator can type an address into the resolver page — which is what that
// field is for — and still have it work as an update target.
const dnsPort = "53"

// exchange sends one DNS message and returns the reply.
//
// UDP first, then TCP if the answer comes back truncated. An UPDATE is small
// and a signed one is still small, so truncation is unlikely on the way out;
// what does get truncated is the SOA lookup against a resolver that wants to
// tell us about a large delegation, and falling back is one branch.
//
// The reply's id is checked against the query's. Without that a late reply to
// the previous tick, arriving on a freshly opened socket, would be read as the
// answer to this one — off-path spoofing matters less on the link this runs on
// than simple crossed wires do, but the check costs nothing and rules out both.
func exchange(server string, id uint16, msg []byte) ([]byte, error) {
	addr := withPort(server)

	reply, truncated, err := exchangeUDP(addr, id, msg)
	if err == nil && !truncated {
		return reply, nil
	}
	if err != nil && !truncated {
		return nil, err
	}
	return exchangeTCP(addr, id, msg)
}

func withPort(server string) string {
	s := strings.TrimSpace(server)
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	return net.JoinHostPort(s, dnsPort)
}

func exchangeUDP(addr string, id uint16, msg []byte) (reply []byte, truncated bool, err error) {
	c, err := net.DialTimeout("udp", addr, exchangeTimeout)
	if err != nil {
		return nil, false, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(exchangeTimeout))
	if _, err := c.Write(msg); err != nil {
		return nil, false, fmt.Errorf("send to %s: %w", addr, err)
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return nil, false, fmt.Errorf("no reply from %s: %w", addr, err)
	}
	if n < 12 {
		return nil, false, fmt.Errorf("short reply from %s (%d bytes)", addr, n)
	}
	if got := binary.BigEndian.Uint16(buf[0:2]); got != id {
		return nil, false, fmt.Errorf("reply from %s answers a different query (id %d, wanted %d)", addr, got, id)
	}
	if binary.BigEndian.Uint16(buf[2:4])&0x0200 != 0 { // TC
		return nil, true, nil
	}
	return buf[:n], false, nil
}

func exchangeTCP(addr string, id uint16, msg []byte) ([]byte, error) {
	c, err := net.DialTimeout("tcp", addr, exchangeTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s over tcp: %w", addr, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(exchangeTimeout))

	// DNS over TCP length-prefixes each message with a 16-bit count.
	framed := binary.BigEndian.AppendUint16(nil, uint16(len(msg)))
	if _, err := c.Write(append(framed, msg...)); err != nil {
		return nil, fmt.Errorf("send to %s over tcp: %w", addr, err)
	}
	var lenBuf [2]byte
	if _, err := readFull(c, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("no reply from %s over tcp: %w", addr, err)
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n < 12 {
		return nil, fmt.Errorf("short reply from %s over tcp (%d bytes)", addr, n)
	}
	buf := make([]byte, n)
	if _, err := readFull(c, buf); err != nil {
		return nil, fmt.Errorf("truncated reply from %s over tcp: %w", addr, err)
	}
	if got := binary.BigEndian.Uint16(buf[0:2]); got != id {
		return nil, fmt.Errorf("reply from %s answers a different query (id %d, wanted %d)", addr, got, id)
	}
	return buf, nil
}

func readFull(c net.Conn, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := c.Read(b[total:])
		if n > 0 {
			total += n
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
