package mesh

import (
	"net/netip"
	"testing"
)

// preferTestNet builds a network whose forwarding snapshot has `origins`
// advertising `prefix`, each with its own metric, all with live sessions.
func preferTestNet(t *testing.T, prefix netip.Prefix, metrics map[string]int, prefer []string) (*Engine, *netState) {
	t.Helper()
	e, ns := testEngineWithNet(t)
	ns.mu.Lock()
	for id, m := range metrics {
		// A brand-new peerSession counts as live for both families: familyLive
		// is deliberately optimistic until a probe round has actually failed.
		ps := &peerSession{nodeID: id}
		ns.byNode[id] = ps
		ns.redist = append(ns.redist, routeEntry{prefix: prefix, origin: id, metric: m})
	}
	ns.mu.Unlock()
	if prefer != nil {
		pm := map[netip.Prefix][]string{prefix: prefer}
		ns.advPrefer.Store(&pm)
	}
	ns.mu.Lock()
	ns.publishFwd()
	ns.mu.Unlock()
	return e, ns
}

// The question this feature answers: four peers each advertise 0.0.0.0/0 at
// different metrics, and the operator wants one of the others. Without a
// receiver-side preference the only lever is asking those peers to renumber
// their metrics, which is not always possible.
func TestPreferredOriginWinsOverLowerMetric(t *testing.T) {
	p := netip.MustParsePrefix("0.0.0.0/0")
	_, ns := preferTestNet(t, p, map[string]int{
		"peerA": 10, // lowest metric — would win by default
		"peerB": 20,
		"peerC": 30,
		"peerD": 40,
	}, []string{"peerC"})

	got, _ := ns.bestRedistOrigins(netip.MustParseAddr("8.8.8.8"))
	if len(got) != 1 || got[0] != "peerC" {
		t.Fatalf("origins = %v, want [peerC] — the preference did not override the metric", got)
	}
}

// The whole point of a preference over a pin: when the preferred origin is no
// longer usable, the next choice takes over on its own. bestRedistOrigins
// already drops origins with no live session before ranking, so this needs no
// timeout and no withdrawal step.
func TestFallbackWhenPreferredOriginGoesAway(t *testing.T) {
	p := netip.MustParsePrefix("0.0.0.0/0")
	_, ns := preferTestNet(t, p, map[string]int{
		"peerA": 10,
		"peerB": 20,
		"peerC": 30,
	}, []string{"peerC", "peerB"})

	got, _ := ns.bestRedistOrigins(netip.MustParseAddr("8.8.8.8"))
	if len(got) != 1 || got[0] != "peerC" {
		t.Fatalf("origins = %v, want [peerC]", got)
	}

	// peerC's session drops.
	ns.mu.Lock()
	delete(ns.byNode, "peerC")
	ns.publishFwd()
	ns.mu.Unlock()

	got, _ = ns.bestRedistOrigins(netip.MustParseAddr("8.8.8.8"))
	if len(got) != 1 || got[0] != "peerB" {
		t.Fatalf("origins = %v, want [peerB] — the second-ranked origin should take over immediately", got)
	}

	// And peerB too: with no listed origin left, plain lowest-metric resumes.
	ns.mu.Lock()
	delete(ns.byNode, "peerB")
	ns.publishFwd()
	ns.mu.Unlock()

	got, _ = ns.bestRedistOrigins(netip.MustParseAddr("8.8.8.8"))
	if len(got) != 1 || got[0] != "peerA" {
		t.Fatalf("origins = %v, want [peerA] — selection must fall back to lowest metric", got)
	}
}

// Preference ranks origins against each other; it must not drag traffic away
// from a more specific prefix another peer advertises.
func TestPreferenceDoesNotBeatSpecificity(t *testing.T) {
	e, ns := testEngineWithNet(t)
	def := netip.MustParsePrefix("0.0.0.0/0")
	spec := netip.MustParsePrefix("10.9.0.0/24")
	ns.mu.Lock()
	for _, id := range []string{"peerDefault", "peerSpecific"} {
		ps := &peerSession{nodeID: id}
		ns.byNode[id] = ps
	}
	ns.redist = append(ns.redist,
		routeEntry{prefix: def, origin: "peerDefault", metric: 1},
		routeEntry{prefix: spec, origin: "peerSpecific", metric: 500},
	)
	ns.publishFwd()
	ns.mu.Unlock()
	pm := map[netip.Prefix][]string{def: {"peerDefault"}}
	ns.advPrefer.Store(&pm)
	_ = e

	got, _ := ns.bestRedistOrigins(netip.MustParseAddr("10.9.0.7"))
	if len(got) != 1 || got[0] != "peerSpecific" {
		t.Fatalf("origins = %v, want [peerSpecific] — a preference on 0.0.0.0/0 must not steal a /24's traffic", got)
	}
}

