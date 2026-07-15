package mesh

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gravinet/internal/logx"
)

// mkRule compiles an authored rule against a firewall's catalog and installs it,
// returning the firewall for chaining in tests.
func fwWith(t *testing.T, objs []FirewallObject, svcs []FirewallService, rules ...FirewallRule) *firewall {
	t.Helper()
	f := newFirewall(nil)
	f.setCatalog(objs, svcs)
	f.loadRules(rules)
	return f
}

// ---- Feature 1: named objects + groups ----

func TestFirewallObjectHostAndSubnet(t *testing.T) {
	a := netip.MustParseAddr
	objs := []FirewallObject{
		{Name: "webservers", Kind: "host", Addresses: []string{"10.0.0.10", "10.0.0.11"}},
		{Name: "lan", Kind: "subnet", Addresses: []string{"10.9.0.0/16"}},
	}
	// Deny to the webservers object; everything else allowed.
	f := fwWith(t, objs, nil, FirewallRule{Action: "deny", Dst: "webservers"})

	if f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.0.0.10"), 6, 1)) {
		t.Fatal("dst 10.0.0.10 is in webservers, should be denied")
	}
	if f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.0.0.11"), 6, 1)) {
		t.Fatal("dst 10.0.0.11 is in webservers, should be denied")
	}
	if !f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.0.0.12"), 6, 1)) {
		t.Fatal("dst 10.0.0.12 is NOT in webservers, should be allowed")
	}
}

func TestFirewallObjectGroupRecursive(t *testing.T) {
	a := netip.MustParseAddr
	objs := []FirewallObject{
		{Name: "east", Kind: "host", Addresses: []string{"10.0.0.10"}},
		{Name: "west", Kind: "host", Addresses: []string{"10.0.0.20"}},
		{Name: "sites", Kind: "group", Members: []string{"east", "west"}},
		{Name: "everything", Kind: "group", Members: []string{"sites"}}, // nested group
	}
	f := fwWith(t, objs, nil, FirewallRule{Action: "deny", Dst: "everything"})

	for _, ip := range []string{"10.0.0.10", "10.0.0.20"} {
		if f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a(ip), 6, 1)) {
			t.Fatalf("dst %s should be denied via nested group", ip)
		}
	}
	if !f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.0.0.30"), 6, 1)) {
		t.Fatal("dst outside the group should be allowed")
	}
}

func TestFirewallObjectGroupCycleSafe(t *testing.T) {
	// A group that (mis)references itself must not loop forever at compile time.
	objs := []FirewallObject{
		{Name: "g", Kind: "group", Members: []string{"g", "h"}},
		{Name: "h", Kind: "host", Addresses: []string{"10.0.0.5"}},
	}
	done := make(chan *firewall, 1)
	go func() { done <- fwWith(t, objs, nil, FirewallRule{Action: "deny", Dst: "g"}) }()
	select {
	case f := <-done:
		a := netip.MustParseAddr
		if f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.0.0.5"), 6, 1)) {
			t.Fatal("dst 10.0.0.5 (via self-referential group) should still resolve and be denied")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("compiling a self-referential group did not terminate")
	}
}

func TestFirewallObjectRange(t *testing.T) {
	a := netip.MustParseAddr
	objs := []FirewallObject{{Name: "pool", Kind: "range", Addresses: []string{"10.0.0.5-10.0.0.20"}}}
	f := fwWith(t, objs, nil, FirewallRule{Action: "deny", Dst: "pool"})

	for _, ip := range []string{"10.0.0.5", "10.0.0.13", "10.0.0.20"} {
		if f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a(ip), 6, 1)) {
			t.Fatalf("dst %s is inside the range, should be denied", ip)
		}
	}
	for _, ip := range []string{"10.0.0.4", "10.0.0.21"} {
		if !f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a(ip), 6, 1)) {
			t.Fatalf("dst %s is outside the range, should be allowed", ip)
		}
	}
}

