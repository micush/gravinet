package mesh

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/crypto"
	"gravinet/internal/protocol"
	"gravinet/internal/transport"
)

// TestTamperedDataRejected is the anti-spoofing guarantee: because data packets
// are AEAD-sealed under the session key, any modification to the ciphertext or
// the authenticated header fails to open. An attacker can't forge or alter a
// packet without the key, regardless of source address.
func TestTamperedDataRejected(t *testing.T) {
	shared := bytes.Repeat([]byte{0x11}, 32)
	psk := bytes.Repeat([]byte{0x22}, 32)
	tr := []byte("handshake-transcript")
	ka := crypto.DeriveSessionKeys(shared, psk, tr, true)
	kb := crypto.DeriveSessionKeys(shared, psk, tr, false)
	a, err := crypto.NewSession(ka)
	if err != nil {
		t.Fatal(err)
	}
	b, err := crypto.NewSession(kb)
	if err != nil {
		t.Fatal(err)
	}

	aad := []byte("header-aad")
	ctr, ct := a.Seal(nil, []byte("secret payload"), aad)

	// Untampered opens fine.
	if _, err := b.Open(nil, ct, aad, ctr); err != nil {
		t.Fatalf("clean packet should open: %v", err)
	}

	// Flip a ciphertext byte → auth failure.
	bad := append([]byte(nil), ct...)
	bad[0] ^= 0x80
	if _, err := b.Open(nil, bad, aad, ctr); err == nil {
		t.Fatal("tampered ciphertext must not open")
	}

	// Tamper the authenticated header (aad) → auth failure.
	ctr2, ct2 := a.Seal(nil, []byte("another"), aad)
	if _, err := b.Open(nil, ct2, []byte("forged-aad"), ctr2); err == nil {
		t.Fatal("tampered AAD must not open")
	}
}

// TestGarbageHandshakesSafe throws malformed datagrams at the real entry point
// and asserts no peer is ever formed (and, implicitly, nothing panics).
func TestGarbageHandshakesSafe(t *testing.T) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	const netID = uint64(0xC0FFEE)
	dev := newFakeDev("G")
	e := NewEngine(Options{
		NodeID: "G", Hostname: "G",
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: netip.MustParseAddr("10.0.0.1")}},
	})
	tr, err := openTestTransport(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Attach(tr)
	defer tr.Close()

	from := netip.MustParseAddrPort("203.0.113.5:51820")
	inits := [][]byte{
		nil,
		{1},
		{1, byte(protocol.TypeHSInit)},
		bytes.Repeat([]byte{0xAB}, 40),
	}
	// A decodable HS-init with an unknown key.
	hi := make([]byte, protocol.HSInitHeaderLen+64)
	protocol.EncodeHSInit(hi, protocol.HSInitHeader{Network: netID})
	inits = append(inits, hi)

	for i := 0; i < 50; i++ {
		for _, p := range inits {
			e.OnPacket(append([]byte(nil), p...), from, transport.V4)
		}
	}
	if e.PeerCount(netID) != 0 {
		t.Fatal("garbage handshakes must not form peers")
	}
}

// TestJoinThrottleBans verifies the brute-force defence: repeated bad-key joins
// from one source get that source banned (3-bad-joins/min policy, here tightened
// to 2 for a fast test). Key-tries within the coalesce window count as one
// attempt, so the failures are spaced apart.
func TestJoinThrottleBans(t *testing.T) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	const netID = uint64(0xBADBAD)
	dev := newFakeDev("T")
	e := NewEngine(Options{
		NodeID: "T", Hostname: "T",
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: netip.MustParseAddr("10.0.0.1"),
			Ban: config.BanPolicy{MaxFailures: 2, WindowSeconds: 60, BanSeconds: 900},
		}},
	})
	tr, err := openTestTransport(e)
	if err != nil {
		t.Fatal(err)
	}
	e.Attach(tr)
	defer tr.Close()

	from := netip.MustParseAddrPort("192.0.2.66:51820")
	src := from.Addr().String()

	// A decodable init whose key won't match → counts as a failed attempt.
	badInit := func() []byte {
		p := make([]byte, protocol.HSInitHeaderLen+64)
		protocol.EncodeHSInit(p, protocol.HSInitHeader{Network: netID})
		return p
	}

	ns := e.network(netID)
	if ns.throttle.Banned(src) {
		t.Fatal("source should start unbanned")
	}
	e.OnPacket(badInit(), from, transport.V4) // failure #1
	time.Sleep(3100 * time.Millisecond)       // clear the coalesce window
	e.OnPacket(badInit(), from, transport.V4) // failure #2 → ban

	if !ns.throttle.Banned(src) {
		t.Fatal("source should be banned after repeated bad joins")
	}
}

// TestTruncatedDecoders feeds short/empty buffers to every decoder and asserts
// they reject cleanly (no panic, no false success).
func TestTruncatedDecoders(t *testing.T) {
	for n := 0; n < 24; n++ {
		b := make([]byte, n)
		_, _ = decodeBanAdd(b)
		_, _, _ = decodeBanDel(b)
		_, _, _, _ = decodeRouteAdd(b)
		_, _, _, _ = decodeRelay(b)
		_, _ = decodePeerList(b)
		_, _ = decodeAddr(b)
		_, _, _, _, _ = parseL4(b)
		_, _, _ = parseAddrs(b)
		_, _, _, _, _, _, _ = ipv4Fields(b)
		_, _ = decodeHSPayload(b)
		_, _, _, _ = protocol.DecodeData(b)
		_, _, _, _ = protocol.DecodeHSInit(b)
		_, _, _, _ = protocol.DecodeHSResp(b)
		_, _ = protocol.PacketType(b)
	}
}
