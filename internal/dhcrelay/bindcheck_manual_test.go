package dhcrelay

import (
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

// The property the two-socket split exists for, checked against real sockets.
//
// Set RELAY_IF to a client-facing interface and RELAY_ADDR to its address.
// Skipped by default: it needs port 67 and CAP_NET_RAW, which CI does not have.
//
// Two things are asserted. Both of a link's sockets bind port 67 at once, and
// the server-facing one is delivered a datagram that arrives on an interface
// other than the client-facing one's — which is every reply, in every topology
// where the server is not already on the client's LAN.
func TestManualTwoSocketsBindAndReplyArrivesOffLink(t *testing.T) {
	ifName, addr := os.Getenv("RELAY_IF"), os.Getenv("RELAY_ADDR")
	if ifName == "" || addr == "" {
		t.Skip("set RELAY_IF and RELAY_ADDR")
	}
	self := netip.MustParseAddr(addr)

	client, err := listenClient(ifName)
	if err != nil {
		t.Fatalf("client-facing socket: %v", err)
	}
	defer client.Close()

	server, err := listenServer(self)
	if err != nil {
		t.Fatalf("server-facing socket failed while the client-facing one was open: %v", err)
	}
	defer server.Close()
	t.Logf("bound %s (client-facing, confined to %s) and %s (server-facing)",
		client.LocalAddr(), ifName, server.LocalAddr())

	// Stand in for the upstream server's reply: sent from off-link, so it
	// arrives by loopback rather than by ifName.
	tx, err := net.Dial("udp4", netip.AddrPortFrom(self, ServerPort).String())
	if err != nil {
		t.Fatalf("dialling the server-facing socket: %v", err)
	}
	defer tx.Close()
	reply := pkt(opReply)
	setAddr(reply, offGiaddr, addr)
	if _, err := tx.Write(reply); err != nil {
		t.Fatalf("sending: %v", err)
	}

	_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, maxLen)
	n, from, err := server.ReadFrom(buf)
	if err != nil {
		t.Fatalf("the server-facing socket was not delivered the reply: %v", err)
	}
	t.Logf("server-facing socket received %d bytes from %s", n, from)

	// And the client-facing socket, confined to the link, was not.
	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := client.ReadFrom(buf); err == nil {
		t.Error("the device-confined socket also received it; the confinement is not doing anything")
	}
}
