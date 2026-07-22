//go:build linux && (amd64 || arm64)

// This file holds the checksum and header-field arithmetic that gsosplit.go
// and grocoalesce.go both need. It is deliberately scoped to what a TSO/GRO
// engine actually requires — IPv4 header checksum, the TCP/UDP pseudo-header
// checksum, and reading/writing the handful of header fields segmentation
// touches — not a general packet-parsing library. TCPv4 only, no IP options
// (IHL must be 5): see gsosplit.go's doc comment for why that scope was
// chosen for this first cut.
package tun

import "encoding/binary"

const (
	ipv4HeaderLen = 20 // no options; packets with options never enter this path
	tcpMinHdrLen  = 20 // no options; packets with options never enter this path
)

// ipv4 offsets (no options).
const (
	ipTotalLenOff  = 2
	ipIDOff        = 4
	ipFlagsFragOff = 6
	ipTTLOff       = 8
	ipProtoOff     = 9
	ipChecksumOff  = 10
	ipSrcOff       = 12
	ipDstOff       = 16
)

// tcp offsets, relative to the start of the TCP header (no options).
const (
	tcpSrcPortOff  = 0
	tcpDstPortOff  = 2
	tcpSeqOff      = 4
	tcpAckOff      = 8
	tcpFlagsOff    = 13
	tcpWindowOff   = 14
	tcpChecksumOff = 16
)

// TCP flag bits (the byte at tcpFlagsOff; the 4 reserved bits above it are
// part of the same byte in the wire format but never touched here).
const (
	tcpFlagFIN = 0x01
	tcpFlagSYN = 0x02
	tcpFlagRST = 0x04
	tcpFlagPSH = 0x08
	tcpFlagACK = 0x10
	tcpFlagURG = 0x20
	tcpFlagECE = 0x40
	tcpFlagCWR = 0x80
)

// isPlainIPv4TCP reports whether pkt is an IPv4 packet with no options
// (IHL==5) carrying a TCP segment with no options (data offset==5), and long
// enough to actually hold both fixed headers. Both fast paths (split and
// coalesce) refuse anything else and fall back to the untouched per-packet
// path — the safe default whenever this returns false.
func isPlainIPv4TCP(pkt []byte) bool {
	if len(pkt) < ipv4HeaderLen+tcpMinHdrLen {
		return false
	}
	if pkt[0]>>4 != 4 || pkt[0]&0x0f != 5 { // version 4, IHL 5 (no options)
		return false
	}
	if pkt[ipProtoOff] != 6 { // TCP
		return false
	}
	tcpOff := ipv4HeaderLen
	if len(pkt) < tcpOff+tcpMinHdrLen {
		return false
	}
	dataOffWords := pkt[tcpOff+12] >> 4
	return dataOffWords == 5 // TCP data offset 5 == 20 bytes, no options
}

// ones16 folds a 32-bit accumulated sum down to a 16-bit one's-complement
// checksum, per RFC 1071.
func ones16(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// sumBytes returns the RFC 1071 running sum (not yet folded/complemented) of
// b, treated as big-endian 16-bit words. Used to build up a checksum across
// several non-contiguous regions (pseudo-header, TCP header, payload) before
// folding once at the end.
func sumBytes(b []byte) uint32 {
	var sum uint32
	n := len(b)
	i := 0
	for ; i+1 < n; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if i < n { // odd trailing byte: pad with a zero low byte
		sum += uint32(b[i]) << 8
	}
	return sum
}

// ipv4Checksum computes the IPv4 header checksum over a 20-byte header with
// the checksum field itself treated as zero, per RFC 791 §3.1.
func ipv4Checksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i < ipv4HeaderLen; i += 2 {
		if i == ipChecksumOff {
			continue // treat the existing checksum field as zero
		}
		sum += uint32(binary.BigEndian.Uint16(hdr[i : i+2]))
	}
	return ones16(sum)
}

// tcpChecksum computes the TCP checksum over ip (whose header gives the
// pseudo-header fields) and tcpSeg (TCP header + payload, checksum field
// treated as zero), per RFC 793 §3.1.
func tcpChecksum(ip []byte, tcpSeg []byte) uint16 {
	var sum uint32
	// Pseudo-header: src IP, dst IP, zero, protocol, TCP length.
	sum += uint32(binary.BigEndian.Uint16(ip[ipSrcOff : ipSrcOff+2]))
	sum += uint32(binary.BigEndian.Uint16(ip[ipSrcOff+2 : ipSrcOff+4]))
	sum += uint32(binary.BigEndian.Uint16(ip[ipDstOff : ipDstOff+2]))
	sum += uint32(binary.BigEndian.Uint16(ip[ipDstOff+2 : ipDstOff+4]))
	sum += uint32(ip[ipProtoOff])
	sum += uint32(len(tcpSeg))
	// TCP header + payload, with the checksum field zeroed.
	sum += sumBytes(tcpSeg[:tcpChecksumOff])
	sum += sumBytes(tcpSeg[tcpChecksumOff+2:])
	return ones16(sum)
}

// flowKey identifies a TCP/IPv4 flow by its 4-tuple, direction-agnostic
// (source and destination are whatever the packet says — the coalescer only
// ever compares two keys for equality, never interprets which side is local).
type flowKey struct {
	src, dst netipAddr4
	sport    uint16
	dport    uint16
}

// netipAddr4 avoids importing net/netip into the hot path for a 4-byte
// comparison; [4]byte is directly comparable, which is all flowKey needs.
type netipAddr4 [4]byte

func flowOf(pkt []byte) flowKey {
	var k flowKey
	copy(k.src[:], pkt[ipSrcOff:ipSrcOff+4])
	copy(k.dst[:], pkt[ipDstOff:ipDstOff+4])
	tcpOff := ipv4HeaderLen
	k.sport = binary.BigEndian.Uint16(pkt[tcpOff+tcpSrcPortOff : tcpOff+tcpSrcPortOff+2])
	k.dport = binary.BigEndian.Uint16(pkt[tcpOff+tcpDstPortOff : tcpOff+tcpDstPortOff+2])
	return k
}

func tcpSeq(pkt []byte) uint32 {
	tcpOff := ipv4HeaderLen
	return binary.BigEndian.Uint32(pkt[tcpOff+tcpSeqOff : tcpOff+tcpSeqOff+4])
}

func tcpFlags(pkt []byte) byte {
	return pkt[ipv4HeaderLen+tcpFlagsOff]
}

func ipTotalLen(pkt []byte) uint16 {
	return binary.BigEndian.Uint16(pkt[ipTotalLenOff : ipTotalLenOff+2])
}
