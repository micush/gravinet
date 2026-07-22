//go:build linux && (amd64 || arm64)

// This file is gravinet's Linux UDP batching fast path: it replaces one
// sendto() per outbound datagram and one recvfrom() per inbound datagram with
// one sendmmsg()/recvmmsg() per batch. A field CPU profile put ~49% of
// hot-path CPU in a single sendto() (internal/runtime/syscall.Syscall6 under
// transport.(*Transport).Send); amortising that syscall over a batch is the
// single largest win available on the underlay.
//
// Zero new dependencies. The obvious implementation wraps golang.org/x/net's
// ipv4/ipv6 PacketConn.ReadBatch/WriteBatch, but this build has no network
// access to fetch modules and the project has an empty go.mod by design, so
// the mmsghdr array is built by hand and handed to syscall.Syscall6. The
// structs below are the kernel's, laid out to match on 64-bit Linux (verified
// by TestMmsghdrLayout).
//
// The syscalls are issued through (*net.UDPConn).SyscallConn's Read/Write
// helpers rather than against a raw duplicated fd. That matters: it keeps the
// socket registered with the Go runtime's netpoller, so a batched read parks
// the goroutine exactly like ReadFromUDPAddrPort did instead of burning a
// thread, EAGAIN retries are handled by the runtime, and Close still unblocks
// the worker.
//
// 32-bit Linux and non-Linux platforms keep the existing per-packet code
// unchanged (see batch_other.go). struct msghdr's layout differs between 32-
// and 64-bit, and this can only be verified on 64-bit here, so the fast path
// is deliberately scoped to what is testable rather than guessed at.

package transport

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"gravinet/internal/protocol"
)

// batchAvailable reports that this build has the batched path compiled in.
const batchAvailable = true

const (
	// rxBatchSize is how many datagrams one recvmmsg may return. Each slot
	// holds a full protocol.MaxUDPPayload (9472) buffer because that is what
	// the current per-packet reader accepts and jumbo-frame underlays really
	// do deliver datagrams that large; truncating to a smaller "typical MTU"
	// would silently corrupt them. That costs 64*9472 ≈ 606 KB per read
	// worker, which is small next to the 4 MB SO_RCVBUF this transport
	// already asks the kernel for on every socket.
	rxBatchSize = 64

	// txBatchSize caps how many queued datagrams one sendmmsg carries.
	txBatchSize = 64

	// txRingSize is the per-socket outbound queue depth. Must be a power of
	// two. Slot buffers grow on demand (see sendRing), so this costs the
	// ring's bookkeeping plus whatever the live traffic actually needs.
	txRingSize = 256
)

// sockaddrLen is the size of the largest sockaddr this file builds
// (sockaddr_in6). Every message reserves this much name space.
const sockaddrLen = syscall.SizeofSockaddrInet6

// mmsghdr mirrors the kernel's struct mmsghdr:
//
//	struct mmsghdr { struct msghdr msg_hdr; unsigned int msg_len; };
//
// syscall.Msghdr is already generated per-architecture by the stdlib, and Go's
// natural field alignment reproduces the C tail padding (56-byte msghdr, msg_len
// at offset 56, 64 bytes total on 64-bit), so no manual padding is needed.
// TestMmsghdrLayout asserts exactly that.
type mmsghdr struct {
	hdr syscall.Msghdr
	n   uint32
}

// ---- sockaddr marshalling ----

// putSockaddr writes ap into sa as a sockaddr_in or sockaddr_in6 and returns
// the number of bytes written. sa must be at least sockaddrLen long.
func putSockaddr(sa []byte, ap netip.AddrPort) uint32 {
	a := ap.Addr()
	if a.Is4() {
		s := sa[:syscall.SizeofSockaddrInet4]
		for i := range s {
			s[i] = 0
		}
		// sa_family is host byte order; the port is network byte order.
		binary.NativeEndian.PutUint16(s[0:2], uint16(syscall.AF_INET))
		binary.BigEndian.PutUint16(s[2:4], ap.Port())
		v4 := a.As4()
		copy(s[4:8], v4[:])
		return syscall.SizeofSockaddrInet4
	}
	s := sa[:syscall.SizeofSockaddrInet6]
	for i := range s {
		s[i] = 0
	}
	binary.NativeEndian.PutUint16(s[0:2], uint16(syscall.AF_INET6))
	binary.BigEndian.PutUint16(s[2:4], ap.Port())
	v6 := a.As16()
	copy(s[8:24], v6[:])
	// sin6_scope_id stays 0: zoned (link-local) destinations never reach the
	// batched path — Send routes them to the direct write, which resolves the
	// zone through the net package. See sendBatched.
	return syscall.SizeofSockaddrInet6
}

