package mesh

import (
	"net/netip"
	"reflect"
	"testing"

	"gravinet/internal/resolver"
)

// TestDNSAddDelCodecRoundTrip verifies the wire codec for advertised
// conditional-forward domains, including a multi-server entry (unlike a hosts
// record's single IP, a DNS forward carries a variable-length server list).
func TestDNSAddDelCodecRoundTrip(t *testing.T) {
	servers := []netip.Addr{
		netip.MustParseAddr("1.1.1.1"),
		netip.MustParseAddr("2.2.2.2"),
		netip.MustParseAddr("fd00::1"), // mixed v4/v6 in one entry
	}
	add := encodeDNSAdd("origin1", "corp.internal", servers)
	if add[0] != ctrlDNSAdd {
		t.Fatalf("first byte = %#x, want ctrlDNSAdd", add[0])
	}
	origin, domain, got, ok := decodeDNSAdd(add[1:])
	if !ok {
		t.Fatal("decodeDNSAdd: not ok")
	}
	if origin != "origin1" || domain != "corp.internal" {
		t.Fatalf("origin/domain = %q/%q, want origin1/corp.internal", origin, domain)
	}
	if !reflect.DeepEqual(got, servers) {
		t.Fatalf("servers = %v, want %v", got, servers)
	}

	del := encodeDNSDel("origin1", "corp.internal")
	if del[0] != ctrlDNSDel {
		t.Fatalf("first byte = %#x, want ctrlDNSDel", del[0])
	}
	dOrigin, dDomain, ok := decodeDNSDel(del[1:])
	if !ok || dOrigin != "origin1" || dDomain != "corp.internal" {
		t.Fatalf("decodeDNSDel = %q/%q/%v, want origin1/corp.internal/true", dOrigin, dDomain, ok)
	}
}

// TestDNSAddCodecRejectsMalformed checks the same defensive-decode discipline
// hostadv.go's decodeHostAdd applies: truncated or empty-required-field input
// must fail closed, not panic or silently accept.
func TestDNSAddCodecRejectsMalformed(t *testing.T) {
	if _, _, _, ok := decodeDNSAdd(nil); ok {
		t.Error("decodeDNSAdd(nil) should fail")
	}
	// Valid origin+domain but a claimed server count exceeding the actual bytes.
	b := appendLenStr(nil, "origin1")
	b = appendLenStr(b, "corp.internal")
	b = append(b, 5) // claims 5 servers, provides none
	if _, _, _, ok := decodeDNSAdd(b); ok {
		t.Error("decodeDNSAdd should fail when the server list is truncated")
	}
	// Empty domain must be rejected even with a well-formed server list.
	b2 := appendLenStr(nil, "origin1")
	b2 = appendLenStr(b2, "")
	b2 = append(b2, 0)
	if _, _, _, ok := decodeDNSAdd(b2); ok {
		t.Error("decodeDNSAdd should reject an empty domain")
	}
}

// TestDNSRejectFiltersButStillLearns mirrors TestHostRejectFiltersLearned: a
// locally-rejected domain is still learned and relayed (so lifting the reject
// restores it instantly without waiting on the mesh) but dnsRejected reports it
// as filtered, and reloadDNS can lift/reapply the reject live.
func TestDNSRejectFiltersButStillLearns(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
		DNSSync: true, DNSReject: []string{"Blocked.internal"}, // mixed case on purpose
	}}})
	ns := e.netSnapshot()[1]
	ps := &peerSession{net: ns, nodeID: "peer1"}

	if !ns.dnsRejected("blocked.internal") || !ns.dnsRejected("BLOCKED.INTERNAL") {
		t.Error("reject match should be case-insensitive")
	}
	if ns.dnsRejected("ok.internal") {
		t.Error("ok.internal should not be rejected")
	}

	servers := []netip.Addr{netip.MustParseAddr("9.9.9.9")}
	e.onDNSAdd(ps, encodeDNSAdd("peer1", "blocked.internal", servers)[1:])
	e.onDNSAdd(ps, encodeDNSAdd("peer1", "ok.internal", servers)[1:])

	// Both are learned regardless of the reject — it's a filter applied at
	// sync time, not at learn time (same discipline as hostRejected).
	ns.mu.RLock()
	_, lb := ns.learnedDNS[dnsKey("peer1", "blocked.internal")]
	_, lo := ns.learnedDNS[dnsKey("peer1", "ok.internal")]
	ns.mu.RUnlock()
	if !lb || !lo {
		t.Fatalf("both forwards should be learned: blocked=%v ok=%v", lb, lo)
	}

	// Lifting the reject live updates advDNSReject immediately.
	e.reloadDNS(ns, nil, nil)
	if ns.dnsRejected("blocked.internal") {
		t.Fatal("reject should be lifted after reloadDNS(nil reject)")
	}

	// Re-applying it live restores the filter without needing to re-learn.
	e.reloadDNS(ns, nil, []string{"blocked.internal"})
	if !ns.dnsRejected("blocked.internal") {
		t.Fatal("reject should be re-applied after reloadDNS")
	}
	ns.mu.RLock()
	_, stillLearned := ns.learnedDNS[dnsKey("peer1", "blocked.internal")]
	ns.mu.RUnlock()
	if !stillLearned {
		t.Fatal("re-applying the reject must not drop the already-learned record")
	}
}

