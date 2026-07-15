package mesh

import (
	"net/netip"
	"testing"
)

// TestReloadMergesSeedsLive confirms a seed added to config and applied via the
// runtime reload is dialed without a restart (merged into the live seed set),
// and that re-applying the same seed doesn't duplicate it.
func TestReloadMergesSeedsLive(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	ns := e.netSnapshot()[1]
	count := func() int { ns.mu.RLock(); defer ns.mu.RUnlock(); return len(ns.seeds) }
	if count() != 0 {
		t.Fatalf("expected 0 initial seeds, got %d", count())
	}
	ap := netip.MustParseAddrPort("203.0.113.5:51820")
	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{ap}}); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("seed was not merged live on reload; count=%d", count())
	}
	if err := e.ReloadRuntime(1, NetSpec{ID: 1, Seeds: []netip.AddrPort{ap}}); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatalf("re-applying the same seed duplicated it; count=%d", count())
	}
}
