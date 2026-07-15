//go:build linux

package transport

import (
	"net"
	"syscall"
	"testing"
)

// TestDisableNagleSetsTCPNoDelay checks the actual kernel socket option, not
// just that SetNoDelay returned no error: disableNagle exists specifically
// because the mesh's TCP/TLS fallback carries small, sporadic frames where
// Nagle's default-on delay is the whole problem (see disableNagle's doc
// comment in tcptls.go) — a test that only checked for a nil error wouldn't
// catch a wrong constant or a build tag mismatch actually leaving Nagle on.
func TestDisableNagleSetsTCPNoDelay(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	acceptedCh := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			acceptedCh <- c
		}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-acceptedCh
	defer server.Close()

	disableNagle(client)
	disableNagle(server)

	checkNoDelay := func(name string, c net.Conn) {
		t.Helper()
		tc, ok := c.(*net.TCPConn)
		if !ok {
			t.Fatalf("%s: not a *net.TCPConn", name)
		}
		sc, err := tc.SyscallConn()
		if err != nil {
			t.Fatalf("%s: SyscallConn: %v", name, err)
		}
		var val int
		var sockErr error
		if err := sc.Control(func(fd uintptr) {
			val, sockErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY)
		}); err != nil {
			t.Fatalf("%s: Control: %v", name, err)
		}
		if sockErr != nil {
			t.Fatalf("%s: GetsockoptInt(TCP_NODELAY): %v", name, sockErr)
		}
		if val == 0 {
			t.Fatalf("%s: TCP_NODELAY is off after disableNagle", name)
		}
	}
	checkNoDelay("client", client)
	checkNoDelay("server", server)
}

// TestDisableNagleIgnoresNonTCPConn checks the best-effort fallback disableNagle's
// doc comment describes: a net.Conn that isn't a *net.TCPConn (net.Pipe's
// in-memory implementation, standing in for some future non-TCP dialer) must
// be left alone silently, not panic or error.
func TestDisableNagleIgnoresNonTCPConn(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	disableNagle(c1) // must not panic
}