// addrPortFromSockaddr decodes a sockaddr the kernel filled in on receive.
// IPv4-mapped IPv6 sources are unmapped, matching the per-packet reader.
func addrPortFromSockaddr(sa []byte, n uint32) (netip.AddrPort, bool) {
	if n < 4 || len(sa) < 4 {
		return netip.AddrPort{}, false
	}
	switch binary.NativeEndian.Uint16(sa[0:2]) {
	case syscall.AF_INET:
		if n < syscall.SizeofSockaddrInet4 || len(sa) < syscall.SizeofSockaddrInet4 {
			return netip.AddrPort{}, false
		}
		var v4 [4]byte
		copy(v4[:], sa[4:8])
		return netip.AddrPortFrom(netip.AddrFrom4(v4), binary.BigEndian.Uint16(sa[2:4])), true
	case syscall.AF_INET6:
		if n < syscall.SizeofSockaddrInet6 || len(sa) < syscall.SizeofSockaddrInet6 {
			return netip.AddrPort{}, false
		}
		var v6 [16]byte
		copy(v6[:], sa[8:24])
		return netip.AddrPortFrom(netip.AddrFrom16(v6).Unmap(), binary.BigEndian.Uint16(sa[2:4])), true
	}
	return netip.AddrPort{}, false
}

// ---- receive side ----

// batchReader is one read worker's reusable recvmmsg state: a fixed mmsghdr
// array wired to fixed buffers, iovecs, and sockaddr storage, so a steady-state
// read allocates nothing.
type batchReader struct {
	msgs  []mmsghdr
	iov   []syscall.Iovec
	bufs  [][]byte
	names []byte // rxBatchSize contiguous sockaddr slots

	// gro/ctrl are Phase B state (see gso_linux.go): when gro is set, every
	// message carries a control buffer for the kernel's UDP_GRO stride cmsg
	// and the payload buffers are groBufSize (65535) rather than
	// protocol.MaxUDPPayload — a coalesced super-datagram can approach the
	// 16-bit length ceiling, and undersizing the buffer would mean silent
	// MSG_TRUNC payload loss.
	gro  bool
	ctrl []byte // n contiguous groCtrlLen control slots; nil unless gro

	// armed is how many leading entries the previous call consumed and so may
	// have had their Namelen overwritten by the kernel. Only those need
	// re-arming; re-arming all rxBatchSize entries every call would spend 64
	// iterations of bookkeeping to receive what is very often a single
	// datagram, which measurably outweighed the syscall being saved.
	armed int
}

func newBatchReader(n int, gro bool) *batchReader {
	br := &batchReader{
		msgs:  make([]mmsghdr, n),
		iov:   make([]syscall.Iovec, n),
		bufs:  make([][]byte, n),
		names: make([]byte, n*sockaddrLen),
		gro:   gro,
		armed: n, // the first call arms everything
	}
	bufSize := protocol.MaxUDPPayload
	if gro {
		bufSize = groBufSize
		br.ctrl = make([]byte, n*groCtrlLen)
	}
	for i := 0; i < n; i++ {
		b := make([]byte, bufSize)
		br.bufs[i] = b
		br.iov[i].Base = &b[0]
		br.iov[i].Len = uint64(len(b))
		h := &br.msgs[i].hdr
		h.Name = &br.names[i*sockaddrLen]
		h.Namelen = sockaddrLen
		h.Iov = &br.iov[i]
		h.Iovlen = 1
		if gro {
			h.Control = &br.ctrl[i*groCtrlLen]
			h.Controllen = groCtrlLen
		}
	}
	return br
}

