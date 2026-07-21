//go:build linux && (amd64 || arm64)

package transport

import (
	"fmt"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TestMmsghdrLayout pins the hand-built struct to the kernel's ABI. Everything
// in the batched path is memory handed straight to a syscall, so a layout drift
// here would not fail loudly — it would silently send garbage addresses or
// read into the wrong offsets. On 64-bit Linux the kernel's struct mmsghdr is a
// 56-byte struct msghdr followed by a 4-byte msg_len, padded out to 64.
func TestMmsghdrLayout(t *testing.T) {
	var m mmsghdr
	if got, want := unsafe.Sizeof(m.hdr), uintptr(56); got != want {
		t.Errorf("sizeof(struct msghdr) = %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(m.n), uintptr(56); got != want {
		t.Errorf("offsetof(msg_len) = %d, want %d", got, want)
	}
	if got, want := unsafe.Sizeof(m), uintptr(64); got != want {
		t.Errorf("sizeof(struct mmsghdr) = %d, want %d", got, want)
	}
	// The array must be tightly packed: the kernel walks it by index.
	var arr [2]mmsghdr
	if got, want := uintptr(unsafe.Pointer(&arr[1]))-uintptr(unsafe.Pointer(&arr[0])), unsafe.Sizeof(m); got != want {
		t.Errorf("mmsghdr array stride = %d, want %d", got, want)
	}
	if got, want := sockaddrLen, syscall.SizeofSockaddrInet6; got != want {
		t.Errorf("sockaddrLen = %d, want %d", got, want)
	}
}

// TestSockaddrRoundTrip checks the marshaller against the decoder for both
// families. A wrong byte order here would send every datagram to the wrong
// host or port, so the port is deliberately asymmetric (0x1234) to catch an
// endianness slip.
func TestSockaddrRoundTrip(t *testing.T) {
	cases := []netip.AddrPort{
		netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0x1234),
		netip.AddrPortFrom(netip.MustParseAddr("203.0.113.7"), 65535),
		netip.AddrPortFrom(netip.MustParseAddr("::1"), 1),
		netip.AddrPortFrom(netip.MustParseAddr("2001:db8::dead:beef"), 51820),
	}
	for _, want := range cases {
		t.Run(want.String(), func(t *testing.T) {
			sa := make([]byte, sockaddrLen)
			n := putSockaddr(sa, want)
			if want.Addr().Is4() && n != syscall.SizeofSockaddrInet4 {
				t.Fatalf("v4 sockaddr length %d, want %d", n, syscall.SizeofSockaddrInet4)
			}
			if !want.Addr().Is4() && n != syscall.SizeofSockaddrInet6 {
				t.Fatalf("v6 sockaddr length %d, want %d", n, syscall.SizeofSockaddrInet6)
			}
			got, ok := addrPortFromSockaddr(sa, n)
			if !ok {
				t.Fatal("addrPortFromSockaddr refused a sockaddr we just built")
			}
			if got != want {
				t.Fatalf("round trip = %v, want %v", got, want)
			}
		})
	}
}

// TestSockaddrRejectsShort makes sure a truncated or unknown-family sockaddr is
// refused rather than decoded into a bogus endpoint.
func TestSockaddrRejectsShort(t *testing.T) {
	sa := make([]byte, sockaddrLen)
	putSockaddr(sa, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 80))
	if _, ok := addrPortFromSockaddr(sa, 3); ok {
		t.Error("accepted a sockaddr shorter than its family requires")
	}
	if _, ok := addrPortFromSockaddr(sa[:2], 16); ok {
		t.Error("accepted a buffer shorter than the declared length")
	}
	other := make([]byte, sockaddrLen) // AF_UNSPEC
	if _, ok := addrPortFromSockaddr(other, sockaddrLen); ok {
		t.Error("accepted an unknown address family")
	}
}

// TestSockaddrV4MappedUnmapped confirms an IPv4-mapped source decodes to a
// plain IPv4 address, matching what the per-packet reader hands the handler
// (netip.Addr.Unmap) — the engine keys sessions on that form.
func TestSockaddrV4MappedUnmapped(t *testing.T) {
	sa := make([]byte, sockaddrLen)
	mapped := netip.AddrPortFrom(netip.MustParseAddr("::ffff:192.0.2.5"), 4242)
	n := putSockaddr(sa, mapped)
	got, ok := addrPortFromSockaddr(sa, n)
	if !ok {
		t.Fatal("decode refused")
	}
	if !got.Addr().Is4() || got.Addr().String() != "192.0.2.5" {
		t.Fatalf("got %v, want the unmapped 192.0.2.5", got)
	}
}

