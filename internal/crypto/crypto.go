// Package crypto provides gravinet's symmetric and handshake primitives using
// only the standard library: AES-256-GCM for the data channel, X25519 for
// forward secrecy, and an HMAC-SHA256 HKDF (Go 1.22 has no crypto/hkdf).
//
// Design notes that matter for the rest of the system:
//
//   - Keys are matched across hosts by *identity*, not slot position. A key's
//     ID is the first 8 bytes of SHA-256(key). This is what lets two nodes
//     authenticate when "at least one key matches" even if they live in
//     different rotation slots.
//
//   - The data path never trials keys. A handshake establishes a Session with
//     one fixed AEAD; data packets carry the receiver's session index, so
//     decryption is O(1).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
)

// KeySize is the AES-256 key length.
const KeySize = 32

// KeyIDSize is the length of a derived key identifier.
const KeyIDSize = 8

// KeyID is a short, collision-resistant-enough identifier for a key.
type KeyID [KeyIDSize]byte

// DeriveKeyID returns the identity tag for a key: first 8 bytes of SHA-256.
func DeriveKeyID(key []byte) KeyID {
	sum := sha256.Sum256(key)
	var id KeyID
	copy(id[:], sum[:KeyIDSize])
	return id
}

// GenerateKey returns 32 cryptographically random bytes, base64-encoded for
// storage in the config. Used by `gravinet genkey`.
func GenerateKey() (string, error) {
	b := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// DecodeKey parses a base64 key string and checks its length.
func DecodeKey(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("key not valid base64: %w", err)
	}
	if len(b) != KeySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", KeySize, len(b))
	}
	return b, nil
}

// KeySet holds the active keys for one network, indexed by KeyID for fast
// lookup during handshakes. Slot order is preserved for "try in order" joins.
type KeySet struct {
	byID  map[KeyID][]byte
	order []KeyID // slot order, for initiators trying keys
}

// NewKeySet builds a set from up to 8 base64 key strings; empty/disabled
// entries are skipped. Returns an error on malformed key material.
func NewKeySet(keysB64 []string) (*KeySet, error) {
	ks := &KeySet{byID: make(map[KeyID][]byte)}
	for i, s := range keysB64 {
		if s == "" {
			continue
		}
		raw, err := DecodeKey(s)
		if err != nil {
			return nil, fmt.Errorf("slot %d: %w", i, err)
		}
		id := DeriveKeyID(raw)
		if _, dup := ks.byID[id]; dup {
			continue // same key in two slots; harmless
		}
		ks.byID[id] = raw
		ks.order = append(ks.order, id)
	}
	return ks, nil
}

// Lookup returns the key for an ID, matching by identity across slots.
func (ks *KeySet) Lookup(id KeyID) ([]byte, bool) {
	k, ok := ks.byID[id]
	return k, ok
}

// Order returns key IDs in slot order, for an initiator to try sequentially.
func (ks *KeySet) Order() []KeyID { return ks.order }

// Len reports how many usable keys are present.
func (ks *KeySet) Len() int { return len(ks.order) }

// With returns a new KeySet equal to ks plus rawKey appended as the last slot.
// If rawKey (by derived ID) is already present, ks is returned unchanged. The
// receiver is never mutated, so it is safe to compute a replacement and swap it
// into an atomic.Pointer while other goroutines still hold the old set.
func (ks *KeySet) With(rawKey []byte) *KeySet {
	id := DeriveKeyID(rawKey)
	out := &KeySet{byID: make(map[KeyID][]byte)}
	if ks != nil {
		if _, dup := ks.byID[id]; dup {
			return ks
		}
		for k, v := range ks.byID {
			out.byID[k] = v
		}
		out.order = append(out.order, ks.order...)
	}
	out.byID[id] = append([]byte(nil), rawKey...)
	out.order = append(out.order, id)
	return out
}

// ---- HKDF (RFC 5869) over HMAC-SHA256, stdlib only ----

func hkdfExtract(salt, ikm []byte) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	m := hmac.New(sha256.New, salt)
	m.Write(ikm)
	return m.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) []byte {
	out := make([]byte, 0, length)
	var prev []byte
	counter := byte(1)
	for len(out) < length {
		m := hmac.New(sha256.New, prk)
		m.Write(prev)
		m.Write(info)
		m.Write([]byte{counter})
		prev = m.Sum(nil)
		out = append(out, prev...)
		counter++
	}
	return out[:length]
}

// HKDF derives `length` bytes from input keying material.
func HKDF(ikm, salt, info []byte, length int) []byte {
	return hkdfExpand(hkdfExtract(salt, ikm), info, length)
}

// ---- X25519 handshake material ----

// Ephemeral is one side's transient X25519 keypair.
type Ephemeral struct {
	priv *ecdh.PrivateKey
}

// NewEphemeral generates a fresh X25519 keypair.
func NewEphemeral() (*Ephemeral, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Ephemeral{priv: priv}, nil
}

// Public returns the 32-byte public key to send to the peer.
func (e *Ephemeral) Public() []byte { return e.priv.PublicKey().Bytes() }

// Shared computes the X25519 shared secret with the peer's public key.
func (e *Ephemeral) Shared(peerPub []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("bad peer public key: %w", err)
	}
	return e.priv.ECDH(pub)
}

// SessionKeys are the directional AEAD keys produced by a handshake.
type SessionKeys struct {
	Send    []byte // key used to seal outbound
	Receive []byte // key used to open inbound
}