// recvBatch issues one recvmmsg and returns how many datagrams it produced.
func recvBatch(rc syscall.RawConn, br *batchReader) (int, error) {
	// Namelen is both input (space available) and output (space used), so any
	// entry the kernel filled last time has to be re-armed before reuse. The
	// same in/out contract applies to Controllen and to the per-message Flags
	// on the GRO path.
	for i := 0; i < br.armed; i++ {
		br.msgs[i].hdr.Namelen = sockaddrLen
		br.msgs[i].n = 0
		if br.gro {
			br.msgs[i].hdr.Controllen = groCtrlLen
			br.msgs[i].hdr.Flags = 0
		}
	}
	var got int
	var errno syscall.Errno
	err := rc.Read(func(fd uintptr) bool {
		r, _, e := syscall.Syscall6(sysRecvmmsg, fd,
			uintptr(unsafe.Pointer(&br.msgs[0])), uintptr(len(br.msgs)), 0, 0, 0)
		if e == syscall.EAGAIN || e == syscall.EWOULDBLOCK || e == syscall.EINTR {
			return false // not ready / interrupted: let the netpoller wait and retry
		}
		got, errno = int(r), e
		return true
	})
	if err != nil {
		br.armed = 0
		return 0, err
	}
	if errno != 0 {
		br.armed = 0
		return 0, errno
	}
	if got < 0 {
		got = 0
	}
	br.armed = got
	return got, nil
}

// readLoopBatched is the batched replacement for readLoop. It falls back to the
// per-packet loop whenever batching is off or the socket won't yield a raw
// conn, so the caller's contract (one goroutine, one wg.Done) is identical
// either way.
func (t *Transport) readLoopBatched(c *net.UDPConn, fam Family) {
	rc, err := c.SyscallConn()
	if !t.batchRX || err != nil {
		if err != nil {
			t.log.Warnf("transport: batched receive unavailable (%v) — using per-packet reads", err)
		}
		t.readLoop(c, fam) // owns the wg.Done
		return
	}
	defer t.wg.Done()

	slots := rxBatchSize
	if t.batchGRO {
		// GRO buffers are 65535 bytes each (see groBufSize); fewer slots keep
		// a worker's resident footprint bounded (~1 MB at 16 slots versus
		// ~4 MB at 64), and each GRO message can already stand in for dozens
		// of coalesced datagrams, so the effective batching per syscall is
		// far higher than the non-GRO reader's despite the smaller count.
		slots = rxBatchSizeGRO
	}
	br := newBatchReader(slots, t.batchGRO)
	for {
		n, err := recvBatch(rc, br)
		if err != nil {
			if t.closed.Load() || errors.Is(err, net.ErrClosed) {
				return // socket closed: this worker is done
			}
			t.log.Debugf("transport: batched read error: %v", err)
			continue // transient; keep serving
		}
		for i := 0; i < n; i++ {
			m := &br.msgs[i]
			from, ok := addrPortFromSockaddr(br.names[i*sockaddrLen:(i+1)*sockaddrLen], m.hdr.Namelen)
			if !ok {
				continue // unrecognised source family; drop like a malformed datagram
			}
			// Same buffer contract as readLoop everywhere below: the handler
			// must not retain the payload. The per-worker buffers are reused
			// directly on the next recvmmsg, which is why this path needs no
			// sync.Pool at all.
			if br.gro {
				h := &m.hdr
				if h.Flags&syscall.MSG_TRUNC != 0 {
					continue // payload didn't fit the buffer: cannot be trusted, drop
				}
				gso := 0
				if h.Flags&syscall.MSG_CTRUNC == 0 && h.Controllen > 0 {
					used := int(h.Controllen)
					if used > groCtrlLen {
						used = groCtrlLen
					}
					gso = parseGROCmsg(br.ctrl[i*groCtrlLen : i*groCtrlLen+used])
				}
				if gso > 0 && int(m.n) > gso {
					// One coalesced super-datagram standing in for several
					// same-flow datagrams: slice it back apart at the stride
					// the kernel reported. Every segment is stride bytes
					// except possibly the last — the kernel's own coalescing
					// invariant, mirrored by gsoRunLen on the send side.
					buf := br.bufs[i][:m.n]
					for off := 0; off < len(buf); off += gso {
						end := off + gso
						if end > len(buf) {
							end = len(buf)
						}
						t.rxPackets.Add(1)
						t.handler(buf[off:end], from, fam)
					}
					continue
				}
			}
			t.rxPackets.Add(1)
			t.handler(br.bufs[i][:m.n], from, fam)
		}
	}
}

