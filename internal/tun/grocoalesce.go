//go:build linux && (amd64 || arm64)

package tun

import "encoding/binary"

// Coalescer greedily merges a run of decrypted TCPv4 segments belonging to
// the same flow into one write, mirroring — in software, on the way into a
// TUN device — what a real NIC's GRO engine does on the way out of one. It
// is not safe for concurrent use: exactly one goroutine (the network's TUN
// write flusher — see the mesh package's tungro.go) may call TryAdd/Flush.
//
// Never waits. TryAdd only ever gets called with packets a producer has
// already handed the flusher; nothing here delays a packet hoping a
// contiguous follow-up will show up in time to merge with it. Coalescing
// only happens when packets already arrived together.
//
// Ack number, window, and TCP flags in the merged buffer end up being the
// most recently added segment's, not each original segment's own — this is
// the same tradeoff a real GRO engine makes (Linux's inet_gro_receive keeps
// one shared header across the merged skb), and is safe for the same
// reason: TCP acks are monotonic information, a slightly-stale ack
// propagated in place of a newer one is exactly the ordinary reordering
// tolerance TCP already has to have, never a correctness violation.
type Coalescer struct {
	buf     []byte
	pending bool
	flow    flowKey
	segSize uint16
	nextSeq uint32
	segs    int
}

// NewCoalescer returns a ready-to-use Coalescer.
func NewCoalescer() *Coalescer { return &Coalescer{} }

const gsoHdrLen = ipv4HeaderLen + tcpMinHdrLen

// TryAdd attempts to fold pkt into the run in progress. Returns true if it
// did — pkt is now owned by the Coalescer (copied in), the caller must not
// reuse pkt's backing array expecting TryAdd to have left it alone, but by
// the same token the caller is done with pkt either way. Returns false when
// pkt doesn't belong in the current run: different or no flow in progress,
// non-contiguous sequence number, a SYN/RST/options/non-TCPv4 shape, no
// payload (a bare ACK), the run's already-established segment size doesn't
// fit, or merging would push the buffer past IPv4's 65535-byte ceiling. The
// caller must Flush before offering pkt again — see tungro.go's drain loop.
func (c *Coalescer) TryAdd(pkt []byte) bool {
	if !isPlainIPv4TCP(pkt) {
		return false
	}
	flags := tcpFlags(pkt)
	if flags&(tcpFlagSYN|tcpFlagRST) != 0 {
		return false // never fold connection-control segments into a data run
	}
	payload := pkt[gsoHdrLen:]
	if len(payload) == 0 {
		return false // pure ack: nothing to merge, and it has no seq range to chain onto
	}
	fk := flowOf(pkt)
	seq := tcpSeq(pkt)

	if !c.pending {
		need := gsoHdrLen + len(payload)
		if cap(c.buf) < need {
			c.buf = make([]byte, 0, need*2)
		}
		c.buf = c.buf[:need]
		copy(c.buf, pkt[:gsoHdrLen])
		copy(c.buf[gsoHdrLen:], payload)
		c.pending = true
		c.flow = fk
		c.segSize = uint16(len(payload))
		c.nextSeq = seq + uint32(len(payload))
		c.segs = 1
		return true
	}

	if fk != c.flow || seq != c.nextSeq {
		return false
	}
	if uint16(len(payload)) > c.segSize {
		return false // would grow the run's established segment size — never done mid-run
	}
	// A segment shorter than segSize can only be the last one in a valid
	// GSO run. If the buffer's current payload total isn't an exact
	// multiple of segSize, the previous add was already that short
	// terminal segment, so nothing may follow it.
	curPayload := len(c.buf) - gsoHdrLen
	if curPayload%int(c.segSize) != 0 {
		return false
	}
	need := len(c.buf) + len(payload)
	if need > 65535 {
		return false // IPv4 total-length field ceiling
	}
	if cap(c.buf) < need {
		nb := make([]byte, len(c.buf), need*2)
		copy(nb, c.buf)
		c.buf = nb
	}
	c.buf = c.buf[:need]
	copy(c.buf[need-len(payload):], payload)
	c.nextSeq = seq + uint32(len(payload))
	c.segs++

	copy(c.buf[ipv4HeaderLen+tcpAckOff:ipv4HeaderLen+tcpAckOff+4], pkt[ipv4HeaderLen+tcpAckOff:ipv4HeaderLen+tcpAckOff+4])
	copy(c.buf[ipv4HeaderLen+tcpWindowOff:ipv4HeaderLen+tcpWindowOff+2], pkt[ipv4HeaderLen+tcpWindowOff:ipv4HeaderLen+tcpWindowOff+2])
	c.buf[ipv4HeaderLen+tcpFlagsOff] = flags
	return true
}

