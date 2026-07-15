package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// newBanTestEngine makes a standalone engine (no transport) for exercising the
// ban state machine deterministically.
func newBanTestEngine(t *testing.T, name string, netID uint64, ttl time.Duration) (*Engine, *netState) {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{mustKey()})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID:   name,
		Hostname: name,
		Nets:     []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: netip.MustParseAddr("10.1.0.1"), BanTTL: ttl}},
	})
	eng.Attach(nopSender{})
	return eng, eng.network(netID)
}

type nopSender struct{}

func (nopSender) Send(netip.AddrPort, []byte) error { return nil }

func mustKey() string { k, _ := crypto.GenerateKey(); return k }

func TestBanExpiry(t *testing.T) {
	const netID = uint64(0x7777)
	eng, ns := newBanTestEngine(t, "A", netID, time.Hour)

	if err := eng.BanNode(netID, "victim", "x"); err != nil {
		t.Fatal(err)
	}
	if !ns.isBanned("victim") {
		t.Fatal("victim should be banned right after BanNode")
	}
	// Sweep in the present: nothing expires.
	eng.sweepBans(ns, time.Now())
	if !ns.isBanned("victim") {
		t.Fatal("ban should survive a present-time sweep")
	}
	// Sweep well past the TTL: the ban lapses (dead-origin self-heal).
	eng.sweepBans(ns, time.Now().Add(2*time.Hour))
	if ns.isBanned("victim") {
		t.Fatal("ban should have expired after TTL")
	}
	if len(eng.ListBans(netID)) != 0 {
		t.Fatalf("expired ban should be gone, got %v", eng.ListBans(netID))
	}
}

func TestBanRefreshExtendsExpiry(t *testing.T) {
	const netID = uint64(0x7778)
	eng, ns := newBanTestEngine(t, "A", netID, time.Hour)
	_ = eng.BanNode(netID, "victim", "x")

	ns.mu.RLock()
	before := ns.bans[banKey("A", "victim")].expiresNano
	ns.mu.RUnlock()

	// Refresh advances the expiry of our own bans.
	future := time.Now().Add(30 * time.Minute)
	eng.refreshBans(ns, future)
	ns.mu.RLock()
	after := ns.bans[banKey("A", "victim")].expiresNano
	ns.mu.RUnlock()
	if after <= before {
		t.Fatalf("refresh should extend expiry: before=%d after=%d", before, after)
	}
}

// TestBanRefreshVersioning checks that onBanAdd accepts a newer-expiry refresh
// but ignores a stale duplicate (so floods converge and don't loop).
func TestBanRefreshVersioning(t *testing.T) {
	const netID = uint64(0x7779)
	eng, ns := newBanTestEngine(t, "B", netID, time.Hour) // B receives A's bans

	now := time.Now()
	r1 := &banRecord{target: "v", origin: "A", notes: "x", atNano: now.UnixNano(), expiresNano: now.Add(time.Hour).UnixNano()}
	ps := &peerSession{nodeID: "A", net: ns}

	eng.onBanAdd(ps, encodeBanAdd(r1)[1:]) // strip ctrl byte (onBanAdd takes body after type)
	if !ns.isBanned("v") {
		t.Fatal("B should enforce A's ban")
	}

	// A refresh with a later expiry must be accepted.
	r2 := *r1
	r2.expiresNano = now.Add(2 * time.Hour).UnixNano()
	eng.onBanAdd(ps, encodeBanAdd(&r2)[1:])
	ns.mu.RLock()
	exp := ns.bans[banKey("A", "v")].expiresNano
	ns.mu.RUnlock()
	if exp != r2.expiresNano {
		t.Fatalf("refresh not applied: got %d want %d", exp, r2.expiresNano)
	}

	// A stale copy (older expiry) must be ignored.
	eng.onBanAdd(ps, encodeBanAdd(r1)[1:])
	ns.mu.RLock()
	exp2 := ns.bans[banKey("A", "v")].expiresNano
	ns.mu.RUnlock()
	if exp2 != r2.expiresNano {
		t.Fatalf("stale ban overwrote fresh expiry: got %d want %d", exp2, r2.expiresNano)
	}
}

// TestBanAdoptUnion verifies the multi-origin (adopt) semantics: two origins ban
// the same target; one unbans and the target stays banned via the other.
func TestBanAdoptUnion(t *testing.T) {
	const netID = uint64(0x777A)
	eng, ns := newBanTestEngine(t, "C", netID, time.Hour)

	now := time.Now()
	fromA := &banRecord{target: "v", origin: "A", notes: "a", atNano: now.UnixNano(), expiresNano: now.Add(time.Hour).UnixNano()}
	fromB := &banRecord{target: "v", origin: "B", notes: "b", atNano: now.UnixNano(), expiresNano: now.Add(time.Hour).UnixNano()}
	psA := &peerSession{nodeID: "A", net: ns}
	psB := &peerSession{nodeID: "B", net: ns}
	eng.onBanAdd(psA, encodeBanAdd(fromA)[1:])
	eng.onBanAdd(psB, encodeBanAdd(fromB)[1:])
	if !ns.isBanned("v") {
		t.Fatal("v should be banned")
	}

	// A unbans (origin A only): B's ban still holds.
	eng.onBanDel(psA, encodeBanDel("A", "v")[1:])
	if !ns.isBanned("v") {
		t.Fatal("v should remain banned via origin B after A unbans")
	}

	// Force-unban clears all origins.
	if err := eng.ForceUnban(netID, "v"); err != nil {
		t.Fatal(err)
	}
	if ns.isBanned("v") {
		t.Fatal("force-unban should clear all origins")
	}
}