// TestDNSSignatureStableAndOrderIndependent checks that dnsSignature produces
// the same value regardless of entry/server order, so syncDNS's debounce isn't
// defeated by nondeterministic map iteration order.
func TestDNSSignatureStableAndOrderIndependent(t *testing.T) {
	a := []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2.2.2.2")}
	b := []netip.Addr{netip.MustParseAddr("2.2.2.2"), netip.MustParseAddr("1.1.1.1")} // reordered

	sig1 := dnsSignature([]resolver.Entry{{Domain: "d1", Servers: a}}, nil)
	sig2 := dnsSignature([]resolver.Entry{{Domain: "d1", Servers: b}}, nil)
	if sig1 != sig2 {
		t.Fatalf("signature should be server-order independent: %q vs %q", sig1, sig2)
	}

	// Also independent of entry order across multiple domains.
	c := []resolver.Entry{{Domain: "d2", Servers: a}, {Domain: "d1", Servers: a}}
	d := []resolver.Entry{{Domain: "d1", Servers: a}, {Domain: "d2", Servers: a}}
	if dnsSignature(c, nil) != dnsSignature(d, nil) {
		t.Fatal("signature should be entry-order independent")
	}

	// Search domains are order-independent too.
	s1 := dnsSignature(nil, []string{"corp.internal", "mesh.local"})
	s2 := dnsSignature(nil, []string{"mesh.local", "corp.internal"})
	if s1 != s2 {
		t.Fatalf("search-domain signature should be order independent: %q vs %q", s1, s2)
	}

	// A search-domain-only change (routing entries unchanged) must still
	// change the signature, or syncDNS's debounce would never notice it.
	if dnsSignature(nil, []string{"corp.internal"}) == dnsSignature(nil, []string{"other.internal"}) {
		t.Fatal("changing the search domain set should change the signature")
	}
	if dnsSignature(nil, nil) == dnsSignature(nil, []string{"corp.internal"}) {
		t.Fatal("adding a search domain where there was none should change the signature")
	}
}

// TestSearchDomainsThreadedFromSpec checks that NetSpec.SearchDomains lands in
// ns.searchDomains at construction (so syncDNS sees it from the first tick),
// and that ReloadRuntime updates it live — the same hot-reload treatment as
// AdvDNS/DNSReject, since it's edited through the same web UI section and an
// operator would reasonably expect it to apply without a restart.
func TestSearchDomainsThreadedFromSpec(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: dev,
		Subnet4:       netip.MustParsePrefix("10.30.0.0/24"),
		Self4:         netip.MustParseAddr("10.30.0.5"),
		SearchDomains: []string{"corp.internal"},
	}}})
	ns := e.netSnapshot()[1]

	p := ns.searchDomains.Load()
	if p == nil || len(*p) != 1 || (*p)[0] != "corp.internal" {
		t.Fatalf("searchDomains at construction = %v, want [corp.internal]", p)
	}

	// A live reload with a different SearchDomains list must replace it.
	newSpec := ns.spec
	newSpec.SearchDomains = []string{"mesh.local", "corp.internal"}
	if err := e.ReloadRuntime(1, newSpec); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	p = ns.searchDomains.Load()
	if p == nil || len(*p) != 2 {
		t.Fatalf("searchDomains after reload = %v, want 2 entries", p)
	}

	// And an empty list on reload must clear it, not leave the old one stuck.
	newSpec.SearchDomains = nil
	if err := e.ReloadRuntime(1, newSpec); err != nil {
		t.Fatalf("ReloadRuntime: %v", err)
	}
	p = ns.searchDomains.Load()
	if p == nil || len(*p) != 0 {
		t.Fatalf("searchDomains after clearing = %v, want empty", p)
	}
}

