//go:build linux && (amd64 || arm64)

package tun

import "encoding/binary"

// splitGSO splits a coalesced TCPv4 super-packet into its individual
// segments, calling emit once per segment with a fully valid, independently
// checksummed IPv4/TCP packet built into scratch. scratch is reused across
// calls — emit must finish using a segment (hand it off synchronously)
// before splitGSO calls emit again, since the next segment overwrites it.
//
// Returns false, having called emit zero times, whenever h/pkt don't
// describe something worth splitting: GSO type isn't plain TCPv4 (ECN is
// fine; anything else — UDP GSO/USO, TCPv6, mrg_rxbuf — is out of scope for
// this first cut, see vnethdr_linux.go's top comment), the packet's shape
// doesn't match isPlainIPv4TCP (options present, wrong protocol, too
// short), there's no payload to split, or there's only one segment's worth
// of it anyway. Callers should treat false as "not a super-packet, handle
// pkt as the one ordinary packet it already is" — exactly what happened
// before this file existed.
func splitGSO(h vnetHdr, pkt []byte, scratch []byte, emit func([]byte)) bool {
	if h.gsoType&^gsoECNbit != gsoTCPv4 {
		return false
	}
	if !isPlainIPv4TCP(pkt) {
		return false
	}
	if h.gsoSize == 0 {
		return false
	}
	const hdrLen = ipv4HeaderLen + tcpMinHdrLen
	if len(pkt) <= hdrLen {
		return false // header only, nothing to split
	}
	payload := pkt[hdrLen:]
	nSegs := (len(payload) + int(h.gsoSize) - 1) / int(h.gsoSize)
	if nSegs <= 1 {
		return false // fits in one segment already; splitting would do nothing
	}
	need := hdrLen + int(h.gsoSize)
	if len(scratch) < need {
		return false // caller's scratch too small: bail to the safe per-packet path
	}

	origSeq := tcpSeq(pkt)
	origFlags := tcpFlags(pkt)
	origID := binary.BigEndian.Uint16(pkt[ipIDOff : ipIDOff+2])

	off := 0
	for i := 0; i < nSegs; i++ {
		segLen := int(h.gsoSize)
		if off+segLen > len(payload) {
			segLen = len(payload) - off
		}
		total := hdrLen + segLen
		seg := scratch[:total]
		copy(seg[:hdrLen], pkt[:hdrLen])
		copy(seg[hdrLen:], payload[off:off+segLen])

		binary.BigEndian.PutUint16(seg[ipTotalLenOff:ipTotalLenOff+2], uint16(total))
		// Each segment is a distinct IP datagram; give it its own ID the way
		// the kernel's own tcp_gso_segment does (originalID + segment
		// index), rather than reusing one ID across all of them.
		binary.BigEndian.PutUint16(seg[ipIDOff:ipIDOff+2], origID+uint16(i))
		binary.BigEndian.PutUint32(seg[ipv4HeaderLen+tcpSeqOff:ipv4HeaderLen+tcpSeqOff+4], origSeq+uint32(off))

		// FIN/PSH belong to the original send's final byte only; every
		// segment but the last one has them cleared, matching standard TSO
		// behaviour (RFC-wise this mirrors how a real single large send
		// would have been segmented if the sender's stack had done it
		// itself instead of relying on GSO).
		flags := origFlags
		if i != nSegs-1 {
			flags &^= tcpFlagFIN | tcpFlagPSH
		}
		seg[ipv4HeaderLen+tcpFlagsOff] = flags

		seg[ipChecksumOff], seg[ipChecksumOff+1] = 0, 0
		binary.BigEndian.PutUint16(seg[ipChecksumOff:ipChecksumOff+2], ipv4Checksum(seg[:ipv4HeaderLen]))

		seg[ipv4HeaderLen+tcpChecksumOff], seg[ipv4HeaderLen+tcpChecksumOff+1] = 0, 0
		tcpck := tcpChecksum(seg[:ipv4HeaderLen], seg[ipv4HeaderLen:total])
		binary.BigEndian.PutUint16(seg[ipv4HeaderLen+tcpChecksumOff:ipv4HeaderLen+tcpChecksumOff+2], tcpck)

		emit(seg)
		off += segLen
	}
	return true
}

// ReadPackets reads one frame from the device and calls emit once per
// individual packet it represents: once, with exactly what a plain Read
// would have produced, for an ordinary packet (including any GSO type this
// cut doesn't split, which is handled as a single already-valid packet —
// virtio_net_hdr guarantees the checksum fields are meaningful either way);
// or once per segment, each independently valid, for a coalesced TCPv4
// super-packet. emit's argument is only valid until it returns; nothing may
// retain it (same contract as Read).
//
// buf sizes the underlying read exactly like Read does. scratch is where
// split segments are rebuilt (needed only when splitGSO actually has
// something to split) — Device owns and grows it, so callers don't need to
// size it themselves. Returns any read error; emit is never called on error.
func (d *Device) ReadPackets(buf []byte, emit func([]byte)) error {
	n, h, err := d.ReadSuper(buf)
	if err != nil {
		return err
	}
	pkt := buf[:n]
	if len(d.splitScratch) < len(buf) {
		d.splitScratch = make([]byte, len(buf))
	}
	if splitGSO(h, pkt, d.splitScratch, emit) {
		return nil
	}
	emit(pkt)
	return nil
}
