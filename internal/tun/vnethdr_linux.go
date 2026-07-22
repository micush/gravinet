//go:build linux && (amd64 || arm64)

// This file is gravinet's TUN-side batching fast path (the project's Phase C,
// named and deferred in v556's changelog entry — see docs/changelog.md).
// Phase A (internal/transport/batch_linux.go) amortised the UDP socket's
// syscall-per-datagram cost with recvmmsg/sendmmsg. That doesn't transfer to
// the TUN character device: a single read()/write() on /dev/net/tun always
// carries exactly one packet, and there is no recvmmsg/sendmmsg equivalent
// for it. The one real batching mechanism the kernel exposes here is
// virtio_net_hdr-framed GSO/TSO: with IFF_VNET_HDR negotiated, a read() can
// return one coalesced super-packet standing in for several same-flow TCP
// segments, and a write() can submit one to be re-segmented downstream —
// each replacing what would otherwise be several syscalls with one, but only
// for genuinely coalescible traffic (same TCP flow, contiguous data), unlike
// Phase A's syscall batching which works for any independent datagrams.
//
// That data-dependence is exactly why this is riskier than Phase A: Phase A
// only ever changed how many syscalls carried the same bytes. This changes
// the bytes — splitting a super-packet into individually valid segments, or
// merging several segments into one, both requiring correct IPv4/TCP
// checksum and sequence-number arithmetic (see gsomath_linux.go). A mistake
// here corrupts tunneled traffic, not just throughput, and unlike Phase A
// this repo's sandbox cannot originate real NIC-driven GSO traffic to
// characterize a field profile against. v572 shipped this off by default for
// exactly that reason — the same standard v556 applied when Phase B (UDP
// GSO/GRO) was cut outright. v573 defaults it on instead, at the operator's
// explicit request and with that verification gap already known rather than
// closed; see EnableGSO and mesh/tungso.go's tunGSORequested for the current
// gate (GRAVINET_TUN_GSO=0 to fall back to the original per-packet path).
//
// Scope of this first cut: IPv4 TCP only, no IP or TCP options (see
// isPlainIPv4TCP in gsomath_linux.go). IPv6, UDP GSO/USO, and merged-buffer
// (mrg_rxbuf / virtio_net_hdr_v1) mode are all out of scope — anything that
// doesn't match the narrow shape gsomath_linux.go checks for takes the
// existing, unchanged, per-packet Read/Write path. That path is exercised by
// every non-Linux platform and 32-bit Linux already (see tun_other.go's
// counterpart of this file, which doesn't exist — there is no GSO fast path
// there at all, matching batch_linux.go/batch_other.go's split), so a
// four-and-a-half-year-old ARM board and a fresh dual-core Linux box behave
// identically for anything this file doesn't explicitly opt into.

package tun

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

// ioctlVal is ioctl's counterpart for TUNSET* commands the kernel reads as a
// plain integer argument rather than a pointer to a buffer — see the comment
// on EnableGSO's TUNSETOFFLOAD call for why the two are not interchangeable.
func ioctlVal(fd uintptr, req uintptr, val uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, val); errno != 0 {
		return errno
	}
	return nil
}

const (
	cIFF_VNET_HDR    = 0x4000
	cTUNSETOFFLOAD   = 0x400454d0
	cTUNSETVNETHDRSZ = 0x400454d8

	tunFCsum   = 0x01
	tunFTSO4   = 0x02
	tunFTSO6   = 0x04
	tunFTSOECN = 0x08

	// vnetHdrLen is sizeof(struct virtio_net_hdr) — the 10-byte "legacy"
	// layout (flags, gso_type, hdr_len, gso_size, csum_start, csum_offset),
	// not virtio_net_hdr_v1's 12-byte mrg_rxbuf variant. TUNSETVNETHDRSZ
	// pins the fd to this size explicitly rather than trusting the kernel
	// default, so a future kernel changing that default can't silently
	// desync every read/write's framing.
	vnetHdrLen = 10

	gsoNone   = 0
	gsoTCPv4  = 1
	gsoTCPv6  = 4
	gsoECNbit = 0x80

	vnetHdrFlagNeedsCSUM = 0x01
)

// vnetHdr mirrors struct virtio_net_hdr. Fields are native-endian: this repo
// never calls TUNSETVNETBE, and the fast path is scoped to amd64/arm64 (see
// the build tag), both little-endian, so binary.NativeEndian is exactly
// binary.LittleEndian here — same reasoning batch_linux.go uses for sockaddr
// fields.
type vnetHdr struct {
	flags     byte
	gsoType   byte
	hdrLen    uint16
	gsoSize   uint16
	csumStart uint16
	csumOff   uint16
}

func (h vnetHdr) put(b []byte) {
	b[0] = h.flags
	b[1] = h.gsoType
	binary.NativeEndian.PutUint16(b[2:4], h.hdrLen)
	binary.NativeEndian.PutUint16(b[4:6], h.gsoSize)
	binary.NativeEndian.PutUint16(b[6:8], h.csumStart)
	binary.NativeEndian.PutUint16(b[8:10], h.csumOff)
}

func getVnetHdr(b []byte) vnetHdr {
	return vnetHdr{
		flags:     b[0],
		gsoType:   b[1],
		hdrLen:    binary.NativeEndian.Uint16(b[2:4]),
		gsoSize:   binary.NativeEndian.Uint16(b[4:6]),
		csumStart: binary.NativeEndian.Uint16(b[6:8]),
		csumOff:   binary.NativeEndian.Uint16(b[8:10]),
	}
}