// ---- send side ----

// flusher owns one socket's outbound ring and turns queued datagrams into
// sendmmsg calls. Exactly one runs per outbound socket, so it is the only
// consumer of its ring.
type flusher struct {
	t    *Transport
	c    *net.UDPConn
	rc   syscall.RawConn
	ring *sendRing

	msgs  []mmsghdr
	iov   []syscall.Iovec
	names []byte

	// gsoTX/ctrl are Phase B (see gso_linux.go): when gsoTX is set, drain
	// routes through transmitRuns, which collapses eligible same-destination
	// equal-size runs into single UDP_SEGMENT sends. Owned exclusively by
	// this flusher's goroutine — including the self-disable on a driver
	// error in sendGSORun — so no synchronisation is needed.
	gsoTX bool
	ctrl  []byte
}

func newFlusher(t *Transport, c *net.UDPConn, rc syscall.RawConn, ring *sendRing) *flusher {
	f := &flusher{
		t: t, c: c, rc: rc, ring: ring,
		msgs:  make([]mmsghdr, txBatchSize),
		iov:   make([]syscall.Iovec, txBatchSize),
		names: make([]byte, txBatchSize*sockaddrLen),
	}
	if t.gsoTX {
		f.gsoTX = true
		f.ctrl = make([]byte, segmentCmsgSpace)
	}
	return f
}

// run drains the ring on every wakeup until told to stop, then makes one final
// best-effort pass so datagrams queued during shutdown still go out.
func (f *flusher) run(stop <-chan struct{}) {
	defer f.t.flushWG.Done()
	for {
		select {
		case <-f.ring.sig:
			f.drain()
		case <-stop:
			f.drain()
			return
		}
	}
}

func (f *flusher) drain() {
	for {
		start, n := f.ring.claim(txBatchSize)
		if n == 0 {
			return
		}
		if f.gsoTX {
			f.transmitRuns(start, n)
		} else {
			f.transmit(start, n)
		}
		f.ring.release(n)
	}
}

// transmitRuns is transmit's GSO-aware sibling: it walks the claimed slots in
// ring order, collapsing every UDP_SEGMENT-eligible run (>= 2 consecutive
// datagrams, same destination, equal size with at most one shorter tail —
// see gsoRunLen) into a single GSO send, and handing every stretch that
// doesn't qualify to the unmodified transmit. Processing strictly in ring
// order, flushing the pending plain stretch before each GSO run, preserves
// the ring's FIFO exactly as before.
//
// A note on why runs are common enough to matter despite the equal-size
// constraint: bulk transfer to one peer is a stream of PMTU-sized sealed
// datagrams — precisely a maximal run — while mixed control/keepalive
// traffic simply falls through to the plain path it always took.
func (f *flusher) transmitRuns(start, n uint64) {
	plainStart, plainLen := start, uint64(0)
	i := uint64(0)
	for i < n {
		r := gsoRunLen(f.ring.slots, f.ring.mask, start+i, n-i)
		if r < 2 || !f.gsoTX { // gsoTX can self-clear mid-walk on a send error
			plainLen += r
			i += r
			continue
		}
		if plainLen > 0 {
			f.transmit(plainStart, plainLen)
		}
		f.sendGSORun(start+i, r)
		i += r
		plainStart, plainLen = start+i, 0
	}
	if plainLen > 0 {
		f.transmit(plainStart, plainLen)
	}
}

