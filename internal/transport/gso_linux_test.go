//go:build linux && (amd64 || arm64)

package transport

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TestSegmentCmsgLayout pins putSegmentCmsg's hand-built bytes to the
// stdlib's own cmsg arithmetic — same reasoning as TestMmsghdrLayout: this
// buffer goes straight to a syscall, so a drift would not fail loudly.
func TestSegmentCmsgLayout(t *testing.T) {
	b := make([]byte, segmentCmsgSpace)
	got := putSegmentCmsg(b, 1200)
	// Controllen for a single (final) cmsg is CmsgLen, not CmsgSpace — the
	// trailing alignment padding is only required BETWEEN cmsgs. This exact
	// value is what TestRawGSOSendSegments hands the live kernel, so this
	// assertion is pinned to verified-working behaviour, not to convention.
	if want := syscall.CmsgLen(2); got != want {
		t.Errorf("putSegmentCmsg returned %d, want CmsgLen(2)=%d", got, want)
	}
	if l := binary.NativeEndian.Uint64(b[0:8]); l != uint64(syscall.CmsgLen(2)) {
		t.Errorf("cmsg_len = %d, want CmsgLen(2)=%d", l, syscall.CmsgLen(2))
	}
	if lv := binary.NativeEndian.Uint32(b[8:12]); lv != solUDP {
		t.Errorf("cmsg_level = %d, want SOL_UDP=%d", lv, solUDP)
	}
	if ty := binary.NativeEndian.Uint32(b[12:16]); ty != udpSegment {
		t.Errorf("cmsg_type = %d, want UDP_SEGMENT=%d", ty, udpSegment)
	}
	if s := binary.NativeEndian.Uint16(b[16:18]); s != 1200 {
		t.Errorf("stride = %d, want 1200", s)
	}
}

// buildGROCmsg builds a control buffer the way the kernel's udp_cmsg_recv
// does (4-byte int payload), for parseGROCmsg to consume.
func buildGROCmsg(gsoSize int32) []byte {
	b := make([]byte, syscall.CmsgSpace(4))
	binary.NativeEndian.PutUint64(b[0:8], uint64(syscall.CmsgLen(4)))
	binary.NativeEndian.PutUint32(b[8:12], solUDP)
	binary.NativeEndian.PutUint32(b[12:16], udpGRO)
	binary.NativeEndian.PutUint32(b[16:20], uint32(gsoSize))
	return b
}

func TestParseGROCmsg(t *testing.T) {
	if got := parseGROCmsg(buildGROCmsg(1372)); got != 1372 {
		t.Errorf("well-formed GRO cmsg: got %d, want 1372", got)
	}
	if got := parseGROCmsg(nil); got != 0 {
		t.Errorf("empty control: got %d, want 0", got)
	}
	// A different cmsg (wrong type) followed by nothing: no GRO.
	other := buildGROCmsg(999)
	binary.NativeEndian.PutUint32(other[12:16], 42) // not UDP_GRO
	if got := parseGROCmsg(other); got != 0 {
		t.Errorf("non-GRO cmsg: got %d, want 0", got)
	}
	// Malformed length larger than the buffer must end the walk, not slice
	// out of bounds.
	bad := buildGROCmsg(1372)
	binary.NativeEndian.PutUint64(bad[0:8], 1<<20)
	if got := parseGROCmsg(bad); got != 0 {
		t.Errorf("oversized cmsg_len: got %d, want 0", got)
	}
	// GRO cmsg sitting second, after an unrelated first one, must be found.
	first := make([]byte, syscall.CmsgSpace(4))
	binary.NativeEndian.PutUint64(first[0:8], uint64(syscall.CmsgLen(4)))
	binary.NativeEndian.PutUint32(first[8:12], syscall.SOL_SOCKET)
	binary.NativeEndian.PutUint32(first[12:16], 99)
	two := append(first, buildGROCmsg(1444)...)
	if got := parseGROCmsg(two); got != 1444 {
		t.Errorf("second-position GRO cmsg: got %d, want 1444", got)
	}
}