// TestEffectiveSearchDomains covers the pure promotion logic syncDNS relies
// on: configured domains always pass through, byDomain (this node's full
// accepted forward set) is only folded in when learned is true, and an empty
// byDomain or learned=false leaves configured untouched rather than growing a
// nil slice into an empty non-nil one (which would needlessly flip
// dnsSignature on every idle tick).
func TestEffectiveSearchDomains(t *testing.T) {
	byDomain := map[string][]netip.Addr{
		"cush.local":    {netip.MustParseAddr("192.168.168.168")},
		"corp.internal": {netip.MustParseAddr("10.0.0.1")},
	}

	// learned=false: only the configured list, regardless of what's in byDomain.
	got := effectiveSearchDomains([]string{"own.internal"}, byDomain, false)
	if len(got) != 1 || got[0] != "own.internal" {
		t.Fatalf("learned=false: got %v, want [own.internal]", got)
	}

	// learned=true: configured plus every byDomain key.
	got = effectiveSearchDomains([]string{"own.internal"}, byDomain, true)
	want := map[string]bool{"own.internal": true, "cush.local": true, "corp.internal": true}
	if len(got) != len(want) {
		t.Fatalf("learned=true: got %v, want keys %v", got, want)
	}
	for _, d := range got {
		if !want[d] {
			t.Errorf("unexpected domain %q in result", d)
		}
	}

	// Empty byDomain: nothing to add either way.
	if got := effectiveSearchDomains([]string{"own.internal"}, nil, true); len(got) != 1 || got[0] != "own.internal" {
		t.Fatalf("empty byDomain: got %v, want [own.internal]", got)
	}

	// Nil configured, learned=true: byDomain alone still comes through.
	got = effectiveSearchDomains(nil, byDomain, true)
	if len(got) != 2 {
		t.Fatalf("nil configured + learned=true: got %v, want 2 entries", got)
	}
}

// TestSearchLearnedPromotesGossipedForward reproduces the reported gap: a
// DNSSync-only consumer (DNSSync on, no local AdvDNS of its own — e.g. a
// relay node that only forwards mesh traffic, like gn-ionos1) learns a peer's
// conditional forward and, without SearchLearned, only gets it as a routing
// domain (fully-qualified queries resolve, bare ones don't); with
// SearchLearned it also becomes a search suffix.
func TestSearchLearnedPromotesGossipedForward(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
		DNSSync: true, // no AdvDNS, no SearchDomains: a pure consumer, like gn-ionos1
	}}})
	ns := e.netSnapshot()[1]
	ps := &peerSession{net: ns, nodeID: "peer1"}

	servers := []netip.Addr{netip.MustParseAddr("192.168.168.168")}
	e.onDNSAdd(ps, encodeDNSAdd("peer1", "cush.local", servers)[1:])

	ns.mu.RLock()
	byDomain := map[string][]netip.Addr{}
	for _, d := range ns.learnedDNS {
		if !ns.dnsRejected(d.domain) {
			byDomain[d.domain] = d.servers
		}
	}
	ns.mu.RUnlock()

	if len(byDomain) != 1 {
		t.Fatalf("expected the learned forward in byDomain, got %v", byDomain)
	}

	// SearchLearned off (the default, and this node's spec as constructed
	// above): the learned domain must NOT become a search suffix — this is
	// the exact gap reported (FQDN resolves, bare hostname doesn't).
	got := effectiveSearchDomains(nil, byDomain, ns.spec.SearchLearned)
	if len(got) != 0 {
		t.Fatalf("SearchLearned off: expected no search domains, got %v", got)
	}

	// Flipping the opt-in on must promote it.
	got = effectiveSearchDomains(nil, byDomain, true)
	if len(got) != 1 || got[0] != "cush.local" {
		t.Fatalf("SearchLearned on: got %v, want [cush.local]", got)
	}
}

// TestSearchLearnedThreadedFromConfig checks NetSpec.SearchLearned lands in
// ns.spec.SearchLearned at construction, mirroring
// TestSearchDomainsThreadedFromSpec's coverage of NetSpec.SearchDomains.
func TestSearchLearnedThreadedFromConfig(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
		DNSSync: true, SearchLearned: true,
	}}})
	ns := e.netSnapshot()[1]
	if !ns.spec.SearchLearned {
		t.Fatal("expected ns.spec.SearchLearned to be true from NetSpec")
	}
}
