package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func openTestTransport(eng *Engine) (*transport.Transport, error) {
	return transport.Open(transport.Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket,
	})
}

func mustRule(t *testing.T, action, dir, proto string, dport int) *fwRule {
	t.Helper()
	r, err := FirewallRule{Action: action, Direction: dir, Proto: proto, DstPortMin: dport, DstPortMax: dport}.toRule()
	if err != nil {
		t.Fatalf("toRule: %v", err)
	}
	return r
}

func makeL4Addr(src, dst netip.Addr, proto uint8, dstPort uint16) []byte {
	p := make([]byte, 24)
	p[0] = 0x45
	total := uint16(24)
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64
	p[9] = proto
	s, d := src.As4(), dst.As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	p[22], p[23] = byte(dstPort>>8), byte(dstPort)
	return p
}

func TestFirewallDefaultAllow(t *testing.T) {
	f := newFirewall(nil)
	if !f.allow(fwOut, makeL4(6, 1234, 80, 0)) {
		t.Fatal("empty rulebase must allow by default")
	}
	var nilFW *firewall
	if !nilFW.allow(fwOut, makeL4(6, 1, 2, 0)) {
		t.Fatal("nil firewall must allow")
	}
}

// TestFirewallRuleNotesRoundTrip verifies a rule's Notes survives the
// export<->internal conversion (toRule/ruleToExport) that config load, the
// engine API, and persistence all go through — the same round trip Action,
// Src, Dst etc. already need to survive.
func TestFirewallRuleNotesRoundTrip(t *testing.T) {
	in := FirewallRule{Action: "deny", Direction: "in", Proto: "tcp", DstPortMin: 80, DstPortMax: 80,
		Notes: "block legacy HTTP from the corp side"}
	r, err := in.toRule()
	if err != nil {
		t.Fatalf("toRule: %v", err)
	}
	if r.notes != in.Notes {
		t.Fatalf("internal rule notes = %q, want %q", r.notes, in.Notes)
	}
	out := ruleToExport(r)
	if out.Notes != in.Notes {
		t.Fatalf("ruleToExport notes = %q, want %q", out.Notes, in.Notes)
	}
}

func TestFirewallEval(t *testing.T) {
	// Deny outbound TCP to port 80; everything else falls through to allow.
	f := newFirewall([]*fwRule{mustRule(t, "deny", "out", "tcp", 80)})

	if f.allow(fwOut, makeL4(6, 5000, 80, 0)) {
		t.Fatal("tcp/80 outbound should be denied")
	}
	if !f.allow(fwOut, makeL4(6, 5000, 443, 0)) {
		t.Fatal("tcp/443 should fall through to allow")
	}
	if !f.allow(fwOut, makeL4(17, 5000, 80, 0)) {
		t.Fatal("udp/80 should not match a tcp rule")
	}
	if !f.allow(fwIn, makeL4(6, 5000, 80, 0)) {
		t.Fatal("inbound tcp/80 should not match an out-only rule")
	}
}

func TestFirewallCIDR(t *testing.T) {
	r, err := FirewallRule{Action: "deny", Dst: "10.5.0.0/16"}.toRule()
	if err != nil {
		t.Fatal(err)
	}
	f := newFirewall([]*fwRule{r})
	a := netip.MustParseAddr
	if f.allow(fwOut, makeL4Addr(a("10.5.1.1"), a("10.5.9.9"), 6, 1)) {
		t.Fatal("dst in 10.5.0.0/16 should be denied")
	}
	if !f.allow(fwOut, makeL4Addr(a("10.5.1.1"), a("10.6.0.1"), 6, 1)) {
		t.Fatal("dst outside the prefix should be allowed")
	}
}

func ids(rules []*fwRule) []uint64 {
	out := make([]uint64, len(rules))
	for i, r := range rules {
		out[i] = r.id
	}
	return out
}

