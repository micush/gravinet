//go:build linux

package transport

import (
	"context"
	"net"
	"syscall"
	"testing"
)

// The underlay socket must carry the don't-fragment / PMTU-discovery option so
// an oversized datagram is dropped (EMSGSIZE) rather than silently IP-fragmented.
func TestControlSetsDontFragment(t *testing.T) {
	lc := net.ListenConfig{Control: control}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	sc, err := pc.(*net.UDPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var val int
	var gErr error
	if err := sc.Control(func(fd uintptr) {
		val, gErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IP, ipMTUDiscover)
	}); err != nil {
		t.Fatal(err)
	}
	if gErr != nil {
		t.Fatalf("getsockopt IP_MTU_DISCOVER: %v", gErr)
	}
	if val != pmtuDiscDo {
		t.Errorf("IP_MTU_DISCOVER=%d, want IP_PMTUDISC_DO=%d", val, pmtuDiscDo)
	}
}