func TestGSORunLen(t *testing.T) {
	a := netip.MustParseAddrPort("10.0.0.1:1000")
	b := netip.MustParseAddrPort("10.0.0.2:1000")
	mk := func(specs ...struct {
		addr netip.AddrPort
		n    int
	}) []sendSlot {
		slots := make([]sendSlot, 8) // power of two for mask
		for i, s := range specs {
			slots[i] = sendSlot{addr: s.addr, n: s.n, buf: make([]byte, s.n)}
		}
		return slots
	}
	type spec = struct {
		addr netip.AddrPort
		n    int
	}
	cases := []struct {
		name  string
		slots []sendSlot
		avail uint64
		want  uint64
	}{
		{"uniform run", mk(spec{a, 1200}, spec{a, 1200}, spec{a, 1200}), 3, 3},
		{"destination change breaks run", mk(spec{a, 1200}, spec{b, 1200}, spec{a, 1200}), 3, 1},
		{"short tail included and ends run", mk(spec{a, 1200}, spec{a, 1200}, spec{a, 700}, spec{a, 1200}), 4, 3},
		{"larger next breaks run", mk(spec{a, 1200}, spec{a, 1300}), 2, 1},
		{"single slot", mk(spec{a, 1200}), 1, 1},
		{"short first then full breaks after tail rule", mk(spec{a, 700}, spec{a, 1200}), 2, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gsoRunLen(c.slots, 7, 0, c.avail); got != c.want {
				t.Errorf("gsoRunLen = %d, want %d", got, c.want)
			}
		})
	}
	// The segment-count cap: 70 identical slots must clamp at maxGSOSegs.
	big := make([]sendSlot, 128)
	for i := range big {
		big[i] = sendSlot{addr: a, n: 100, buf: make([]byte, 100)}
	}
	if got := gsoRunLen(big, 127, 0, 70); got != maxGSOSegs {
		t.Errorf("segment cap: gsoRunLen = %d, want %d", got, maxGSOSegs)
	}
	// The byte cap: 9000-byte slots must stop before maxGSOBytes.
	jumbo := make([]sendSlot, 16)
	for i := range jumbo {
		jumbo[i] = sendSlot{addr: a, n: 9000, buf: make([]byte, 9000)}
	}
	if got := gsoRunLen(jumbo, 15, 0, 10); got != maxGSOBytes/9000 {
		t.Errorf("byte cap: gsoRunLen = %d, want %d", got, maxGSOBytes/9000)
	}
}

// TestRawGSOSendSegments proves the kernel genuinely segments a UDP_SEGMENT
// send: one sendmsg carrying four distinct payloads must arrive at a PLAIN
// (no-GRO) receiver as four separate, byte-correct datagrams. This is the
// direct kernel-behaviour check the whole TX design rests on, with no
// gravinet machinery in between.
func TestRawGSOSendSegments(t *testing.T) {
	recv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer recv.Close()
	send, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer send.Close()
	src, _ := send.SyscallConn()
	if !probeUDPSegment(src) {
		t.Skip("kernel does not support UDP_SEGMENT")
	}

	const stride = 400
	var payloads [4][]byte
	for i := range payloads {
		payloads[i] = make([]byte, stride)
		if i == 3 {
			payloads[i] = payloads[i][:250] // exercise the short-tail rule
		}
		for j := range payloads[i] {
			payloads[i][j] = byte(i*31 + j)
		}
	}

	dst := recv.LocalAddr().(*net.UDPAddr)
	name := make([]byte, sockaddrLen)
	nl := putSockaddr(name, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(dst.Port)))
	iov := make([]syscall.Iovec, len(payloads))
	for i, p := range payloads {
		iov[i].Base = &p[0]
		iov[i].Len = uint64(len(p))
	}
	ctrl := make([]byte, segmentCmsgSpace)
	clen := putSegmentCmsg(ctrl, stride)
	var h syscall.Msghdr
	h.Name = &name[0]
	h.Namelen = nl
	h.Iov = &iov[0]
	h.Iovlen = uint64(len(iov))
	h.Control = &ctrl[0]
	h.Controllen = uint64(clen)

	var errno syscall.Errno
	werr := src.Write(func(fd uintptr) bool {
		_, _, e := syscall.Syscall(syscall.SYS_SENDMSG, fd, uintptr(unsafe.Pointer(&h)), 0)
		if e == syscall.EAGAIN {
			return false
		}
		errno = e
		return true
	})
	if werr != nil || errno != 0 {
		t.Fatalf("gso sendmsg failed: err=%v errno=%v", werr, errno)
	}

	recv.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2000)
	for i := 0; i < len(payloads); i++ {
		n, _, err := recv.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read %d: %v (kernel did not segment into %d datagrams?)", i, err, len(payloads))
		}
		if n != len(payloads[i]) {
			t.Fatalf("segment %d: %d bytes, want %d", i, n, len(payloads[i]))
		}
		for j := 0; j < n; j++ {
			if buf[j] != payloads[i][j] {
				t.Fatalf("segment %d corrupted at byte %d", i, j)
			}
		}
	}
	t.Logf("kernel segmented one sendmsg into %d correct datagrams (3 full + 1 short)", len(payloads))
}