// Naming an origin takes it out of the equal-cost set: it now outranks its
// siblings rather than sharing flows with them. Unlisted origins still ECMP
// among themselves.
func TestPreferenceBreaksECMPTie(t *testing.T) {
	p := netip.MustParsePrefix("0.0.0.0/0")
	_, ns := preferTestNet(t, p, map[string]int{"peerA": 10, "peerB": 10}, nil)

	got, _ := ns.bestRedistOrigins(netip.MustParseAddr("8.8.8.8"))
	if len(got) != 2 {
		t.Fatalf("origins = %v, want both siblings before a preference is set", got)
	}

	pm := map[netip.Prefix][]string{p: {"peerB"}}
	ns.advPrefer.Store(&pm)
	got, _ = ns.bestRedistOrigins(netip.MustParseAddr("8.8.8.8"))
	if len(got) != 1 || got[0] != "peerB" {
		t.Fatalf("origins = %v, want [peerB] — a named origin should pin, not share", got)
	}
}

// With no preference configured, selection must be byte-for-byte what it was.
func TestNoPreferenceLeavesMetricOrderUnchanged(t *testing.T) {
	p := netip.MustParsePrefix("0.0.0.0/0")
	_, ns := preferTestNet(t, p, map[string]int{"peerA": 10, "peerB": 20, "peerC": 5}, nil)
	got, _ := ns.bestRedistOrigins(netip.MustParseAddr("8.8.8.8"))
	if len(got) != 1 || got[0] != "peerC" {
		t.Fatalf("origins = %v, want [peerC] (lowest metric) when nothing is preferred", got)
	}
}

// An origin named for a prefix it never advertises can never win a comparison
// it is not part of, and must not disturb the ones that are.
func TestPreferringAnAbsentOriginIsHarmless(t *testing.T) {
	p := netip.MustParsePrefix("0.0.0.0/0")
	_, ns := preferTestNet(t, p, map[string]int{"peerA": 10, "peerB": 20}, []string{"peerGhost"})
	got, _ := ns.bestRedistOrigins(netip.MustParseAddr("8.8.8.8"))
	if len(got) != 1 || got[0] != "peerA" {
		t.Fatalf("origins = %v, want [peerA]", got)
	}
}

// clonePreferMap must deep-copy: advPrefer is published by atomic swap and read
// without a lock, so a retained caller slice must not be able to reorder a live
// ranking under the data path.
func TestClonePreferMapCopiesSlices(t *testing.T) {
	p := netip.MustParsePrefix("0.0.0.0/0")
	src := map[netip.Prefix][]string{p: {"peerA", "peerB"}}
	got := clonePreferMap(src)
	src[p][0] = "mutated"
	if got[p][0] != "peerA" {
		t.Fatal("clonePreferMap aliased the caller's slice; a live ranking could be reordered underneath readers")
	}
}

// The metric programmed into the OS table must describe the advertisement this
// node actually follows. Before bestRedistMetric was ranked the same way,
// forwarding left via the preferred origin while the kernel route carried some
// other peer's number.
func TestKernelMetricFollowsThePreferredOrigin(t *testing.T) {
	p := netip.MustParsePrefix("10.9.0.0/24")
	_, ns := preferTestNet(t, p, map[string]int{
		"peerA": 10,
		"peerB": 20,
		"peerC": 300,
	}, []string{"peerC"})

	metric, advertised := ns.bestRedistMetric(p)
	if !advertised {
		t.Fatal("prefix reported as unadvertised")
	}
	if metric != 300 {
		t.Fatalf("metric = %d, want 300 (peerC's) — the kernel route describes a peer we are not using", metric)
	}
}

// With nothing preferred the metric rule is unchanged: lowest wins.
func TestKernelMetricUnchangedWithoutPreference(t *testing.T) {
	p := netip.MustParsePrefix("10.9.0.0/24")
	_, ns := preferTestNet(t, p, map[string]int{"peerA": 10, "peerB": 20, "peerC": 300}, nil)
	metric, advertised := ns.bestRedistMetric(p)
	if !advertised || metric != 10 {
		t.Fatalf("metric = %d advertised = %v, want 10/true", metric, advertised)
	}
}

// bestRedistMetric answers "is this advertised at all", so unlike
// bestRedistOrigins it must not drop an advertiser whose session is momentarily
// down — withdrawing the kernel route on every blink would churn the table.
func TestKernelMetricIgnoresSessionLiveness(t *testing.T) {
	p := netip.MustParsePrefix("10.9.0.0/24")
	_, ns := preferTestNet(t, p, map[string]int{"peerA": 10}, nil)

	ns.mu.Lock()
	delete(ns.byNode, "peerA") // session gone, advertisement still on record
	ns.publishFwd()
	ns.mu.Unlock()

	if _, advertised := ns.bestRedistMetric(p); !advertised {
		t.Fatal("prefix went unadvertised because its origin's session dropped; the kernel route would flap")
	}
	if origins, _ := ns.bestRedistOrigins(netip.MustParseAddr("10.9.0.7")); len(origins) != 0 {
		t.Fatalf("forwarding still offers %v as a next hop with no live session", origins)
	}
}
