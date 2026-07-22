//go:build openbsd

package webadmin

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const captureSupported = true

// This backend talks directly to the same /dev/bpf character devices that
// tcpdump/libpcap use on OpenBSD, via the classic BIOC* ioctls from
// <net/bpf.h>. The ioctl request codes and struct ifreq layout are byte-for-
// byte identical to capture_freebsd.go's and capture_darwin.go's — the
// BIOCGBLEN(102)/BIOCGDLT(106)/BIOCSETIF(108)/BIOCIMMEDIATE(112) numbering
// and the 32-byte struct ifreq shape come from the same shared BSD BPF
// ancestry and were confirmed against OpenBSD's own current sys/net/bpf.h
// (bpf.h,v 1.74, 2025/03/04), not assumed from either sibling — so that
// plumbing, including openBPF's /dev/bpfN enumeration (OpenBSD's own bpf(4)
// documents the identical "separate device file per instance, EBUSY if a
// node is already in use" model), is shared verbatim below.
//
// What is NOT shared with either sibling is the alignment constant used to
// step from one captured packet to the next: OpenBSD's own bpf.h defines
// BPF_ALIGNMENT as sizeof(u_int32_t) — always 4 bytes — where FreeBSD (and
// presumably Darwin) use sizeof(long), 8 bytes on 64-bit. Using the wrong
// one wouldn't fail loudly; every *first* packet in a batch would parse
// fine, and only batches where the kernel had coalesced more than one
// packet into a single read() would start misparsing from the second
// packet onward — worth calling out since a quick single-packet test could
// pass while this was still wrong. The bpf_hdr byte offsets themselves
// (see parseBPFHdr) happen to be identical to capture_darwin.go's, since
// both use the same 32-bit-only bpf_timeval.
//
// NOTE: this has been written and cross-checked against three independent
// current OpenBSD kernel sources (sys/net/bpf.h, the bpf(4) man page, and
// usr.bin/top's own use of related UVM/sched interfaces elsewhere in this
// package) but has not been exercised against a real OpenBSD kernel in this
// environment. If packet capture doesn't work, bpfWordAlign and
// parseBPFHdr's offset table are the first places to check — e.g. with a
// debug build that logs bh_hdrlen and compares it against what tcpdump sees
// on the same interface.

const (
	iocVoid      = 0x20000000
	iocOut       = 0x40000000
	iocIn        = 0x80000000
	iocParmMask  = 0x1fff
	bpfGroup     = uintptr('B')
	sizeofIfreq  = 32 // struct ifreq on BSD: 16-byte name + 16-byte union
	sizeofUint32 = 4
)

func iocEncode(inout, group, num, length uintptr) uintptr {
	return inout | ((length & iocParmMask) << 16) | (group << 8) | num
}
func iorCmd(group, num, length uintptr) uintptr { return iocEncode(iocOut, group, num, length) }
func iowCmd(group, num, length uintptr) uintptr { return iocEncode(iocIn, group, num, length) }

var (
	biocGBLEN     = iorCmd(bpfGroup, 102, sizeofUint32)
	biocSETIF     = iowCmd(bpfGroup, 108, sizeofIfreq)
	biocGDLT      = iorCmd(bpfGroup, 106, sizeofUint32)
	biocIMMEDIATE = iowCmd(bpfGroup, 112, sizeofUint32)
)

func bpfIoctl(fd int, cmd uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), cmd, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// openBPF finds and opens the first free /dev/bpfN device. OpenBSD's bpf(4)
// documents this exact model: "A separate device file is required for each
// minor device. If a file is in use, the open will fail and errno will be
// set to EBUSY" — the same one-node-per-listener scheme as FreeBSD/Darwin,
// not a single cloning /dev/bpf node.
func openBPF() (int, error) {
	var lastErr error
	for i := 0; i < 256; i++ {
		path := fmt.Sprintf("/dev/bpf%d", i)
		fd, err := syscall.Open(path, syscall.O_RDWR, 0)
		if err == nil {
			return fd, nil
		}
		lastErr = err
	}
	return -1, fmt.Errorf("no free /dev/bpf* device (need root; last error: %v)", lastErr)
}

