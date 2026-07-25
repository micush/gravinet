package webadmin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestL2NeighborsGet checks the read shape Monitor > L2 Peers draws from.
// Read-only all the way down (systemL2DiscoJSON's own service.* calls only
// ever query live state — see TestSystemL2DiscoGet's identical reasoning),
// so this is safe regardless of whether lldpd happens to be installed on
// the machine running the test suite.
func TestL2NeighborsGet(t *testing.T) {
	ts, c := l2discoTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/l2neighbors", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/l2neighbors = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// Same shape as /api/system/l2disco (handleL2Neighbors deliberately
	// reuses systemL2DiscoJSON — see its own doc comment) — the page reads
	// neighbors/neighbors_available/neighbors_hint/supported/hint/
	// strays/stray_hint directly, plus protocol per neighbor row.
	for _, k := range []string{"supported", "hint", "running", "neighbors", "neighbors_available", "neighbors_hint", "strays", "stray_hint"} {
		if _, ok := out[k]; !ok {
			t.Errorf("reply is missing %q; the page reads it directly", k)
		}
	}
	if _, ok := out["neighbors"].([]any); !ok {
		t.Errorf("neighbors = %#v, want a JSON array even when empty", out["neighbors"])
	}
}

// TestL2PeersNavAndWiring pins Monitor > L2 Peers into the monitor nav
// group after BGP Peers, gated the same way BGP Peers is (a capability
// flag, not a hardcoded position), and checks the render is actually wired
// end to end: dispatch table entry, page function, label, gating, the
// /api/l2neighbors fetch, and a protocol column that can tell LLDP and CDP
// apart — the whole point of this page over what System > L2 Disco already
// silently computed but never displayed.
func TestL2PeersNavAndWiring(t *testing.T) {
	block := indexHTML[strings.Index(indexHTML, "{ name:'monitor'"):]
	block = block[:strings.Index(block, "]},")]
	for _, want := range []string{"'bgp-peers'", "'l2-peers'", "'hosts-file'"} {
		if !strings.Contains(block, want) {
			t.Errorf("the monitor nav group is missing %s:\n%s", want, block)
		}
	}
	if strings.Index(block, "'bgp-peers'") > strings.Index(block, "'l2-peers'") {
		t.Error("L2 Peers must come after BGP Peers in the monitor group")
	}
	if !strings.Contains(indexHTML, "'l2-peers':secL2Peers") {
		t.Error("no 'l2-peers':secL2Peers entry in the section dispatch table")
	}
	if !strings.Contains(indexHTML, "function secL2Peers(") {
		t.Error("secL2Peers is not defined")
	}
	if !strings.Contains(indexHTML, "s==='l2-peers') return 'L2 Peers'") {
		t.Error("label() has no l2-peers -> 'L2 Peers' case")
	}
	// Gated on the same l2discoSupported flag as System > L2 Disco itself —
	// not a separate capability, since both ultimately depend on the same
	// lldpd/lldpcli presence.
	if !strings.Contains(indexHTML, "sec === 'l2disco' || sec === 'l2-peers') return !!state.l2discoSupported") {
		t.Error("sectionVisible() no longer gates l2-peers on state.l2discoSupported")
	}
	if !strings.Contains(indexHTML, "api('/api/l2neighbors')") {
		t.Error("secL2Peers no longer fetches /api/l2neighbors")
	}
	// The protocol column/pill is what actually answers "show LLDP and CDP
	// neighbors" — without it every row would look identical regardless of
	// which protocol found it.
	if !strings.Contains(indexHTML, "n.protocol") {
		t.Error("l2PeersLiveStatus no longer renders each neighbor's protocol")
	}
}

// TestL2PeersNoTitleAlwaysFiltered pins two deliberate UI choices for
// Monitor > L2 Peers: no "L2 Neighbors" <h3> above the table (the section's
// own <h2> plus secHint's description already say what the page is), and
// the filter box present from the very first render rather than only
// appearing once a neighbor shows up — enhanceTable otherwise skips the
// filter/toolbar entirely for a table with no rows and no +/- buttons (see
// its own doc comment on table._forceFilter), which would make the box pop
// into existence later and read as the page rearranging itself.
func TestL2PeersNoTitleAlwaysFiltered(t *testing.T) {
	idx := strings.Index(indexHTML, "function secL2Peers(")
	if idx < 0 {
		t.Fatal("secL2Peers not found")
	}
	body := indexHTML[idx : idx+600]
	if strings.Contains(body, "<h3>L2 Neighbors</h3>") {
		t.Error("secL2Peers still renders a card title; it should be gone")
	}
	idx2 := strings.Index(indexHTML, "async function l2PeersLiveStatus(")
	if idx2 < 0 {
		t.Fatal("l2PeersLiveStatus not found")
	}
	body2 := indexHTML[idx2 : idx2+800]
	if !strings.Contains(body2, "_forceFilter = true") {
		t.Error("l2PeersLiveStatus no longer forces the filter box on for an empty table")
	}
	// The mechanism itself: enhanceTable must actually honor _forceFilter in
	// its early-return gate, or setting the flag above does nothing.
	if !strings.Contains(indexHTML, "table._rowButtons && !table._forceFilter) return;") {
		t.Error("enhanceTable no longer honors table._forceFilter in its early-return gate")
	}
}

// TestL2NeighborsIsProxyable mirrors TestSystemL2DiscoIsProxyable: Monitor >
// L2 Peers follows the currently selected node like every other Monitor
// page (BGP Peers, Route Table, ...), so its endpoint must NOT be pinned in
// the client's LOCAL_API list.
func TestL2NeighborsIsProxyable(t *testing.T) {
	local := indexHTML[strings.Index(indexHTML, "const LOCAL_API = ["):]
	local = local[:strings.Index(local, "];")]
	if strings.Contains(local, "/api/l2neighbors") {
		t.Error("/api/l2neighbors is pinned in LOCAL_API; it should follow the selected node like the other Monitor > * endpoints")
	}
}