// DeriveSessionKeys mixes the ECDH secret with the pre-shared key and the
// handshake transcript, then splits into two directional keys. `initiator`
// selects which half is send vs receive so both ends agree.
func DeriveSessionKeys(ecdhShared, psk, transcript []byte, initiator bool) SessionKeys {
	ikm := make([]byte, 0, len(ecdhShared)+len(psk))
	ikm = append(ikm, ecdhShared...)
	ikm = append(ikm, psk...)
	okm := HKDF(ikm, transcript, []byte("gravinet v1 session"), 2*KeySize)
	a, b := okm[:KeySize], okm[KeySize:]
	if initiator {
		return SessionKeys{Send: a, Receive: b}
	}
	return SessionKeys{Send: b, Receive: a}
}

// ---- AEAD session cipher ----

// ErrReplay indicates a packet counter that has already been seen or is too old.
var ErrReplay = errors.New("crypto: replayed or stale packet")

// Cipher is a one-directional AEAD with a monotonic nonce counter (for sealing)
// or a sliding replay window (for opening). A Session pairs two of them.
type Cipher struct {
	aead    cipher.AEAD
	counter uint64 // next send counter
	// replay window state (open side):
	window  uint64
	highest uint64
}

func newCipher(key []byte) (*Cipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func nonceFor(counter uint64) []byte {
	var n [12]byte
	// 4 zero bytes + 8-byte big-endian counter; unique per key for 2^64 packets.
	n[4] = byte(counter >> 56)
	n[5] = byte(counter >> 48)
	n[6] = byte(counter >> 40)
	n[7] = byte(counter >> 32)
	n[8] = byte(counter >> 24)
	n[9] = byte(counter >> 16)
	n[10] = byte(counter >> 8)
	n[11] = byte(counter)
	return n[:]
}

// Seal encrypts plaintext, returning the counter used and the ciphertext.
// The caller writes the counter into the packet header. dst may be nil, or set
// to plaintext[:0] to encrypt in place. The send counter is bumped atomically so
// Seal is safe to call concurrently (each call gets a unique nonce).
func (c *Cipher) Seal(dst, plaintext, aad []byte) (counter uint64, out []byte) {
	counter = atomic.AddUint64(&c.counter, 1) - 1
	out = c.aead.Seal(dst, nonceFor(counter), plaintext, aad)
	return counter, out
}

const replayWindowSize = 64

// Open decrypts a packet given its header counter, enforcing the replay window.
func (c *Cipher) Open(dst, ciphertext, aad []byte, counter uint64) ([]byte, error) {
	if !c.replayOK(counter) {
		return nil, ErrReplay
	}
	pt, err := c.aead.Open(dst, nonceFor(counter), ciphertext, aad)
	if err != nil {
		return nil, err
	}
	c.replayAdvance(counter)
	return pt, nil
}

// replayOK reports whether a counter is acceptable without mutating state.
func (c *Cipher) replayOK(counter uint64) bool {
	if counter > c.highest {
		return true
	}
	diff := c.highest - counter
	if diff >= replayWindowSize {
		return false
	}
	return c.window&(1<<diff) == 0
}

// replayAdvance records that a counter was successfully verified.
func (c *Cipher) replayAdvance(counter uint64) {
	if counter > c.highest {
		shift := counter - c.highest
		if shift >= replayWindowSize {
			c.window = 0
		} else {
			c.window <<= shift
		}
		c.window |= 1
		c.highest = counter
		return
	}
	diff := c.highest - counter
	if diff < replayWindowSize {
		c.window |= 1 << diff
	}
}

// Session bundles the send and receive ciphers for one peer link.
type Session struct {
	send *Cipher
	recv *Cipher
}

// NewSession builds a Session from derived directional keys.
func NewSession(keys SessionKeys) (*Session, error) {
	s, err := newCipher(keys.Send)
	if err != nil {
		return nil, err
	}
	r, err := newCipher(keys.Receive)
	if err != nil {
		return nil, err
	}
	return &Session{send: s, recv: r}, nil
}

// Seal encrypts an outbound packet body.
func (s *Session) Seal(dst, plaintext, aad []byte) (uint64, []byte) {
	return s.send.Seal(dst, plaintext, aad)
}

// Open decrypts an inbound packet body with replay protection.
func (s *Session) Open(dst, ciphertext, aad []byte, counter uint64) ([]byte, error) {
	return s.recv.Open(dst, ciphertext, aad, counter)
}

// ---- handshake AEAD (keyed directly by a PSK, used before a Session exists) ----

// SealWithKey encrypts a handshake payload directly under a PSK with a random
// nonce, returning nonce||ciphertext. Used for HS_INIT/HS_RESP bodies.
func SealWithKey(key, plaintext, aad []byte) ([]byte, error) {
	c, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := c.aead.Seal(nil, nonce, plaintext, aad)
	return append(nonce, ct...), nil
}

// OpenWithKey reverses SealWithKey. Input is nonce||ciphertext.
func OpenWithKey(key, in, aad []byte) ([]byte, error) {
	if len(in) < 12 {
		return nil, errors.New("crypto: handshake payload too short")
	}
	c, err := newCipher(key)
	if err != nil {
		return nil, err
	}
	return c.aead.Open(nil, in[:12], in[12:], aad)
}

// ConstantTimeEqual is a helper for comparing authenticators without leaking
// timing. Exposed for the handshake/auth code.
func ConstantTimeEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }
