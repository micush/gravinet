//go:build linux && !(amd64 || arm64)

// This file is gso_linux.go/vnethdr_linux.go/grocoalesce.go's counterpart
// for 32-bit Linux, the same split batch_linux.go/batch_other.go already
// established for Phase A: struct virtio_net_hdr's layout is verified here
// only on 64-bit (see vnethdr_linux.go's top comment), so the GSO/GRO fast
// path is scoped out on 32-bit the same way the UDP batching fast path is —
// not attempted, not guessed at.
//
// tun_linux.go is the single shared Linux TUN implementation across every
// architecture (unlike batch_linux.go, which has no 32-bit counterpart file
// at all — transport.go calls readLoopBatched, which either file provides).
// tun_linux.go's Device struct and its New/Read/Write directly reference a
// handful of GSO-path names (the vnetHdr type, cIFF_VNET_HDR, enableVnetHdr,
// ReadSuper, WriteSuper, the Coalescer type for a struct field) regardless of
// architecture, so unlike batch's split, this file cannot just omit an
// equivalent of readLoopBatched and let the base file branch around it —
// tun_linux.go has no such branch, and shouldn't grow one only to duplicate
// what a stub already does more simply. These stubs make every one of those
// names resolve to inert behaviour: vnetHdr framing never turns on
// (enableVnetHdr always errors, so New's best-effort call just leaves it
// off), Read/Write fall through to a plain, unframed read()/write() exactly
// like every pre-GSO Linux build, and Coalescer is an unused empty type that
// only exists so the Device struct's coalescer field type resolves — nothing
// on this build path ever constructs one.

package tun

import "fmt"

const cIFF_VNET_HDR = 0x4000 // matches vnethdr_linux.go's value; never actually requested here, see enableVnetHdr

// vnetHdr is never populated on this build; ReadSuper always returns the
// zero value and WriteSuper ignores whatever it's given.
type vnetHdr struct {
	flags     byte
	gsoType   byte
	hdrLen    uint16
	gsoSize   uint16
	csumStart uint16
	csumOff   uint16
}

// Coalescer exists only so tun_linux.go's Device struct has a type to name
// for its coalescer field; nothing on 32-bit Linux ever constructs or calls
// into one (CoalesceWrite/FlushCoalesced/WriteCoalesced, which would use it,
// are defined only in grocoalesce.go, gated to amd64/arm64 — a 32-bit
// *tun.Device simply never satisfies the mesh package's gsoDevice interface,
// the same way every non-Linux platform doesn't).
type Coalescer struct{}

// enableVnetHdr always fails: New's caller treats that as "framing didn't
// negotiate," leaving Read/Write in their plain, unframed shape — this is
// deliberately an error return, not a silent success that then does
// nothing, so a future change to New's best-effort handling can't
// accidentally start assuming vnetHdr is true here.
func (d *Device) enableVnetHdr() error {
	return fmt.Errorf("vnet_hdr framing not implemented on this architecture")
}

// ReadSuper on this build is exactly the pre-GSO Read: no header to strip,
// the zero-value vnetHdr accurately says so.
func (d *Device) ReadSuper(p []byte) (int, vnetHdr, error) {
	n, err := d.f.Read(p)
	return n, vnetHdr{}, err
}

// WriteSuper on this build is exactly the pre-GSO Write: h is always the
// zero value here (see WriteCoalesced's absence on this build), so there is
// nothing in it worth acting on.
func (d *Device) WriteSuper(h vnetHdr, p []byte) (int, error) {
	return d.f.Write(p)
}