// sendGSORun transmits slots [start, start+r) — a run gsoRunLen validated —
// as one sendmsg with a UDP_SEGMENT cmsg: the iovec array points straight at
// the ring slots (the kernel gathers; no copy into a contiguous buffer), and
// the stride is the first slot's size.
//
// Failure handling is the load-bearing part. sendmsg for a UDP socket is
// atomic — it either queues the entire super-datagram or returns an error
// with NOTHING on the wire — so on any error the whole run can be re-sent
// per-packet with zero duplication risk (contrast transmit's sendmmsg
// remainder logic, which must reason about partial acceptance). An errno in
// the recognised does-not-support-GSO set additionally clears f.gsoTX for
// good on this socket: kernels or drivers that reject UDP_SEGMENT do so
// consistently, and retrying it per drain would just burn a failed syscall
// per batch forever.
func (f *flusher) sendGSORun(start, r uint64) {
	first := &f.ring.slots[start&f.ring.mask]
	stride := first.n
	nl := putSockaddr(f.names[0:sockaddrLen], first.addr)
	for i := uint64(0); i < r; i++ {
		s := &f.ring.slots[(start+i)&f.ring.mask]
		f.iov[i].Base = &s.buf[0]
		f.iov[i].Len = uint64(s.n)
	}
	clen := putSegmentCmsg(f.ctrl, uint16(stride))

	var h syscall.Msghdr
	h.Name = &f.names[0]
	h.Namelen = nl
	h.Iov = &f.iov[0]
	h.Iovlen = uint64(r)
	h.Control = &f.ctrl[0]
	h.Controllen = uint64(clen)

	var errno syscall.Errno
	err := f.rc.Write(func(fd uintptr) bool {
		_, _, e := syscall.Syscall(syscall.SYS_SENDMSG, fd,
			uintptr(unsafe.Pointer(&h)), 0)
		if e == syscall.EAGAIN || e == syscall.EWOULDBLOCK || e == syscall.EINTR {
			return false // socket buffer full: park until writable, then retry
		}
		errno = e
		return true
	})
	if err == nil && errno == 0 {
		f.t.txPackets.Add(r)
		return
	}
	if f.t.closed.Load() || errors.Is(err, net.ErrClosed) {
		return // shutting down; nothing useful to retry onto
	}
	if isGSOUnsupported(errno) {
		f.gsoTX = false
		f.t.log.Warnf("transport: udp gso send failed (%v) — disabling gso on this socket, falling back to per-packet sends", errno)
	} else {
		f.t.log.Debugf("transport: udp gso send failed (%v); re-sending run per-packet", errno)
	}
	// The failed sendmsg put nothing on the wire (atomicity, above), so the
	// per-packet re-send below cannot duplicate anything. It also restores
	// per-datagram error attribution — EMSGSIZE in particular surfaces here
	// against the individual datagram, feeding the same out-of-band PMTU
	// clamp the batched path already uses.
	for i := uint64(0); i < r; i++ {
		s := &f.ring.slots[(start+i)&f.ring.mask]
		if _, werr := f.c.WriteToUDPAddrPort(s.buf[:s.n], s.addr); werr != nil {
			if isMsgSize(werr) && f.t.onMsgSize != nil {
				f.t.onMsgSize(s.addr, s.n)
			}
			f.t.log.Debugf("transport: gso-fallback send to %s failed: %v", s.addr, werr)
			continue
		}
		f.t.txPackets.Add(1)
	}
}

// isGSOUnsupported reports whether errno is one of the ways a kernel or
// driver signals it cannot perform this UDP_SEGMENT send at all (as opposed
// to a transient or per-destination failure): EINVAL and EOPNOTSUPP are the
// documented rejections, and EIO is the known buggy-driver escape hatch.
func isGSOUnsupported(errno syscall.Errno) bool {
	return errno == syscall.EINVAL || errno == syscall.EOPNOTSUPP || errno == syscall.EIO
}

