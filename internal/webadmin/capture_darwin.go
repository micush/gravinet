//go:build darwin

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
// tcpdump/libpcap use on macOS and other BSDs, via the classic BIOC* ioctls
// from <net/bpf.h>. Those ioctl request codes are computed the same way the C
// _IOR/_IOW/_IO macros do (see ioc/iow/ior below) rather than hardcoded, so
// the only things that have to be right are the well-established, decades-
// stable BIOC command numbers and struct sizes themselves — not a
// hand-transcribed magic constant.
//
// NOTE: this has been written to match the documented BPF ABI but has not
// been exercised against a real macOS kernel in this environment. If packet
// capture on macOS doesn't work, that's the first place to look.

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
func ioCmd(group, num uintptr) uintptr          { return iocEncode(iocVoid, group, num, 0) }

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

// openBPF finds and opens the first free /dev/bpfN device.
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

type darwinCapture struct {
	fd      int
	stopped atomic.Bool
}

func (h *darwinCapture) stop() {
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
	h := &darwinCapture{fd: fd}
	go h.loop(int(bufLen), onPacket)
	return h, linktype, nil
}

// loop reads batches of BPF-framed packets and hands each one to
// parseBPFBatch (capture_bpfbatch.go), which reads bh_tstamp/bh_caplen/
// bh_hdrlen via parseBPFHdr at their documented byte offsets:
//
//	offset 0:  bh_tstamp.tv_sec  (int32, the BSD-stable 32-bit BPF_TIMEVAL)
//	offset 4:  bh_tstamp.tv_usec (int32)
//	offset 8:  bh_caplen         (uint32)
//	offset 12: bh_datalen        (uint32)
//	offset 16: bh_hdrlen         (uint16)
//
// and advances to the next record on a 4-byte boundary: Darwin's
// BPF_ALIGNMENT is sizeof(int32_t), not sizeof(long) — see
// capture_bpfbatch.go's doc comment for how getting this specific value
// wrong on a previous version of this file (8, not 4) produced exactly the
// "every other record decodes to a plausible-but-wrong timestamp over a
// garbage payload" shape a real capture bundle turned up, rather than a
// clean failure.
func (h *darwinCapture) loop(bufLen int, onPacket func(time.Time, []byte)) {
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
		parseBPFBatch(buf, n, 4, onPacket)
		if h.stopped.Load() {
			return
		}
	}
}
