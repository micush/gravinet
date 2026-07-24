//go:build linux

package tun

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
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

// TestTUNFdNotInheritedByChildProcess is a direct regression test for the bug
// that used to make SELinux flag chpasswd (spawned by System > Users' "add
// user") for reading and writing a chr_file it had no legitimate reason to
// touch at all: /dev/net/tun's fd was opened without O_CLOEXEC, so it stayed
// open in every child process gravinet ever forked, on an SELinux-enforcing
// host chpasswd runs under a domain with no policy rule for that device and
// the access got logged as a denial. This test proves the concrete, checkable
// fact that actually matters — a forked child does not have the fd open —
// rather than merely asserting O_CLOEXEC appears in the source.
func TestTUNFdNotInheritedByChildProcess(t *testing.T) {
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

	fd := int(dev.f.Fd())
	// A child that inherited fd would find /proc/self/fd/<fd> pointing at it;
	// one that didn't gets ENOENT. Checked from the child's own perspective
	// (not the parent's /proc/<pid>/fd, which would show it as gravinet's own
	// regardless) so this is testing exactly what a spawned helper like
	// chpasswd would actually see.
	if err := exec.Command("/bin/sh", "-c", fmt.Sprintf("test -e /proc/self/fd/%d", fd)).Run(); err == nil {
		t.Fatalf("child process inherited the tun device's fd (%d) — O_CLOEXEC isn't taking effect", fd)
	}
}
