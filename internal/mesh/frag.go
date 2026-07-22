package mesh

import (
	"encoding/binary"
	"errors"
	"syscall"
	"time"

	"gravinet/internal/protocol"
)

// Application-layer fragmentation. The overlay interface runs a jumbo MTU (9216
// by default) so applications can send large packets, but the underlay path —
// especially mobile/5G — often can't carry a datagram that big and silently
// drops it (or its IP fragments). Rather than rely on IP fragmentation, we split
// an oversized overlay packet into pieces that each fit the configured underlay
// MTU once sealed, send each as its own authenticated DATA packet (inner type
// innerFrag), and reassemble on the far side before handing the packet to the
// TUN. Because every fragment is sealed by the peer's session key, only a real
// peer can produce them — there is no forged-fragment reassembly DoS.

const (
	// fragHeaderLen is the cleartext-inside-ciphertext fragment header:
	// [group_id:4][index:1][count:1] followed by the fragment's bytes.
	fragHeaderLen = 4 + 1 + 1

	// maxFragments bounds a single packet's fragment count (index/count are one
	// byte). 9216 / a ~1200-byte underlay fragment is ~8, far below this.
	maxFragments = 255

	// reasmTTL is how long an incomplete reassembly is kept before being dropped.
	reasmTTL = 2 * time.Second

	// maxReasmInFlight caps concurrent in-progress reassemblies per peer, so a
	// peer that sends only partial packets can't grow memory without bound.
	maxReasmInFlight = 64

	// fragOverheadV6 is the worst-case outer overhead we subtract when sizing a
	// fragment: IPv6 (40) + UDP (8). Using the v6 figure keeps v4 safely within
	// the cap too.
	fragOverheadV6 = 40 + 8
)

// computeMaxInnerFrag returns the largest overlay-packet slice that fits in one
// underlay datagram once sealed as a fragment. underlayMTU<=0 selects a safe
// default (1280). The result is the cap on both an unfragmented packet and each
// fragment's payload.
func computeMaxInnerFrag(underlayMTU int) int {
	if underlayMTU <= 0 {
		underlayMTU = 1280
	}
	// Sealed datagram = DataHeader + innerType(1) + body + GCM tag, inside UDP/IP.
	// For a fragment, body = fragHeaderLen + payload.
	sealedOverhead := protocol.DataHeaderLen + 1 + protocol.GCMOverhead
	max := underlayMTU - fragOverheadV6 - sealedOverhead - fragHeaderLen
	if max < 1 {
		max = 1
	}
	return max
}

// sendFragmented splits an oversized overlay packet into fragments and seals each
// one independently. If the packet somehow needs more than maxFragments pieces
// it is dropped (cannot happen with a 9216 ceiling and a sane underlay MTU).
func (e *Engine) sendFragmented(ps *peerSession, packet []byte, per int) {
	if per < 1 {
		per = 1
	}
	count := (len(packet) + per - 1) / per
	if count > maxFragments {
		e.log.Debugf("mesh: packet too large to fragment (%d bytes, %d pieces)", len(packet), count)
		ps.fragSendDrop.Add(1)
		return
	}
	id := e.fragSeq.Add(1)
	body := make([]byte, fragHeaderLen+per)
	binary.BigEndian.PutUint32(body[0:4], id)
	body[5] = byte(count)
	for i := 0; i < count; i++ {
		start := i * per
		end := start + per
		if end > len(packet) {
			end = len(packet)
		}
		chunk := packet[start:end]
		body[4] = byte(i)
		n := copy(body[fragHeaderLen:], chunk)
		if err := e.sealAndSend(ps, innerFrag, body[:fragHeaderLen+n]); err != nil && isMsgSize(err) {
			// The path MTU shrank below our per-fragment size (DF is set, so the
			// kernel refused to fragment). Drop to the floor and re-discover; the
			// remaining fragments would fail too, so stop and let TCP retransmit
			// the packet, which will then fragment small enough to get through.
			ps.fragSendDrop.Add(1)
			ps.resetPMTU()
			return
		}
		ps.fragsSent.Add(1)
	}
}

// isMsgSize reports whether err is the "message too long" error returned when a
// datagram exceeds the path MTU and the don't-fragment bit is set.
func isMsgSize(err error) bool { return errors.Is(err, syscall.EMSGSIZE) }

// fragReasm accumulates the fragments of one overlay packet.
type fragReasm struct {
	count    int
	got      int
	deadline time.Time
	parts    [][]byte // indexed by fragment index; nil until that piece arrives
	total    int      // running sum of received fragment lengths
}

// onFragment buffers one fragment and, once all pieces for its group have
// arrived, reassembles the overlay packet and delivers it to the TUN.
func (e *Engine) onFragment(ps *peerSession, body []byte) {
	if len(body) < fragHeaderLen {
		return
	}
	id := binary.BigEndian.Uint32(body[0:4])
	idx := int(body[4])
	count := int(body[5])
	chunk := body[fragHeaderLen:]
	if count == 0 || count > maxFragments || idx >= count || len(chunk) == 0 {
		return
	}

	now := time.Now()
	ps.fragsRcvd.Add(1)
	ps.reasmMu.Lock()
	if ps.reasm == nil {
		ps.reasm = make(map[uint32]*fragReasm)
	}
	// Opportunistically evict expired partials, and bound the table size.
	for k, r := range ps.reasm {
		if now.After(r.deadline) {
			delete(ps.reasm, k)
			ps.reasmDrop.Add(1)
		}
	}
	r := ps.reasm[id]
	if r == nil {
		if len(ps.reasm) >= maxReasmInFlight {
			// Drop the oldest in-flight entry to make room.
			var oldestKey uint32
			var oldest time.Time
			first := true
			for k, e := range ps.reasm {
				if first || e.deadline.Before(oldest) {
					oldestKey, oldest, first = k, e.deadline, false
				}
			}
			delete(ps.reasm, oldestKey)
			ps.reasmDrop.Add(1)
		}
		r = &fragReasm{count: count, parts: make([][]byte, count), deadline: now.Add(reasmTTL)}
		ps.reasm[id] = r
	}
	if r.count != count || idx >= len(r.parts) {
		// Inconsistent count for this group id — discard and restart.
		delete(ps.reasm, id)
		ps.reasmDrop.Add(1)
		ps.reasmMu.Unlock()
		return
	}
	if r.parts[idx] != nil {
		ps.reasmMu.Unlock()
		return // duplicate fragment
	}
	// Copy the chunk out of the shared RX buffer; it is reused after we return.
	piece := make([]byte, len(chunk))
	copy(piece, chunk)
	r.parts[idx] = piece
	r.got++
	r.total += len(piece)
	if r.got < r.count {
		ps.reasmMu.Unlock()
		return // still waiting on more pieces
	}
	// Complete: assemble and remove from the table.
	delete(ps.reasm, id)
	full := make([]byte, 0, r.total)
	for _, p := range r.parts {
		full = append(full, p...)
	}
	ps.reasmMu.Unlock()

	ps.reasmOK.Add(1)
	e.deliverInner(ps, full, r.total)
}