// Flush finalises whatever run is pending and always hands it back — it
// never silently drops a packet. segs tells the caller what buf actually is:
//
//   - 0: nothing was pending. buf is nil; there is nothing to write.
//   - 1: exactly one segment was ever added and nothing merged with it. buf
//     is that segment, byte-for-byte identical to the original packet (see
//     TryAdd: the first add on an empty run only copies, it never rewrites
//     any field), so the caller should hand it to the device's plain Write,
//     not WriteCoalesced — there is nothing here to tag as GSO.
//   - 2+: buf is a genuinely coalesced super-packet with corrected IP total
//     length and IP/TCP checksums, ready for WriteCoalesced(buf, segSize).
//
// buf is valid until the next TryAdd or Flush call — copy it if the caller
// needs it to outlive that (WriteCoalesced/Write both consume it
// synchronously, so the normal drain-loop caller never needs to).
func (c *Coalescer) Flush() (buf []byte, segSize uint16, segs int) {
	if !c.pending {
		return nil, 0, 0
	}
	segs = c.segs
	if segs >= 2 {
		total := len(c.buf)
		binary.BigEndian.PutUint16(c.buf[ipTotalLenOff:ipTotalLenOff+2], uint16(total))
		c.buf[ipChecksumOff], c.buf[ipChecksumOff+1] = 0, 0
		binary.BigEndian.PutUint16(c.buf[ipChecksumOff:ipChecksumOff+2], ipv4Checksum(c.buf[:ipv4HeaderLen]))

		c.buf[ipv4HeaderLen+tcpChecksumOff], c.buf[ipv4HeaderLen+tcpChecksumOff+1] = 0, 0
		tcpck := tcpChecksum(c.buf[:ipv4HeaderLen], c.buf[ipv4HeaderLen:total])
		binary.BigEndian.PutUint16(c.buf[ipv4HeaderLen+tcpChecksumOff:ipv4HeaderLen+tcpChecksumOff+2], tcpck)
	}
	buf, segSize = c.buf, c.segSize
	c.pending, c.segs = false, 0
	return buf, segSize, segs
}

// WriteCoalesced submits merged (as produced by Flush) to the device as one
// GSO-tagged write, so the kernel knows how to re-segment it if this packet
// is forwarded onward through an interface with a real, smaller MTU.
func (d *Device) WriteCoalesced(merged []byte, segSize uint16) (int, error) {
	return d.WriteSuper(vnetHdr{gsoType: gsoTCPv4, hdrLen: gsoHdrLen, gsoSize: segSize}, merged)
}

// ---- single-device coalescing glue ----
//
// CoalesceWrite/FlushCoalesced exist so a caller outside this package (the
// mesh engine's inbound write path — see mesh/tungso.go) can drive GRO
// coalescing through the Device's own exported Device/gsoDevice-shaped
// methods, without importing Coalescer directly. That keeps the mesh
// package's stated decoupling from internal/tun intact (see engine.go's
// NewDevice doc comment: "the engine stays decoupled from internal/tun and
// tests keep substituting a fake") — a test fake Device simply doesn't
// implement these methods, and the mesh package's optional-capability type
// assertion for them never fires, so nothing here is otherwise reachable.
//
// Not safe for concurrent use — exactly one goroutine (the netState's TUN
// write flusher) may call CoalesceWrite/FlushCoalesced on a given Device, the
// same single-consumer contract Coalescer itself documents.

// CoalesceWrite offers pkt to this device's write-side coalescer. If pkt
// doesn't fit the run already in progress, whatever was pending is flushed
// to the kernel first, then pkt starts a fresh run. A well-formed pkt is
// therefore never lost, only possibly delayed until the next
// CoalesceWrite/FlushCoalesced call — the caller must call FlushCoalesced
// once it has offered everything it currently has queued (see
// mesh/tungso.go), or an accepted-but-not-yet-written pkt sits in the
// coalescer indefinitely.
func (d *Device) CoalesceWrite(pkt []byte) error {
	if d.coalescer == nil {
		d.coalescer = NewCoalescer()
	}
	if d.coalescer.TryAdd(pkt) {
		return nil
	}
	if err := d.FlushCoalesced(); err != nil {
		return err
	}
	if d.coalescer.TryAdd(pkt) {
		return nil
	}
	// pkt itself isn't a shape the coalescer will ever run-start with (IP
	// options, non-TCP, SYN/RST, a bare ack with no payload — see TryAdd):
	// an empty coalescer accepting nothing here isn't a resource limit
	// that flushing again would fix, so just write pkt through unchanged.
	_, err := d.Write(pkt)
	return err
}

// FlushCoalesced writes out whatever CoalesceWrite has accumulated so far:
// nothing, if 0 packets are pending; a plain Write, if exactly 1 (see
// Coalescer.Flush's contract — a lone pending packet comes back
// byte-identical to what was offered); or one GSO-tagged WriteCoalesced, if
// 2 or more segments were folded together.
func (d *Device) FlushCoalesced() error {
	if d.coalescer == nil {
		return nil
	}
	buf, segSize, n := d.coalescer.Flush()
	switch n {
	case 0:
		return nil
	case 1:
		_, err := d.Write(buf)
		return err
	default:
		_, err := d.WriteCoalesced(buf, segSize)
		return err
	}
}