// transmit sends slots [start, start+n) in one sendmmsg. There is deliberately
// no timer or artificial delay: a single queued datagram is sent immediately as
// a batch of one (equivalent to sendmsg), so latency under light load is
// unchanged and batching only kicks in when there is genuinely a queue.
func (f *flusher) transmit(start, n uint64) {
	for i := uint64(0); i < n; i++ {
		s := &f.ring.slots[(start+i)&f.ring.mask]
		off := int(i) * sockaddrLen
		nl := putSockaddr(f.names[off:off+sockaddrLen], s.addr)
		f.iov[i].Base = &s.buf[0]
		f.iov[i].Len = uint64(s.n)
		h := &f.msgs[i].hdr
		h.Name = &f.names[off]
		h.Namelen = nl
		h.Iov = &f.iov[i]
		h.Iovlen = 1
		h.Control = nil
		h.Controllen = 0
		h.Flags = 0
		f.msgs[i].n = 0
	}

	sent, err := sendBatch(f.rc, f.msgs[:n])
	if sent < 0 {
		sent = 0
	}
	if sent > 0 {
		f.t.txPackets.Add(uint64(sent))
	}
	if err == nil && uint64(sent) == n {
		return
	}
	if err != nil && (f.t.closed.Load() || errors.Is(err, net.ErrClosed)) {
		return // shutting down; nothing useful to retry onto
	}

	// sendmmsg reports one result for the whole call, not one per message: it
	// returns the number of leading messages it accepted, and the error (if
	// any) belongs to the first one it did not. Re-send only that unaccepted
	// remainder on the direct per-packet path — resending the whole batch, as
	// it is tempting to do, would duplicate every datagram the kernel already
	// put on the wire. The individual writes are what surface per-message
	// errors, EMSGSIZE above all.
	for i := uint64(sent); i < n; i++ {
		s := &f.ring.slots[(start+i)&f.ring.mask]
		if _, werr := f.c.WriteToUDPAddrPort(s.buf[:s.n], s.addr); werr != nil {
			// EMSGSIZE means the path MTU shrank below our estimate. On the
			// synchronous path the caller saw this as Send's return value and
			// clamped the peer's PMTU itself; here Send returned nil long ago,
			// so the signal is delivered out-of-band through this callback
			// instead. See Options.OnSendMsgSize.
			if isMsgSize(werr) && f.t.onMsgSize != nil {
				f.t.onMsgSize(s.addr, s.n)
			}
			f.t.log.Debugf("transport: batched send to %s failed: %v", s.addr, werr)
			continue
		}
		f.t.txPackets.Add(1)
	}
}

// isMsgSize reports whether err is EMSGSIZE ("message too long"), unwrapping
// the *net.OpError/*os.SyscallError chain the net package returns.
func isMsgSize(err error) bool { return errors.Is(err, syscall.EMSGSIZE) }

// sendBatch issues one sendmmsg, returning how many messages the kernel
// accepted. A short count with a nil error is normal and means the caller
// should deal with the remainder.
func sendBatch(rc syscall.RawConn, msgs []mmsghdr) (int, error) {
	var sent int
	var errno syscall.Errno
	err := rc.Write(func(fd uintptr) bool {
		r, _, e := syscall.Syscall6(sysSendmmsg, fd,
			uintptr(unsafe.Pointer(&msgs[0])), uintptr(len(msgs)), 0, 0, 0)
		if e == syscall.EAGAIN || e == syscall.EWOULDBLOCK || e == syscall.EINTR {
			return false // socket buffer full: park until writable, then retry
		}
		if e != 0 {
			sent, errno = 0, e
			return true
		}
		sent = int(r)
		return true
	})
	if err != nil {
		return sent, err
	}
	if errno != 0 {
		return sent, errno
	}
	return sent, nil
}

// ---- lifecycle ----

