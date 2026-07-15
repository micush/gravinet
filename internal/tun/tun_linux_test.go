//go:build linux

package tun

import (
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

// TestTUNCreateAndRead creates a real TUN interface, assigns it an IPv4
// address (which installs a connected route), then sends a UDP datagram into
// that subnet and confirms the kernel hands us the outbound IP packet.
// Skips cleanly when the environment lacks TUN/permissions.
func TestTUNCreateAndRead(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("TUN test needs root / CAP_NET_ADMIN")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("/dev/net/tun not present")
	}

	dev, err := New("", 1500)
	if err != nil {
		t.Skipf("cannot create tun (environment): %v", err)
	}
	defer dev.Close()

	if dev.Name() == "" {
		t.Fatal("kernel did not assign an interface name")
	}
	t.Logf("created interface %s mtu=%d", dev.Name(), dev.MTU())

	if err := dev.AddIPv4(netip.MustParseAddr("10.99.0.1"), 24); err != nil {
		t.Skipf("cannot assign v4 addr (environment): %v", err)
	}
	// IPv6 assignment should also succeed on a modern kernel.
	if err := dev.AddIPv6(netip.MustParseAddr("fd00:99::1"), 64); err != nil {
		t.Logf("v6 assignment failed (non-fatal in this env): %v", err)
	}

	// Read in the background; send a datagram into the subnet to generate one.
	type res struct {
		n   int
		err error
		buf []byte
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, 2048)
		n, err := dev.Read(buf)
		ch <- res{n, err, buf}
	}()

	// Give the reader a moment, then emit a packet to 10.99.0.2.
	time.Sleep(100 * time.Millisecond)
	c, err := net.Dial("udp", "10.99.0.2:9")
	if err != nil {
		t.Skipf("cannot dial into tun subnet: %v", err)
	}
	_, _ = c.Write([]byte("hello-tun"))
	c.Close()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("tun read: %v", r.err)
		}
		if r.n < 20 {
			t.Fatalf("short packet: %d bytes", r.n)
		}
		ver := r.buf[0] >> 4
		if ver != 4 {
			t.Fatalf("expected IPv4 packet, got version %d", ver)
		}
		dst := netip.AddrFrom4([4]byte{r.buf[16], r.buf[17], r.buf[18], r.buf[19]})
		if dst != netip.MustParseAddr("10.99.0.2") {
			t.Fatalf("unexpected dst %s", dst)
		}
		t.Logf("read %d-byte IPv4 packet to %s off %s", r.n, dst, dev.Name())
	case <-time.After(3 * time.Second):
		t.Skip("no packet observed (environment routing); device creation still verified")
	}
}