func TestRangeToPrefixesExact(t *testing.T) {
	a := netip.MustParseAddr
	// A range that isn't a single CIDR must decompose exactly, covering every
	// address in [lo,hi] and none outside it.
	lo, hi := a("10.0.0.5"), a("10.0.0.20")
	pfx := rangeToPrefixes(lo, hi)
	inRange := func(x netip.Addr) bool {
		return addrToBig(lo).Cmp(addrToBig(x)) <= 0 && addrToBig(x).Cmp(addrToBig(hi)) <= 0
	}
	for i := 0; i < 40; i++ {
		x := a("10.0.0." + itoa(i))
		covered := anyContains(pfx, x)
		if covered != inRange(x) {
			t.Fatalf("addr %s: covered=%v want=%v (prefixes=%v)", x, covered, inRange(x), pfx)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestFirewallUnknownObjectRuleSkipped(t *testing.T) {
	// A rule naming an object that doesn't exist fails to compile and is skipped
	// (not silently widened to "any"), leaving the default-allow policy in force.
	f := newFirewall(nil)
	f.setCatalog(nil, nil)
	f.loadRules([]FirewallRule{{Action: "deny", Dst: "ghost"}})
	if got := len(f.current()); got != 0 {
		t.Fatalf("rule referencing an unknown object should be skipped; got %d rules", got)
	}
}

// ---- Feature 2: service catalog ----

func TestFirewallServiceMultiLeg(t *testing.T) {
	svcs := []FirewallService{{Name: "DNS", Ports: []FirewallServicePort{
		{Proto: "udp", PortMin: 53}, {Proto: "tcp", PortMin: 53},
	}}}
	f := fwWith(t, nil, svcs, FirewallRule{Action: "deny", Services: []string{"DNS"}})

	if f.allow(fwOut, makeL4(17, 5000, 53, 0)) {
		t.Fatal("udp/53 should match the DNS service and be denied")
	}
	if f.allow(fwOut, makeL4(6, 5000, 53, 0)) {
		t.Fatal("tcp/53 should match the DNS service and be denied")
	}
	if !f.allow(fwOut, makeL4(17, 5000, 54, 0)) {
		t.Fatal("udp/54 should not match DNS and be allowed")
	}
	if !f.allow(fwOut, makeL4(6, 5000, 80, 0)) {
		t.Fatal("tcp/80 should not match DNS and be allowed")
	}
}

func TestFirewallUnknownServiceRuleSkipped(t *testing.T) {
	f := newFirewall(nil)
	f.setCatalog(nil, nil)
	f.loadRules([]FirewallRule{{Action: "deny", Services: []string{"nope"}}})
	if got := len(f.current()); got != 0 {
		t.Fatalf("rule referencing an unknown service should be skipped; got %d rules", got)
	}
}

// ---- Feature 3: per-rule counters ----

func TestFirewallCounters(t *testing.T) {
	f := fwWith(t, nil, nil, FirewallRule{Action: "deny", Proto: "tcp", DstPortMin: 80, DstPortMax: 80})
	for i := 0; i < 3; i++ {
		f.allow(fwOut, makeL4(6, 5000, 80, 0))
	}
	f.allow(fwOut, makeL4(6, 5000, 443, 0)) // does not match the rule

	rules := f.current()
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	if got := rules[0].cnt.pkts.Load(); got != 3 {
		t.Fatalf("rule packet count = %d, want 3", got)
	}
	if rules[0].cnt.bytes.Load() == 0 {
		t.Fatal("rule byte count should be non-zero")
	}
	ex := ruleToExport(rules[0])
	if ex.Packets != 3 {
		t.Fatalf("exported Packets = %d, want 3", ex.Packets)
	}

	// Counters survive a recompile (an object/catalog edit) — same tally pointer.
	f.setCatalog([]FirewallObject{{Name: "x", Kind: "host", Addresses: []string{"1.2.3.4"}}}, nil)
	if got := f.current()[0].cnt.pkts.Load(); got != 3 {
		t.Fatalf("counter reset by recompile: got %d, want 3", got)
	}

	// Reset clears them.
	f.resetCounters(nil)
	if got := f.current()[0].cnt.pkts.Load(); got != 0 {
		t.Fatalf("after reset, packets = %d, want 0", got)
	}
}

func TestFirewallCountersSurviveReorder(t *testing.T) {
	f := newFirewall(nil)
	f.setCatalog(nil, nil)
	r1 := mustAdd(t, f, FirewallRule{Action: "deny", Proto: "tcp", DstPortMin: 80, DstPortMax: 80})
	mustAdd(t, f, FirewallRule{Action: "allow", Proto: "udp"})
	f.allow(fwOut, makeL4(6, 5000, 80, 0)) // hit r1

	if !f.move(r1, 1) {
		t.Fatal("move failed")
	}
	// Find r1 after the move and confirm its tally persisted.
	for _, r := range f.current() {
		if r.id == r1 {
			if got := r.cnt.pkts.Load(); got != 1 {
				t.Fatalf("counter lost across reorder: got %d, want 1", got)
			}
			return
		}
	}
	t.Fatal("moved rule not found")
}

func mustAdd(t *testing.T, f *firewall, fr FirewallRule) uint64 {
	t.Helper()
	r, err := f.compile(fr)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return f.add(r, -1).id
}

// ---- Feature 4: per-rule logging ----

func TestFirewallRuleLogging(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(&buf, logx.LevelDebug)
	f := newFirewall(nil)
	f.setLogger(log)
	f.logInterval = time.Hour // force suppression of the 2nd+ within the window
	f.setCatalog(nil, nil)
	f.loadRules([]FirewallRule{{Action: "deny", Proto: "tcp", DstPortMin: 80, DstPortMax: 80, Log: true}})

	f.allow(fwOut, makeL4(6, 5000, 80, 0)) // logged
	f.allow(fwOut, makeL4(6, 5000, 80, 0)) // suppressed (within interval)

	out := buf.String()
	if !strings.Contains(out, "firewall deny rule") {
		t.Fatalf("expected a firewall match log line, got: %q", out)
	}
	if strings.Count(out, "firewall deny rule") != 1 {
		t.Fatalf("expected exactly one log line (rate-limited), got: %q", out)
	}

	// A non-logging rule must stay silent.
	var buf2 bytes.Buffer
	log2 := logx.New(&buf2, logx.LevelDebug)
	f2 := newFirewall(nil)
	f2.setLogger(log2)
	f2.setCatalog(nil, nil)
	f2.loadRules([]FirewallRule{{Action: "deny", Proto: "tcp", DstPortMin: 80, DstPortMax: 80}}) // Log:false
	f2.allow(fwOut, makeL4(6, 5000, 80, 0))
	if strings.Contains(buf2.String(), "firewall deny rule") {
		t.Fatalf("rule without Log set must not log; got: %q", buf2.String())
	}
}

// ---- Feature 5: FQDN objects ----

type stubResolver map[string][]netip.Addr

func (s stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return s[host], nil
}

func TestFirewallFQDNResolution(t *testing.T) {
	a := netip.MustParseAddr
	// Swap in a deterministic resolver for the duration of the test.
	orig := netResolver
	netResolver = stubResolver{"db.example.com": {a("10.7.7.7")}}
	defer func() { netResolver = orig }()

	f := newFirewall(nil)
	f.setCatalog([]FirewallObject{{Name: "db", Kind: "fqdn", Addresses: []string{"db.example.com"}}}, nil)
	f.loadRules([]FirewallRule{{Action: "deny", Dst: "db"}})

	// Before resolution the object is empty; a rule constrained to it matches
	// NOTHING (must not widen to "any"), so unrelated traffic still flows.
	if !f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.7.7.7"), 6, 1)) {
		t.Fatal("before resolution, the fqdn rule should match nothing")
	}

	// Resolve as the engine's periodic task would.
	names := fqdnNames([]string{"db.example.com"})
	pfx, ok := resolveNames(names)
	if !ok || len(pfx) == 0 {
		t.Fatalf("resolveNames failed: ok=%v pfx=%v", ok, pfx)
	}
	if !f.setFQDN("db", pfx) {
		t.Fatal("setFQDN should report a change on first resolution")
	}

	if f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.7.7.7"), 6, 1)) {
		t.Fatal("after resolution, traffic to the resolved address should be denied")
	}
	if !f.allow(fwOut, makeL4Addr(a("10.0.0.1"), a("10.7.7.8"), 6, 1)) {
		t.Fatal("an address the name doesn't resolve to should still be allowed")
	}

	// Re-applying the same set is a no-op (no needless recompile).
	if f.setFQDN("db", pfx) {
		t.Fatal("setFQDN with an unchanged set should report no change")
	}
}