func TestFirewallManagement(t *testing.T) {
	f := newFirewall(nil)
	a := f.add(mustRule(t, "allow", "both", "tcp", 22), -1)
	b := f.add(mustRule(t, "deny", "both", "tcp", 80), -1)
	c := f.add(mustRule(t, "allow", "both", "udp", 53), -1)
	// order: a, b, c
	if got := ids(f.current()); got[0] != a.id || got[1] != b.id || got[2] != c.id {
		t.Fatalf("initial order wrong: %v", got)
	}

	// Reorder: move c to the front -> c, a, b
	if !f.move(c.id, 0) {
		t.Fatal("move failed")
	}
	if got := ids(f.current()); got[0] != c.id || got[1] != a.id || got[2] != b.id {
		t.Fatalf("after move: %v", got)
	}

	// Copy a and b to the clipboard, paste at front. Clipboard preserves
	// current order (a before b), pasted copies get fresh ids.
	if n := f.copy([]uint64{b.id, a.id}); n != 2 {
		t.Fatalf("copy count %d", n)
	}
	if n := f.paste(0); n != 2 {
		t.Fatalf("paste count %d", n)
	}
	cur := f.current()
	if len(cur) != 5 {
		t.Fatalf("expected 5 rules after paste, got %d", len(cur))
	}
	// First two are fresh copies (new ids), preserving a-then-b order by action.
	if cur[0].action != fwAllow || cur[1].action != fwDeny {
		t.Fatalf("pasted order wrong: %v", []fwAction{cur[0].action, cur[1].action})
	}
	if cur[0].id == a.id || cur[1].id == b.id {
		t.Fatal("pasted rules should have fresh ids")
	}

	// Cut the original a and b (still present further down) -> removed + on clipboard.
	before := len(f.current())
	if n := f.cut([]uint64{a.id, b.id}); n != 2 {
		t.Fatalf("cut count %d", n)
	}
	if len(f.current()) != before-2 {
		t.Fatalf("cut should remove 2 rules")
	}
	if indexOf(f.current(), a.id) != -1 || indexOf(f.current(), b.id) != -1 {
		t.Fatal("cut rules should be gone")
	}

	// Delete everything remaining; default-allow returns.
	f.remove(ids(f.current()))
	if len(f.current()) != 0 {
		t.Fatal("rulebase should be empty")
	}
	if !f.allow(fwOut, makeL4(6, 1, 80, 0)) {
		t.Fatal("empty rulebase must allow")
	}
}

func TestFirewallStateful(t *testing.T) {
	a := netip.MustParseAddr
	mk := func(fr FirewallRule) *fwRule {
		r, err := fr.toRule()
		if err != nil {
			t.Fatalf("toRule: %v", err)
		}
		return r
	}

	// Just "deny new inbound". Statefulness is automatic — no state field.
	rules := func() []*fwRule {
		return []*fwRule{mk(FirewallRule{Action: "deny", Direction: "in"})}
	}

	f := newFirewall(rules())
	if !f.stateful.Load() {
		t.Fatal("presence of a deny rule should enable connection tracking")
	}

	// We initiate an outbound connection: allowed (default) and now tracked.
	out := makeUDP(a("10.0.0.1"), a("10.0.0.2"), 1000, 53, nil)
	if !f.allow(fwOut, out) {
		t.Fatal("outbound new connection should be allowed")
	}
	// Its reply comes back automatically, despite the inbound-deny rule.
	reply := makeUDP(a("10.0.0.2"), a("10.0.0.1"), 53, 1000, nil)
	if !f.allow(fwIn, reply) {
		t.Fatal("reply to our connection should be allowed automatically")
	}
	// An unsolicited inbound connection is denied.
	unsol := makeUDP(a("10.0.0.9"), a("10.0.0.1"), 9999, 80, nil)
	if f.allow(fwIn, unsol) {
		t.Fatal("unsolicited inbound connection should be denied")
	}

	// Proof it's truly stateful: with no prior outbound, the same reply packet
	// is denied (no flow to be a reply to).
	f2 := newFirewall(rules())
	if f2.allow(fwIn, makeUDP(a("10.0.0.2"), a("10.0.0.1"), 53, 1000, nil)) {
		t.Fatal("inbound with no established flow must be denied")
	}

	// A rulebase with no deny rule needs no tracking (stays on the fast path).
	f3 := newFirewall([]*fwRule{mk(FirewallRule{Action: "allow", Direction: "in"})})
	if f3.stateful.Load() {
		t.Fatal("no deny rule should not enable conntrack")
	}
}

