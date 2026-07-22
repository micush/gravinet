package transport

import (
	"net/netip"
	"syscall"
	"testing"
)

// TestSocketBuffersApplied verifies the bound UDP socket actually carries an
// enlarged SO_RCVBUF/SO_SNDBUF (root here, so the FORCE path should bypass the
// rmem_max clamp). The kernel reports back roughly twice the requested size.
func TestSocketBuffersApplied(t *testing.T) {
	tr, err := Open(Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1,
		SocketBuffer: 4 << 20,
		Handler:      func([]byte, netip.AddrPort, Family) {},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tr.Close()

	raw, err := tr.conns4[0].SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var rcv, snd int
	_ = raw.Control(func(fd uintptr) {
		rcv, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
		snd, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	})
	t.Logf("SO_RCVBUF=%d SO_SNDBUF=%d (requested %d)", rcv, snd, 4<<20)
	if rcv < 2<<20 {
		t.Errorf("rcvbuf not enlarged: got %d, want >= %d", rcv, 2<<20)
	}
	if snd < 2<<20 {
		t.Errorf("sndbuf not enlarged: got %d, want >= %d", snd, 2<<20)
	}
}