// openLoopback starts a transport on loopback that counts and records what it
// receives.
func openLoopback(t *testing.T, workers int) (*Transport, *received) {
	t.Helper()
	forceBatchable(t)
	r := &received{seen: map[string]int{}}
	tr, err := Open(Options{
		BindAddr:    "127.0.0.1",
		PrimaryPort: 0,
		EnableV4:    true,
		Workers:     workers,
		Handler: func(p []byte, from netip.AddrPort, fam Family) {
			r.add(p)
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return tr, r
}

// forceBatchable raises GOMAXPROCS for the duration of a test so the batched
// path engages even on a single-core machine, where initBatch would otherwise
// (correctly) stay on the per-packet path. It does not create parallelism, it
// only lets the code under test be reached.
func forceBatchable(t *testing.T) {
	t.Helper()
	if runtime.GOMAXPROCS(0) >= 2 {
		return
	}
	prev := runtime.GOMAXPROCS(2)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
}

type received struct {
	mu    sync.Mutex
	seen  map[string]int
	count atomic.Uint64
}

func (r *received) add(p []byte) {
	r.mu.Lock()
	r.seen[string(p)]++
	r.mu.Unlock()
	r.count.Add(1)
}

func (r *received) distinct() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

// TestBatchedRoundTripBulk pushes far more datagrams than the ring holds
// through the real batched path — recvmmsg on the receive side, sendmmsg plus
// ring-full direct fallback on the send side — and requires every distinct
// payload to arrive intact. This is the test that would catch a payload aliased
// to a recycled slot, a mangled sockaddr, or a batch dropped on flush.
func TestBatchedRoundTripBulk(t *testing.T) {
	if !batchAvailable {
		t.Skip("batched path not compiled in")
	}
	recv, got := openLoopback(t, 2)
	defer recv.Close()
	send, _ := openLoopback(t, 1)
	defer send.Close()

	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.Port()))
	const n = 4000 // >> txRingSize, so the direct fallback is exercised too
	buf := make([]byte, 256)
	for i := 0; i < n; i++ {
		// Reuse one buffer across every send: this is exactly what
		// mesh.sealAndSend does, and it only works if enqueue copies.
		msg := fmt.Sprintf("datagram-%05d", i)
		copy(buf, msg)
		if err := send.Send(dst, buf[:len(msg)]); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// UDP on loopback can still drop under burst; require the bulk of them and
	// verify that whatever arrived was uncorrupted and unduplicated.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && got.distinct() < n {
		time.Sleep(20 * time.Millisecond)
	}
	distinct := got.distinct()
	if distinct < n*9/10 {
		t.Fatalf("received %d distinct datagrams of %d — too many lost", distinct, n)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	for payload, times := range got.seen {
		if times != 1 {
			t.Errorf("payload %q arrived %d times, want 1", payload, times)
		}
		var idx int
		if _, err := fmt.Sscanf(payload, "datagram-%05d", &idx); err != nil {
			t.Fatalf("corrupted payload %q", payload)
		}
	}
	t.Logf("%d/%d datagrams delivered through the batched path", distinct, n)
}

// TestBatchDisabledByEnv checks the escape hatch actually turns the fast path
// off — and that the transport still works when it does, since that is the
// configuration an operator falls back to when batching is suspected.
func TestBatchDisabledByEnv(t *testing.T) {
	t.Setenv("GRAVINET_NO_UDP_BATCH", "1")

	recv, got := openLoopback(t, 1)
	defer recv.Close()
	send, _ := openLoopback(t, 1)
	defer send.Close()

	if send.batchRX {
		t.Error("batchRX still set with GRAVINET_NO_UDP_BATCH=1")
	}
	for i, r := range send.rings4 {
		if r != nil {
			t.Errorf("ring %d created with batching disabled", i)
		}
	}

	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.Port()))
	if err := send.Send(dst, []byte("unbatched")); err != nil {
		t.Fatalf("send: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && got.count.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got.count.Load() == 0 {
		t.Fatal("no datagram delivered with batching disabled")
	}
}

// TestBatchEnabledByDefault is the counterpart: on this platform the rings and
// the batched reader should be live without any configuration.
func TestBatchEnabledByDefault(t *testing.T) {
	tr, _ := openLoopback(t, 2)
	defer tr.Close()
	if !tr.batchRX {
		t.Error("batched receive not enabled by default on linux")
	}
	if len(tr.rings4) == 0 {
		t.Fatal("no send rings created")
	}
	for i, r := range tr.rings4 {
		if r == nil {
			t.Errorf("socket %d has no send ring", i)
		}
	}
}

// TestCloseDrainsFlushers checks shutdown ordering: Close stops the flushers
// while the sockets are still open, so a datagram queued immediately before
// Close still reaches the wire rather than being abandoned in a ring.
func TestCloseDrainsFlushers(t *testing.T) {
	recv, got := openLoopback(t, 1)
	defer recv.Close()
	send, _ := openLoopback(t, 1)

	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.Port()))
	if err := send.Send(dst, []byte("last-gasp")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := send.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && got.count.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got.count.Load() == 0 {
		t.Fatal("datagram queued before Close never went out")
	}
	if err := send.Close(); err != nil { // idempotent
		t.Fatalf("second close: %v", err)
	}
}