// openGSOLoopback is openLoopback with GRAVINET_UDP_GSO=1 in the
// environment. t.Setenv also guards against parallel tests mutating the env.
func openGSOLoopback(t *testing.T, workers int) (*Transport, *received) {
	t.Helper()
	t.Setenv("GRAVINET_UDP_GSO", "1")
	return openLoopback(t, workers)
}

// TestGSOEnvDefaultOff pins the contract this whole file is gated behind:
// without GRAVINET_UDP_GSO=1, neither TX GSO nor RX GRO engages, leaving the
// exact v571 path.
func TestGSOEnvDefaultOff(t *testing.T) {
	if !batchAvailable {
		t.Skip("batched path not compiled in")
	}
	t.Setenv("GRAVINET_UDP_GSO", "")
	tr, _ := openLoopback(t, 1)
	defer tr.Close()
	if tr.gsoTX || tr.batchGRO {
		t.Fatalf("gsoTX=%v batchGRO=%v with flag unset; both must be false", tr.gsoTX, tr.batchGRO)
	}
}

// TestGSORoundTripBulk is TestBatchedRoundTripBulk's Phase B sibling: bulk
// EQUAL-SIZE distinct payloads — the exact shape that forms UDP_SEGMENT runs
// on send and GRO coalescing on receive — pushed through two real transports
// with the flag on. Every payload must arrive intact, unduplicated, and
// correctly re-split; a slicing error on either side (wrong stride, off-by-one
// at a boundary, a short tail mis-handled) shows up here as a corrupted or
// missing payload.
func TestGSORoundTripBulk(t *testing.T) {
	if !batchAvailable {
		t.Skip("batched path not compiled in")
	}
	recv, got := openGSOLoopback(t, 2)
	defer recv.Close()
	send, _ := openGSOLoopback(t, 1)
	defer send.Close()
	if !send.gsoTX {
		t.Skip("kernel does not support UDP_SEGMENT")
	}
	if !recv.batchGRO {
		t.Log("note: kernel accepted UDP_SEGMENT but not UDP_GRO; receive side runs un-coalesced (still a valid configuration)")
	}

	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.Port()))
	const n = 3000
	const size = 1200 // one fixed size => maximal GSO runs
	buf := make([]byte, size)
	for i := 0; i < n; i++ {
		// Distinct header + position-dependent fill: a segment sliced at the
		// wrong offset produces bytes that fail the fill check below, not a
		// silently plausible payload.
		for j := range buf {
			buf[j] = byte(i + j)
		}
		binary.BigEndian.PutUint32(buf[0:4], uint32(i))
		if err := send.Send(dst, buf); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && got.distinct() < n {
		time.Sleep(20 * time.Millisecond)
	}
	distinct := got.distinct()
	if distinct < n*9/10 {
		t.Fatalf("received %d distinct payloads of %d — too many lost", distinct, n)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	for payload, times := range got.seen {
		if times != 1 {
			t.Errorf("a payload arrived %d times, want 1", times)
		}
		if len(payload) != size {
			t.Fatalf("payload of %d bytes, want %d — a mis-sliced segment", len(payload), size)
		}
		idx := int(binary.BigEndian.Uint32([]byte(payload[0:4])))
		for j := 4; j < size; j++ {
			if payload[j] != byte(idx+j) {
				t.Fatalf("payload %d corrupted at byte %d — cross-segment bleed", idx, j)
			}
		}
	}
	t.Logf("%d/%d equal-size payloads delivered intact through gso tx=%v / gro rx=%v", distinct, n, send.gsoTX, recv.batchGRO)
}

// TestGSOMixedTrafficRoundTrip interleaves the shapes real mesh traffic has —
// two destinations, alternating sizes, occasional jumbo — so run detection's
// boundaries (destination change, size growth, short tails) are all crossed
// repeatedly, with the plain sendmmsg path carrying everything that doesn't
// qualify. Everything must still arrive exactly once, uncorrupted.
func TestGSOMixedTrafficRoundTrip(t *testing.T) {
	if !batchAvailable {
		t.Skip("batched path not compiled in")
	}
	recvA, gotA := openGSOLoopback(t, 1)
	defer recvA.Close()
	recvB, gotB := openGSOLoopback(t, 1)
	defer recvB.Close()
	send, _ := openGSOLoopback(t, 1)
	defer send.Close()
	if !send.gsoTX {
		t.Skip("kernel does not support UDP_SEGMENT")
	}

	lo := netip.MustParseAddr("127.0.0.1")
	dstA := netip.AddrPortFrom(lo, uint16(recvA.Port()))
	dstB := netip.AddrPortFrom(lo, uint16(recvB.Port()))
	sizes := []int{1200, 1200, 1200, 300, 1200, 9000, 60, 1200}
	const rounds = 250
	sent := 0
	for r := 0; r < rounds; r++ {
		for si, size := range sizes {
			dst := dstA
			if (r+si)%3 == 0 {
				dst = dstB
			}
			p := make([]byte, size)
			for j := range p {
				p[j] = byte(sent + j)
			}
			binary.BigEndian.PutUint32(p[0:4], uint32(sent))
			if err := send.Send(dst, p); err != nil {
				t.Fatalf("send %d: %v", sent, err)
			}
			sent++
		}
	}

	total := func() int { return gotA.distinct() + gotB.distinct() }
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && total() < sent {
		time.Sleep(20 * time.Millisecond)
	}
	if got := total(); got < sent*9/10 {
		t.Fatalf("received %d distinct payloads of %d", got, sent)
	}
	check := func(r *received) {
		r.mu.Lock()
		defer r.mu.Unlock()
		for payload, times := range r.seen {
			if times != 1 {
				t.Errorf("a payload arrived %d times, want 1", times)
			}
			idx := int(binary.BigEndian.Uint32([]byte(payload[0:4])))
			for j := 4; j < len(payload); j++ {
				if payload[j] != byte(idx+j) {
					t.Fatalf("payload %d corrupted at byte %d", idx, j)
				}
			}
		}
	}
	check(gotA)
	check(gotB)
	t.Logf("%d/%d mixed-shape payloads delivered intact across two destinations", total(), sent)
}

// TestGSOConcurrentSenders drives the ring from many producer goroutines at
// once with run-eligible traffic, under whatever scheduler pressure the race
// detector adds — the closest this environment gets to the concurrency
// pattern that broke Phase C. Payload integrity is the assertion; the race
// detector is the second assertion.
func TestGSOConcurrentSenders(t *testing.T) {
	if !batchAvailable {
		t.Skip("batched path not compiled in")
	}
	recv, got := openGSOLoopback(t, 2)
	defer recv.Close()
	send, _ := openGSOLoopback(t, 1)
	defer send.Close()
	if !send.gsoTX {
		t.Skip("kernel does not support UDP_SEGMENT")
	}
	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.Port()))

	const producers = 8
	const each = 250
	const size = 1000
	var wg sync.WaitGroup
	wg.Add(producers)
	for g := 0; g < producers; g++ {
		go func(g int) {
			defer wg.Done()
			buf := make([]byte, size)
			for i := 0; i < each; i++ {
				id := g*each + i
				for j := range buf {
					buf[j] = byte(id + j)
				}
				binary.BigEndian.PutUint32(buf[0:4], uint32(id))
				if err := send.Send(dst, buf); err != nil {
					t.Errorf("send %d: %v", id, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	const n = producers * each
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && got.count.Load() < uint64(n) {
		time.Sleep(20 * time.Millisecond)
	}
	if d := got.distinct(); d < n*9/10 {
		t.Fatalf("received %d distinct payloads of %d", d, n)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	for payload, times := range got.seen {
		if times != 1 {
			t.Errorf("a payload arrived %d times, want 1", times)
		}
		idx := int(binary.BigEndian.Uint32([]byte(payload[0:4])))
		for j := 4; j < len(payload); j++ {
			if payload[j] != byte(idx+j) {
				t.Fatalf("payload %d corrupted at byte %d", idx, j)
			}
		}
	}
	// len(got.seen) directly, NOT got.distinct(): got.mu is held here, and
	// distinct() locks it again — sync.Mutex is not reentrant, so that is a
	// self-deadlock. An earlier version of this test did exactly that and
	// wedged reproducibly; the resulting investigation (guard-zone forensics
	// on the received struct, mutex word inspection at quiescence, GSO-on/off
	// and structure bisections) conclusively cleared the transport data path
	// — delivery was 2000/2000 with pristine memory every single run — before
	// the recursive lock in the test's own final log line was spotted. The
	// pre-existing TestBatchedRoundTripBulk avoids this by logging a value
	// computed before taking the lock; this test now does the equivalent.
	t.Logf("%d/%d payloads from %d concurrent producers delivered intact", len(got.seen), n, producers)
}