// enableVnetHdr is called from New, after TUNSETIFF has already succeeded
// with cIFF_VNET_HDR requested. It pins the header size and reports whether
// the fd is actually framed — TUNSETIFF accepting the flag and the kernel
// build genuinely supporting a stable 10-byte header are two different
// things on old kernels, so this is checked, not assumed.
func (d *Device) enableVnetHdr() error {
	sz := int32(vnetHdrLen)
	if err := ioctl(d.f.Fd(), cTUNSETVNETHDRSZ, unsafe.Pointer(&sz)); err != nil {
		return fmt.Errorf("TUNSETVNETHDRSZ: %w", err)
	}
	d.vnetHdr = true
	return nil
}

// EnableGSO negotiates TSO4/TSO6/checksum offload on this fd. It is not
// called from New: see this file's top comment for the current default and
// how to override it. Callers gate this behind GOMAXPROCS>=2 (matching
// initBatch's reasoning — a coalesce-then-write pipeline needs a second core
// to pay for itself the same way the flusher does) and mesh/tungso.go's
// tunGSORequested.
//
// Returns an error if IFF_VNET_HDR wasn't negotiated at open time (nothing
// to layer offload on top of) or if TUNSETOFFLOAD itself fails (old kernel,
// or a tun implementation — e.g. inside some containers — that rejects it).
// Both are meant to be handled by falling back to the plain Read/Write path,
// exactly like batch_linux.go's initBatch falling back to per-packet I/O.
func (d *Device) EnableGSO() error {
	if !d.vnetHdr {
		return fmt.Errorf("EnableGSO: IFF_VNET_HDR not negotiated on this fd")
	}
	// TUNSETOFFLOAD is one of the TUNSET* commands the kernel reads as a
	// plain integer ioctl argument (tun_set_offload(tun, arg) uses arg
	// directly), not as a pointer to a buffer the way TUNSETVNETHDRSZ above
	// does. Passing &flags here — the natural-looking thing to do, matching
	// every other ioctl in this file — sends the kernel a pointer value
	// instead of the flag bits themselves, and every one of gravinet's
	// requested bits would then read back unset against whatever garbage
	// bit pattern that pointer happens to have, tripping the driver's
	// "unrecognised bit" check and failing with EINVAL. Confirmed against a
	// real kernel: the pointer form fails every time, the raw-value form
	// below succeeds.
	flags := uintptr(tunFCsum | tunFTSO4 | tunFTSO6 | tunFTSOECN)
	if err := ioctlVal(d.f.Fd(), cTUNSETOFFLOAD, flags); err != nil {
		return fmt.Errorf("TUNSETOFFLOAD: %w", err)
	}
	d.gso = true
	return nil
}

// GSOEnabled reports whether EnableGSO succeeded on this device.
func (d *Device) GSOEnabled() bool { return d.gso }

// ReadSuper reads one frame from the TUN device without stripping GSO
// framing: p receives the packet (which may be a coalesced multi-segment
// super-packet whenever GSOEnabled and the kernel had one to deliver), and
// the returned vnetHdr describes it. Callers that don't want to think about
// super-packets should call the plain Read instead, which always hands back
// a single, already-normal-sized packet — splitting super-packets itself
// internally is deliberately not done there (see gsosplit.go, called
// explicitly by the mesh layer's tunLoop so the split packets can be fed
// straight into the existing per-packet pipeline instead of being
// recombined into one buffer and re-split immediately after).
//
// Only meaningful when d.vnetHdr; callers must check that (or just call
// GSOEnabled, since GSO can't be on without it) before relying on the header.
func (d *Device) ReadSuper(p []byte) (int, vnetHdr, error) {
	if !d.vnetHdr {
		n, err := d.f.Read(p)
		return n, vnetHdr{}, err
	}
	if len(d.rxScratch) < vnetHdrLen+len(p) {
		d.rxScratch = make([]byte, vnetHdrLen+len(p))
	}
	n, err := d.f.Read(d.rxScratch)
	if err != nil {
		return 0, vnetHdr{}, err
	}
	if n < vnetHdrLen {
		return 0, vnetHdr{}, fmt.Errorf("tun read: %d bytes, shorter than the %d-byte vnet header", n, vnetHdrLen)
	}
	h := getVnetHdr(d.rxScratch[:vnetHdrLen])
	payload := n - vnetHdrLen
	if payload > len(p) {
		return 0, vnetHdr{}, fmt.Errorf("tun read: %d-byte payload does not fit the %d-byte buffer", payload, len(p))
	}
	copy(p[:payload], d.rxScratch[vnetHdrLen:n])
	return payload, h, nil
}

// WriteSuper writes p with an explicit vnetHdr. Used both for the ordinary
// case (h is the zero value: gsoType none, i.e. exactly what plain Write
// sends once vnetHdr framing is on) and for a genuinely coalesced buffer
// built by grocoalesce.go.
func (d *Device) WriteSuper(h vnetHdr, p []byte) (int, error) {
	if !d.vnetHdr {
		return d.f.Write(p)
	}
	if len(d.txScratch) < vnetHdrLen+len(p) {
		d.txScratch = make([]byte, vnetHdrLen+len(p))
	}
	h.put(d.txScratch[:vnetHdrLen])
	copy(d.txScratch[vnetHdrLen:], p)
	n, err := d.f.Write(d.txScratch[:vnetHdrLen+len(p)])
	if n > vnetHdrLen {
		n -= vnetHdrLen
	} else {
		n = 0
	}
	return n, err
}