func TestFirewallConntrackExpiry(t *testing.T) {
	a := netip.MustParseAddr
	c := newConntrack()
	now := time.Unix(1000, 0)
	c.track(17, a("10.0.0.1"), 1000, a("10.0.0.2"), 53, now)
	if c.readState(17, a("10.0.0.2"), 53, a("10.0.0.1"), 1000) != ctEstablished {
		t.Fatal("reply should read as established")
	}
	// New (unconfirmed) flows expire after the short TTL.
	c.sweep(now.Add(ctTTLNew + time.Second))
	if c.readState(17, a("10.0.0.2"), 53, a("10.0.0.1"), 1000) != ctNew {
		t.Fatal("flow should have expired")
	}
}

func TestFirewallDataPath(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xF1233)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.20.0.1"))
	// B denies inbound TCP to port 80.
	ks, _ := crypto.NewKeySet([]string{key})
	devB := newFakeDev("B")
	engB := NewEngine(Options{
		NodeID: "B", Hostname: "B",
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: devB, Self4: netip.MustParseAddr("10.20.0.2"),
			FirewallEnabled: true, FirewallRules: []FirewallRule{{Action: "deny", Direction: "in", Proto: "tcp", DstPortMin: 80, DstPortMax: 80}},
		}},
	})
	trB, err := openTestTransport(engB)
	if err != nil {
		t.Fatal(err)
	}
	engB.Attach(trB)
	engB.Start()
	B := &testNode{engB, trB, devB}

	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}

	src := netip.MustParseAddr("10.20.0.1")
	dst := netip.MustParseAddr("10.20.0.2")
	// Denied packet (tcp/80) then an allowed one (udp/53).
	A.dev.in <- makeL4Addr(src, dst, 6, 80)
	allowed := makeL4Addr(src, dst, 17, 53)
	A.dev.in <- allowed

	select {
	case got := <-B.dev.out:
		if got[9] != 17 { // proto byte: must be the UDP packet, not the denied TCP one
			t.Fatalf("firewall let the wrong packet through (proto=%d)", got[9])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("allowed packet never arrived")
	}
	// Nothing else should arrive (the tcp/80 packet was dropped at B).
	select {
	case extra := <-B.dev.out:
		t.Fatalf("unexpected second packet through firewall (proto=%d)", extra[9])
	case <-time.After(500 * time.Millisecond):
	}
}

// TestFirewallEnableToggle proves the live enable flag: a deny rule bites only
// when the firewall is enabled, and disabling reverts to allow-all without
// touching the rulebase. This is the behavior that makes the web "enable" work.
func TestFirewallEnableToggle(t *testing.T) {
	f := newFirewall([]*fwRule{mustRule(t, "deny", "", "tcp", 22)})

	if f.allow(fwOut, makeL4(6, 1234, 22, 0)) {
		t.Fatal("enabled firewall should deny tcp/22")
	}
	f.setEnabled(false)
	if !f.allow(fwOut, makeL4(6, 1234, 22, 0)) {
		t.Fatal("disabled firewall should allow everything (allow-all)")
	}
	f.setEnabled(true)
	if f.allow(fwOut, makeL4(6, 1234, 22, 0)) {
		t.Fatal("re-enabled firewall should deny tcp/22 again")
	}
	if !f.allow(fwOut, makeL4(6, 1234, 80, 0)) {
		t.Fatal("tcp/80 should pass (no matching rule, default allow)")
	}
}
