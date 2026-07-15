package transport

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// TestRoundTrip binds two transports on loopback and confirms a datagram sent
// by one is delivered to the other's handler.
func TestRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	done := make(chan struct{}, 1)

	recv, err := Open(Options{
		BindAddr:    "127.0.0.1",
		PrimaryPort: 0, // ephemeral; reuseport groups still share the chosen port
		EnableV4:    true,
		Workers:     2,
		Handler: func(p []byte, from netip.AddrPort, fam Family) {
			mu.Lock()
			got = append([]byte(nil), p...)
			mu.Unlock()
			select {
			case done <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("open recv: %v", err)
	}
	defer recv.Close()

	// PrimaryPort 0 yields an OS-chosen port; read it back for the sender.
	if recv.Port() == 0 {
		t.Skip("ephemeral port selection returned 0; skipping")
	}

	send, err := Open(Options{
		BindAddr:    "127.0.0.1",
		PrimaryPort: 0,
		EnableV4:    true,
		Workers:     1,
		Handler:     func([]byte, netip.AddrPort, Family) {},
	})
	if err != nil {
		t.Fatalf("open send: %v", err)
	}
	defer send.Close()

	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.Port()))
	want := []byte("gravinet-underlay-hello")
	if err := send.Send(dst, want); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for datagram")
	}
	mu.Lock()
	defer mu.Unlock()
	if string(got) != string(want) {
		t.Fatalf("payload mismatch: got %q want %q", got, want)
	}
}

// TestPortFallback drives bindGroup with a fake binder that refuses the primary
// port, proving the daemon walks to a fallback.
func TestPortFallback(t *testing.T) {
	const primary, fallback = 59820, 59821
	fake := func(network, addr string) (*net.UDPConn, error) {
		host, portStr, _ := net.SplitHostPort(addr)
		if portStr == fmt.Sprint(primary) {
			return nil, fmt.Errorf("simulated EADDRINUSE on %s", addr)
		}
		// bind the literal requested port on loopback so the returned port matches
		la, err := net.ResolveUDPAddr(network, net.JoinHostPort(host, portStr))
		if err != nil {
			return nil, err
		}
		return net.ListenUDP(network, la)
	}
	o := Options{BindAddr: "127.0.0.1", PrimaryPort: primary, FallbackPorts: []int{fallback}, EnableV4: true, Workers: 1}
	port, c4, c6, err := bindGroup(fake, []int{primary, fallback}, o, 1)
	if err != nil {
		t.Fatalf("bindGroup: %v", err)
	}
	for _, c := range c4 {
		c.Close()
	}
	for _, c := range c6 {
		c.Close()
	}
	if port != fallback {
		t.Fatalf("expected fallback to port %d, got %d", fallback, port)
	}
}

func TestNoFamilyIsError(t *testing.T) {
	_, err := Open(Options{EnableV4: false, EnableV6: false, Workers: 1,
		Handler: func([]byte, netip.AddrPort, Family) {}})
	if err == nil {
		t.Fatal("expected error when no address family is enabled")
	}
}
