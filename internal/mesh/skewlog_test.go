package mesh

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/logx"
	"gravinet/internal/protocol"
	"gravinet/internal/transport"
)

// TestVersionMismatchIsLogged confirms dispatch() reports a wire-version
// mismatch at Debug level (see dispatch's doc comment for why Debug — this
// is unauthenticated, so anything louder risks being a log-flood vector
// against a default-configured node) instead of the silent drop that
// existed before. Also confirms other malformed input (too short to even
// contain a version byte) does *not* trigger this specific message — only a
// genuine version mismatch should.
func TestVersionMismatchIsLogged(t *testing.T) {
	var buf bytes.Buffer
	testLog := logx.New(&buf, logx.LevelDebug)

	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	const netID = uint64(0x7E51)
	dev := newFakeDev("V")
	e := NewEngine(Options{
		NodeID: "V", Hostname: "V", Log: testLog,
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: netip.MustParseAddr("10.0.0.1")}},
	})
	tr, err := openTestTransport(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Attach(tr)
	defer tr.Close()

	from := netip.MustParseAddrPort("203.0.113.9:51820")

	// A packet whose first byte doesn't match protocol.Version.
	bad := []byte{protocol.Version + 1, byte(protocol.TypeHSInit), 0, 0, 0, 0}
	e.OnPacket(bad, from, transport.V4)

	got := buf.String()
	if !bytes.Contains([]byte(got), []byte("version mismatch")) {
		t.Fatalf("expected a version-mismatch log line, got: %q", got)
	}
	if !bytes.Contains([]byte(got), []byte(from.String())) {
		t.Fatalf("log line should name the source address, got: %q", got)
	}

	// A too-short packet (can't even contain a version byte) is a different
	// failure (protocol.ErrShort) and must not produce this message.
	buf.Reset()
	e.OnPacket([]byte{1}, from, transport.V4)
	if bytes.Contains(buf.Bytes(), []byte("version mismatch")) {
		t.Fatalf("a too-short packet should not be reported as a version mismatch, got: %q", buf.String())
	}
}

// TestClockSkewHandshakeIsLogged constructs a real, properly PSK-sealed
// HS_INIT — one that passes authentication — with a deliberately skewed
// timestamp, and confirms onHSInit now logs a diagnostic naming the peer,
// network, skew direction, and tolerance, instead of the silent drop that
// existed before. Authentication succeeding first (rather than a garbage
// packet) is the point: this is the case that's provably not anonymous
// internet noise, which is what justifies logging it at Warn unconditionally.
func TestClockSkewHandshakeIsLogged(t *testing.T) {
	var buf bytes.Buffer
	testLog := logx.New(&buf, logx.LevelDebug)

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	const netID = uint64(0x5CE5)
	dev := newFakeDev("S")
	e := NewEngine(Options{
		NodeID: "S", Hostname: "S", Log: testLog,
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: netip.MustParseAddr("10.0.0.1")}},
	})
	tr, err := openTestTransport(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Attach(tr)
	defer tr.Close()

	rawKey, err := crypto.DecodeKey(key)
	if err != nil {
		t.Fatalf("DecodeKey: %v", err)
	}
	keyID := crypto.DeriveKeyID(rawKey)

	// A real HS_INIT: correct cleartext header, PSK-sealed body — this must
	// pass ns.keys.Load().Lookup and crypto.OpenWithKey to reach the
	// freshTimestamp check at all.
	hdrBuf := make([]byte, protocol.HSInitHeaderLen)
	n := protocol.EncodeHSInit(hdrBuf, protocol.HSInitHeader{Network: netID, KeyID: keyID})
	aad := hdrBuf[:n]

	skewedTime := time.Now().Add(10 * time.Minute).UnixNano() // well past the 120s tolerance, peer "ahead"
	pl := hsPayload{
		Index:     1,
		TimeNano:  skewedTime,
		Ephemeral: bytes.Repeat([]byte{0xCD}, ephemeralLen),
		NodeID:    "skewed-peer",
		Hostname:  "skewed-host",
	}
	sealed, err := crypto.SealWithKey(rawKey, encodeHSPayload(pl), aad)
	if err != nil {
		t.Fatalf("SealWithKey: %v", err)
	}
	full := append(append([]byte(nil), hdrBuf...), sealed...)

	from := netip.MustParseAddrPort("203.0.113.10:51820")
	e.OnPacket(full, from, transport.V4)

	got := buf.String()
	for _, want := range []string{"skewed-peer", "ahead", "2m0s"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("expected log to contain %q, got: %q", want, got)
		}
	}

	// And no session should have formed — the diagnostic is additive, not a
	// behavior change.
	if e.PeerCount(netID) != 0 {
		t.Fatalf("PeerCount = %d, want 0 (clock-skewed handshake must still be rejected)", e.PeerCount(netID))
	}
}