// initBatch enables the Linux fast path and starts one flusher per outbound
// socket. Called from openWith after the sockets are bound and before the read
// workers start.
func (t *Transport) initBatch() {
	if os.Getenv("GRAVINET_NO_UDP_BATCH") == "1" {
		t.log.Infof("transport: udp batching=off (GRAVINET_NO_UDP_BATCH=1)")
		return
	}
	// Batching is a throughput optimisation that only pays when datagrams
	// actually queue up. On a single-core box they do not: the sender and
	// receiver ping-pong, each recvmmsg finds one datagram and then spends an
	// extra EAGAIN probe discovering the queue is empty, and each batched send
	// forces a context switch to a flusher goroutine that has no core of its
	// own to run on. Measured on one core, that is ~20% slower on receive and
	// ~68% slower on send than the plain per-packet path (7.1us/op direct
	// versus 12.0us/op batched on BenchmarkOutboundThroughput).
	//
	// This is the same trap the TUN worker pool fell into, and the same fix it
	// settled on — see tunLoop's comment on routing single-worker setups to
	// tunLoopSerial rather than running the pooled path with N forced to 1.
	// With a second core the flusher runs concurrently with the senders feeding
	// it and real backlogs form, which is the regime the field profile that
	// motivated this work was taken in.
	if procs := runtime.GOMAXPROCS(0); procs < 2 {
		t.log.Infof("transport: udp batching=off (GOMAXPROCS=%d; batching needs a second core to pay for itself)", procs)
		return
	}
	t.batchRX = true
	t.initGSO() // must precede startFlushers: t.gsoTX seeds each flusher
	t.stopFlush = make(chan struct{})
	t.rings4 = t.startFlushers(t.conns4)
	t.rings6 = t.startFlushers(t.conns6)
	t.log.Infof("transport: udp batching=on (recvmmsg up to %d, sendmmsg up to %d, tx ring %d per socket)",
		rxBatchSize, txBatchSize, txRingSize)
}

// initGSO is Phase B's opt-in gate (see gso_linux.go's top comment for the
// design and for why this must stay opt-in until it clears a real-hardware
// bar). It only runs when Phase A batching is already on — GSO rides inside
// the flusher and GRO inside the batched reader, so without batching there is
// nothing to attach either to. Everything here is best-effort: any probe or
// setsockopt failure just leaves the corresponding direction on the exact
// v571 path, logged so the operator can see what engaged.
func (t *Transport) initGSO() {
	if os.Getenv("GRAVINET_UDP_GSO") != "1" {
		return // default: off, byte-for-byte v571 behaviour
	}
	all := make([]*net.UDPConn, 0, len(t.conns4)+len(t.conns6))
	all = append(all, t.conns4...)
	all = append(all, t.conns6...)
	if len(all) == 0 {
		return
	}
	// TX support is a kernel capability, so one probe answers for every
	// socket; per-socket degradation after that is sendGSORun's job.
	if rc, err := all[0].SyscallConn(); err == nil {
		t.gsoTX = probeUDPSegment(rc)
	}
	// RX GRO genuinely changes each socket's delivery, so it is enabled (and
	// can fail) per socket; batchGRO reflects whether the readers should be
	// built GRO-shaped, which they must be if ANY socket coalesces. Kernels
	// either support UDP_GRO on all UDP sockets or none, so in practice
	// these all succeed or all fail together.
	groOn := 0
	for _, c := range all {
		if rc, err := c.SyscallConn(); err == nil && enableUDPGRO(rc) {
			groOn++
		}
	}
	t.batchGRO = groOn == len(all)
	t.log.Infof("transport: udp gso tx=%v rx-gro=%v (GRAVINET_UDP_GSO=1; segments up to %d per send, gro slots %d x %d bytes)",
		t.gsoTX, t.batchGRO, maxGSOSegs, rxBatchSizeGRO, groBufSize)
}

// startFlushers builds a ring and flusher goroutine for each socket. A socket
// whose raw conn is unavailable simply gets a nil ring and keeps using the
// direct write path.
func (t *Transport) startFlushers(conns []*net.UDPConn) []*sendRing {
	if len(conns) == 0 {
		return nil
	}
	rings := make([]*sendRing, len(conns))
	for i, c := range conns {
		rc, err := c.SyscallConn()
		if err != nil {
			t.log.Warnf("transport: batched send unavailable on socket %d (%v) — using per-packet writes", i, err)
			continue
		}
		ring := newSendRing(txRingSize)
		rings[i] = ring
		t.flushWG.Add(1)
		go newFlusher(t, c, rc, ring).run(t.stopFlush)
	}
	return rings
}

// stopBatch shuts the flushers down and waits for them. Called by Close before
// the sockets are closed, so the final drain still has somewhere to write.
func (t *Transport) stopBatch() {
	if t.stopFlush == nil {
		return
	}
	close(t.stopFlush)
	t.flushWG.Wait()
}
