package transport

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

// mustListenUDP4 binds an ephemeral loopback UDP socket for test use.
func mustListenUDP4(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	return c
}

// TestSendRoundRobinsAcrossSockets proves Send no longer pins every outbound
// datagram to conns4[0]: with three distinct sockets standing in for a
// REUSEPORT set, repeated sends should spread across all three, observable
// on the receiving end by the distinct source ports datagrams arrive from
// (real REUSEPORT sockets all share one port and can't be told apart this
// way, but the round-robin index logic under test doesn't know or care
// whether the sockets happen to share a port — this exercises the same
// selection code with sockets that are easy to tell apart).
func TestSendRoundRobinsAcrossSockets(t *testing.T) {
	a, b, c := mustListenUDP4(t), mustListenUDP4(t), mustListenUDP4(t)
	defer a.Close()
	defer b.Close()
	defer c.Close()

	tr := &Transport{conns4: []*net.UDPConn{a, b, c}}

	recv := mustListenUDP4(t)
	defer recv.Close()
	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.LocalAddr().(*net.UDPAddr).Port))

	seenPorts := make(map[int]int) // source port -> count
	const n = 30
	for i := 0; i < n; i++ {
		if err := tr.Send(dst, []byte("x")); err != nil {
			t.Fatalf("Send #%d: %v", i, err)
		}
	}
	recv.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	for i := 0; i < n; i++ {
		_, from, err := recv.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("ReadFromUDP #%d: %v", i, err)
		}
		seenPorts[from.Port]++
	}

	wantPorts := map[int]bool{
		a.LocalAddr().(*net.UDPAddr).Port: true,
		b.LocalAddr().(*net.UDPAddr).Port: true,
		c.LocalAddr().(*net.UDPAddr).Port: true,
	}
	if len(seenPorts) != 3 {
		t.Fatalf("datagrams arrived from %d distinct source ports (%v), want 3 — Send is not spreading across all bound sockets", len(seenPorts), seenPorts)
	}
	for p := range seenPorts {
		if !wantPorts[p] {
			t.Fatalf("datagram arrived from unexpected source port %d", p)
		}
	}
	// Round-robin over 30 sends across 3 sockets should be exactly even (10
	// each) — not just "all three got used at least once".
	for p, got := range seenPorts {
		if got != n/3 {
			t.Fatalf("port %d got %d datagrams, want exactly %d (round-robin should be perfectly even here)", p, got, n/3)
		}
	}
}

// TestSendSingleSocketUnaffected confirms the common case (one socket per
// family — reuseport disabled, or a single-worker setup) still always uses
// that one socket: len==1 must make the round-robin a no-op, not an error.
func TestSendSingleSocketUnaffected(t *testing.T) {
	a := mustListenUDP4(t)
	defer a.Close()
	tr := &Transport{conns4: []*net.UDPConn{a}}

	recv := mustListenUDP4(t)
	defer recv.Close()
	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.LocalAddr().(*net.UDPAddr).Port))

	for i := 0; i < 5; i++ {
		if err := tr.Send(dst, []byte("x")); err != nil {
			t.Fatalf("Send #%d: %v", i, err)
		}
	}
	recv.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	wantPort := a.LocalAddr().(*net.UDPAddr).Port
	for i := 0; i < 5; i++ {
		_, from, err := recv.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("ReadFromUDP #%d: %v", i, err)
		}
		if from.Port != wantPort {
			t.Fatalf("datagram #%d from port %d, want %d (the only socket)", i, from.Port, wantPort)
		}
	}
}