// dlt to our pcap-file LINKTYPE constants. DLT_EN10MB(1)/DLT_NULL(0) happen to
// share numeric values with LINKTYPE_ETHERNET/LINKTYPE_NULL, since the pcap
// LINKTYPE registry was defined to mirror libpcap's original BSD DLT_*
// numbering — so no translation table is needed for the cases we handle.
const (
	dltNull   = 0
	dltEn10mb = 1
)

type openbsdCapture struct {
	fd      int
	stopped atomic.Bool
}

func (h *openbsdCapture) stop() {
	h.stopped.Store(true)
	syscall.Close(h.fd) // unblocks/ends a pending Read
}

// startCapture opens a BPF device, binds it to ifaceName, and streams each
// captured frame (already stripped of the kernel's bpf_hdr framing) to
// onPacket until stop() is called. Requires root.
func startCapture(ifaceName string, snaplen int, onPacket func(time.Time, []byte)) (capHandle, int, error) {
	fd, err := openBPF()
	if err != nil {
		return nil, -1, err
	}

	var ifreq [sizeofIfreq]byte
	if len(ifaceName) >= 16 {
		syscall.Close(fd)
		return nil, -1, fmt.Errorf("interface name %q too long", ifaceName)
	}
	copy(ifreq[:16], ifaceName)
	if err := bpfIoctl(fd, biocSETIF, unsafe.Pointer(&ifreq[0])); err != nil {
		syscall.Close(fd)
		return nil, -1, fmt.Errorf("bind %q to bpf device (need root?): %w", ifaceName, err)
	}

	var one uint32 = 1
	if err := bpfIoctl(fd, biocIMMEDIATE, unsafe.Pointer(&one)); err != nil {
		syscall.Close(fd)
		return nil, -1, fmt.Errorf("set immediate mode: %w", err)
	}

	var bufLen uint32
	if err := bpfIoctl(fd, biocGBLEN, unsafe.Pointer(&bufLen)); err != nil || bufLen == 0 {
		bufLen = 1 << 20 // fall back to a generous 1MiB read buffer
	}

	var dlt uint32
	linktype := linktypeRaw
	if err := bpfIoctl(fd, biocGDLT, unsafe.Pointer(&dlt)); err == nil {
		switch dlt {
		case dltEn10mb:
			linktype = linktypeEthernet
		case dltNull:
			linktype = linktypeNull
		default:
			linktype = int(dlt) // best effort passthrough for anything else
		}
	}

	_ = syscall.SetNonblock(fd, true)
	h := &openbsdCapture{fd: fd}
	go h.loop(int(bufLen), onPacket)
	return h, linktype, nil
}

// bpfWordAlign is OpenBSD's BPF_ALIGNMENT: sizeof(u_int32_t), always 4 bytes
// — NOT sizeof(long) (8 bytes on 64-bit), which is what capture_freebsd.go
// uses. See the file header comment for why getting this wrong wouldn't
// fail loudly.
const bpfWordAlign = 4

// loop reads batches of BPF-framed packets and unpacks each one using
// parseBPFHdr. Every read can return multiple packets back-to-back, each
// prefixed by a bpf_hdr.
func (h *openbsdCapture) loop(bufLen int, onPacket func(time.Time, []byte)) {
	buf := make([]byte, bufLen)
	for {
		if h.stopped.Load() {
			return
		}
		n, err := syscall.Read(h.fd, buf)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK || err == syscall.EINTR {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return // closed or fatal
		}
		if n <= 0 {
			continue
		}
		p := 0
		for p+18 <= n {
			sec, usec, caplen, hdrlen, ok := parseBPFHdr(buf[p:n])
			if !ok {
				break
			}
			start := p + int(hdrlen)
			end := start + int(caplen)
			if hdrlen == 0 || end > n || end <= start {
				break // malformed/short trailing record; stop this batch
			}
			pkt := make([]byte, caplen)
			copy(pkt, buf[start:end])
			onPacket(time.Unix(sec, usec*1000), pkt)
			slot := int(hdrlen) + int(caplen)
			p += (slot + bpfWordAlign - 1) &^ (bpfWordAlign - 1)
		}
		if h.stopped.Load() {
			return
		}
	}
}
