// Package protocol defines gravinet's on-the-wire framing. The first two bytes
// of every outer UDP payload are always [version][type]. See docs/ARCHITECTURE.md.
package protocol

import (
	"encoding/binary"
	"errors"

	"gravinet/internal/crypto"
)

// Version is the current wire version.
const Version byte = 1

// Type identifies the packet kind (byte 1 of every packet).
type Type byte

const (
	TypeData      Type = 0x01 // established session data (and control sub-channel)
	TypeHSInit    Type = 0x02 // handshake initiation
	TypeHSResp    Type = 0x03 // handshake response
	TypeKeepalive Type = 0x04 // NAT keepalive (empty, session-protected)
)

// MTU/fragmentation constants.
const (
	DefaultTunnelMTU = 9216 // jumbo-frame default for the overlay interface
	MinTunnelMTU     = 576

	// DataHeaderLen is bytes consumed by a data packet header before ciphertext.
	// [ver:1][type:1][recv_session:4][counter:8]
	DataHeaderLen = 1 + 1 + 4 + 8

	// DataAADLen is the portion of the data header used as AEAD additional data
	// (everything except the counter). The counter does not need to be in the
	// AAD because it is the AEAD nonce — tampering with it fails decryption.
	DataAADLen = 1 + 1 + 4

	// GCMOverhead is the AEAD tag length added to every ciphertext.
	GCMOverhead = 16

	// MaxUDPPayload bounds a single outer datagram we accept on receive. Emitted
	// data datagrams are kept within the configured underlay MTU by the mesh
	// layer, which fragments larger overlay packets (see internal/mesh/frag.go);
	// this ceiling covers handshakes and any datagram a higher underlay MTU allows.
	MaxUDPPayload = 9472
)

var (
	// ErrShort means the buffer is too small to contain the declared header.
	ErrShort = errors.New("protocol: packet too short")
	// ErrVersion means an unsupported wire version.
	ErrVersion = errors.New("protocol: bad version")
)

// DataHeader is the cleartext prefix of a data packet. The body that follows
// is AEAD ciphertext (inner overlay packet) plus the GCM tag.
type DataHeader struct {
	RecvSession uint32 // index the receiver assigned to this session
	Counter     uint64 // AEAD nonce source + replay sequence
}

// EncodeData writes the header into dst (must be >= DataHeaderLen) and returns
// the number of bytes written. The caller appends the ciphertext after it.
func EncodeData(dst []byte, h DataHeader) int {
	dst[0] = Version
	dst[1] = byte(TypeData)
	binary.BigEndian.PutUint32(dst[2:6], h.RecvSession)
	binary.BigEndian.PutUint64(dst[6:14], h.Counter)
	return DataHeaderLen
}

// DecodeData parses a data packet, returning the header and the ciphertext
// slice (a sub-slice of pkt, not copied). The cleartext header doubles as the
// AEAD additional-authenticated-data, binding the session index and counter.
func DecodeData(pkt []byte) (h DataHeader, aad, ciphertext []byte, err error) {
	if len(pkt) < DataHeaderLen+GCMOverhead {
		return h, nil, nil, ErrShort
	}
	if pkt[0] != Version {
		return h, nil, nil, ErrVersion
	}
	h.RecvSession = binary.BigEndian.Uint32(pkt[2:6])
	h.Counter = binary.BigEndian.Uint64(pkt[6:14])
	return h, pkt[:DataAADLen], pkt[DataHeaderLen:], nil
}

// HSInitHeader is the cleartext prefix of a handshake initiation. The body is
// AEAD(nonce||ciphertext) sealed under the matched pre-shared key.
//
//	[ver:1][type:1][network:8][keyID:8] then sealed body
type HSInitHeader struct {
	Network uint64
	KeyID   crypto.KeyID
}

// HSInitHeaderLen is the cleartext header size before the sealed body.
const HSInitHeaderLen = 1 + 1 + 8 + crypto.KeyIDSize

// EncodeHSInit writes the cleartext header into dst (>= HSInitHeaderLen).
func EncodeHSInit(dst []byte, h HSInitHeader) int {
	dst[0] = Version
	dst[1] = byte(TypeHSInit)
	binary.BigEndian.PutUint64(dst[2:10], h.Network)
	copy(dst[10:10+crypto.KeyIDSize], h.KeyID[:])
	return HSInitHeaderLen
}

// DecodeHSInit parses an HS_INIT, returning the header, the cleartext AAD, and
// the sealed body.
func DecodeHSInit(pkt []byte) (h HSInitHeader, aad, body []byte, err error) {
	if len(pkt) < HSInitHeaderLen {
		return h, nil, nil, ErrShort
	}
	if pkt[0] != Version {
		return h, nil, nil, ErrVersion
	}
	h.Network = binary.BigEndian.Uint64(pkt[2:10])
	copy(h.KeyID[:], pkt[10:10+crypto.KeyIDSize])
	return h, pkt[:HSInitHeaderLen], pkt[HSInitHeaderLen:], nil
}

// PacketType peeks the type byte after validating version.
func PacketType(pkt []byte) (Type, error) {
	if len(pkt) < 2 {
		return 0, ErrShort
	}
	if pkt[0] != Version {
		return 0, ErrVersion
	}
	return Type(pkt[1]), nil
}

// HSRespHeader is the cleartext prefix of a handshake response. RecvSession
// echoes the initiator's chosen index so it can match the response to its
// pending handshake. The body is AEAD(nonce||ciphertext) sealed under the same
// pre-shared key the initiator used.
//
//	[ver:1][type:1][network:8][recv_session:4] then sealed body
type HSRespHeader struct {
	Network     uint64
	RecvSession uint32
}

// HSRespHeaderLen is the cleartext header size before the sealed body.
const HSRespHeaderLen = 1 + 1 + 8 + 4

// EncodeHSResp writes the cleartext header into dst (>= HSRespHeaderLen).
func EncodeHSResp(dst []byte, h HSRespHeader) int {
	dst[0] = Version
	dst[1] = byte(TypeHSResp)
	binary.BigEndian.PutUint64(dst[2:10], h.Network)
	binary.BigEndian.PutUint32(dst[10:14], h.RecvSession)
	return HSRespHeaderLen
}

// DecodeHSResp parses an HS_RESP, returning the header, cleartext AAD, and the
// sealed body.
func DecodeHSResp(pkt []byte) (h HSRespHeader, aad, body []byte, err error) {
	if len(pkt) < HSRespHeaderLen {
		return h, nil, nil, ErrShort
	}
	if pkt[0] != Version {
		return h, nil, nil, ErrVersion
	}
	h.Network = binary.BigEndian.Uint64(pkt[2:10])
	h.RecvSession = binary.BigEndian.Uint32(pkt[10:14])
	return h, pkt[:HSRespHeaderLen], pkt[HSRespHeaderLen:], nil
}
