package webadmin

import (
	"strings"
	"testing"
)

// TestNoStandaloneTrOrTdViaInnerHTML guards against reintroducing the bug
// behind "BGP editor renders blank / hangs on Checking… forever with no
// error": the embedded UI's $() helper parses its argument as innerHTML on a
// plain <div>, and every browser's HTML parser silently drops a bare <tr> or
// <td> there — they're only valid inside a real <table> context — so
// $('<tr></tr>') and $('<td></td>') both evaluate to null. The very next
// .appendChild on that null then throws, and since this happens inside a
// synchronous DOM-building call with nothing above it to catch the
// exception, the render aborts before the finished element is ever attached
// — leaving whatever was on screen before the call (a spinner, a "Checking…"
// label, the previous card) exactly as it was, no visible error at all.
//
// This exact failure mode only shows up once real data reaches the affected
// table (an imported or manually-added BGP neighbor row) — a brand-new,
// empty config never exercises it — which is why it can ship unnoticed.
//
// The fix is to build <tr>/<td> nodes with document.createElement instead,
// which has no such context requirement. This test scans the served page for
// the broken pattern directly, rather than running the JS (this package has
// no JS runtime dependency in its test suite), so it fails loudly if the
// pattern — in the BGP editor or in any future table built the same way —
// ever comes back.
func TestNoStandaloneTrOrTdViaInnerHTML(t *testing.T) {
	bad := []string{`$('<tr>`, `$('<tr></tr>')`, `$("<tr>`, `$('<td>`, `$('<td></td>')`, `$("<td>`}
	for _, pat := range bad {
		if idx := strings.Index(indexHTML, pat); idx >= 0 {
			t.Errorf("found %q in indexHTML at byte offset %d — a standalone <tr>/<td> built via the $() "+
				"innerHTML helper parses to null in every real browser (only valid inside a <table>), and the "+
				"next .appendChild on it throws, silently aborting the render. Use document.createElement('tr'/'td') instead.",
				pat, idx)
		}
	}
}

// TestDualStackOverlayAddressNotCollapsedToOneFamily guards against
// reintroducing the bug behind "no IPv6 addressing displayed in Mesh >
// peers nor Monitor > mesh peers": a dual-stack peer's mesh.PeerInfo
// already carries both overlay4 and overlay6 (see mesh/ban.go's PeerInfo
// struct — both fields are genuinely populated by the handshake, not just
// one), but the admin UI's peerRowsForNet used to fold them into a single
// p.overlay value ("Overlay4||...||Overlay6", picking v4 whenever it was
// present at all), and every table that rendered a peer's overlay column
// showed only that one value — so a dual-stack peer's v6 address was
// silently dropped from view entirely, not merely deprioritized. Only a
// v6-only peer (no v4 assigned) ever showed v6, which is what let this
// ship unnoticed: nothing about it looks broken unless you're specifically
// looking for the second address on a peer that has both.
//
// The fix carries overlay4/overlay6 through as their own fields on each
// row (independent of p.overlay, which intentionally stays a single
// address — see peerOverlayEdit's doc comment, since its inline editor
// only ever targets one family's field at a time) and renders both,
// stacked, via a shared overlayCellHTML helper used by every place a
// peer row's overlay column is actually drawn. This test scans the served
// page for that wiring directly, rather than running the JS (this package
// has no JS runtime dependency in its test suite): it fails if
// overlayCellHTML disappears, if either of the two known render sites
// (Mesh > peers' editable and non-editable overlay cells, and Monitor >
// mesh peers) stops calling it, or if peerRowsForNet stops carrying
// overlay4/overlay6 on a peer row.
func TestDualStackOverlayAddressNotCollapsedToOneFamily(t *testing.T) {
	if !strings.Contains(indexHTML, "function overlayCellHTML(p)") {
		t.Fatal("overlayCellHTML helper is missing from indexHTML — the shared renderer for a peer's overlay address(es)")
	}
	if n := strings.Count(indexHTML, "overlayCellHTML(p)"); n < 4 {
		t.Errorf("overlayCellHTML(p) appears %d times in indexHTML, want at least 4 (its own definition, "+
			"Mesh > peers' editable and non-editable overlay cells, and Monitor > mesh peers' overlay cell) "+
			"— a render site may have regressed back to esc(p.overlay), which only ever shows one address family", n)
	}
	if !strings.Contains(indexHTML, "overlay4:ov4, overlay6:ov6") {
		t.Error("peerRowsForNet no longer carries overlay4/overlay6 on a peer row — overlayCellHTML would have nothing to render for a dual-stack peer's second address")
	}
	if !strings.Contains(indexHTML, "overlay4:selfOv4, overlay6:selfOv6") {
		t.Error("peerRowsForNet no longer carries overlay4/overlay6 on the self row — a dual-stack node's own second address would be missing from its own peers table")
	}
}

// TestPeerAddressCellsWrapInsteadOfTruncating guards against reintroducing
// the bug behind "a long IPv6 address in Mesh > peers / Monitor > mesh peers
// gets cut off with no way to see the rest": table.peers-table's cells
// default to overflow:hidden + text-overflow:ellipsis + white-space:nowrap
// (so every network's card lines up under table-layout:fixed — see that
// rule's own comment), which silently hides whatever doesn't fit instead of
// showing it. A full IPv6 address, or one paired with a port, is exactly the
// kind of content long enough to hit that. The ov-cell/ep-cell classes carry
// an override back to a wrapping, fully-visible cell; this scans for that
// override and for both classes actually being used at every known overlay/
// endpoint render site, rather than running the JS (this package has no JS
// runtime dependency in its test suite).
func TestPeerAddressCellsWrapInsteadOfTruncating(t *testing.T) {
	if !strings.Contains(indexHTML, "td.ov-cell") || !strings.Contains(indexHTML, "td.ep-cell") {
		t.Fatal("no CSS override for td.ov-cell/td.ep-cell — the peers-table default (ellipsis + nowrap) would truncate a long address with no way to see the rest")
	}
	if n := strings.Count(indexHTML, `class="ov-cell`); n < 2 {
		t.Errorf(`class="ov-cell" appears %d times, want at least 2 (secPeers' editable and non-editable overlay cells)`, n)
	}
	if !strings.Contains(indexHTML, `<td class="ov-cell"`) {
		t.Error("infoMeshPeers' overlay cell is missing the ov-cell class — its long addresses would still be ellipsis-truncated")
	}
	if n := strings.Count(indexHTML, `class="ep-cell"`); n < 2 {
		t.Errorf(`class="ep-cell" appears %d times, want at least 2 (secPeers' and infoMeshPeers' endpoint cells)`, n)
	}
}

// TestPeerAddressDisplayStripsIPv6Brackets guards against reintroducing
// bracketed IPv6 literals ("[fd00::2]:51820", Go's netip.AddrPort.String()
// format — correct for anything reparsed elsewhere, but not what should sit
// in a read-only table cell) into Mesh > peers, Monitor > mesh peers, or the
// peer-info lookup dialog. dispAddr strips the brackets for display only,
// reusing splitHostPort's own bracket-aware parsing; this checks the helper
// exists and that every known display site (as opposed to sites that still
// need the raw, reparseable value, like the /api/peer-info request body or
// nodeNotesTitle's seed-address matching, which must NOT change) calls it.
func TestPeerAddressDisplayStripsIPv6Brackets(t *testing.T) {
	if !strings.Contains(indexHTML, "function dispAddr(addr)") {
		t.Fatal("dispAddr helper is missing from indexHTML")
	}
	for _, want := range []string{
		"esc(dispAddr(p.endpointText))",
		"esc(dispAddr(state.nat.public))",
		"esc(dispAddr(p.endpoint))",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("indexHTML is missing %q — that display site would still show a bracketed IPv6 endpoint", want)
		}
	}
	if n := strings.Count(indexHTML, "esc(dispAddr(p.endpointText))"); n < 2 {
		t.Errorf("esc(dispAddr(p.endpointText)) appears %d times, want at least 2 (secPeers' and infoMeshPeers' endpoint cells)", n)
	}
}

// TestSubnetChangeWarnsOfSilentPeerMismatch guards against losing the
// specific warning on editing a network's subnet4/subnet6 in place (Mesh >
// networks): changing it is allowed (see startInlineEdit), but nothing in
// the protocol detects a fleet that's only partway migrated — each node's
// own on-link kernel route only covers its own configured subnet (see
// mesh's assignAddr), so a peer still on the old range simply stops being
// reachable from a node that's moved to the new one, with no error anywhere
// to explain why. The confirm() dialog is the only place that risk is ever
// surfaced to the operator, so this scans for the specific wording rather
// than just "a confirm exists" — a generic restart notice (like
// address4/address6 already have) would not carry the same warning.
func TestSubnetChangeWarnsOfSilentPeerMismatch(t *testing.T) {
	if !strings.Contains(indexHTML, "op:'subnet'") {
		t.Fatal("no op:'subnet' payload found in indexHTML — has the subnet edit path moved?")
	}
	if !strings.Contains(indexHTML, "gravinet does not detect a mismatch") {
		t.Error("the subnet-change confirm() no longer explains that a fleet-wide mismatch goes undetected")
	}
	if !strings.Contains(indexHTML, "simply stop being reachable from this node") {
		t.Error("the subnet-change confirm() no longer explains the actual consequence (a peer on the old range becoming unreachable)")
	}
}

// TestBGPEditorTogglesSaveOnChange guards against a checkbox that's read
// into doSave's payload but never actually wired to *trigger* a save: it
// would then only ever get saved as a side effect of some other field also
// changing in the same sitting, so toggling it alone — then navigating
// away, which is exactly how this shipped unnoticed for AutoBGP — silently
// loses the change. Every rowTog(...) checkbox on the BGP editor whose
// .checked is read into the payload must have a matching .onchange
// assignment. Redistribute connected/static/mesh routes moved from a single
// rowTog checkbox to a rowRouteList picker (many checkboxes, one per CIDR)
// — checked separately below, since a single "rcList.onchange" wouldn't
// exist for it the same way.
func TestBGPEditorTogglesSaveOnChange(t *testing.T) {
	for _, cb := range []string{"enableCb", "autoCb"} {
		if !strings.Contains(indexHTML, cb+".onchange") {
			t.Errorf("%s has no .onchange handler — toggling it alone (touching nothing else) would never trigger a save", cb)
		}
	}
	// rowRouteList's per-item checkbox must trigger a save itself — the
	// exact class of bug this test exists for, just inside a picker with
	// many checkboxes instead of a single toggle with one.
	if !strings.Contains(indexHTML, "selSet.delete(cidr); scheduleSave(true);") {
		t.Error("rowRouteList's checkbox has no scheduleSave call — toggling a route in the redistribute connected/static/mesh pickers alone would never trigger a save")
	}
}

// "Redistribute from BGP" subcard (config.Network.RedistributeBGP/
// RedistributeBGPMetric — BGP-into-mesh redistribution, the reverse of BGP's
// own "Redistribute mesh routes" toggle): that it exists, that its state
// toggle and metric cell both post to /api/network's redistribute-bgp op
// (not /api/route — this isn't a Route entry), and that toggling preserves
// the current metric rather than silently resetting it to 0.
func TestRedistributeFromBGPSubcard(t *testing.T) {
	if !strings.Contains(indexHTML, "Redistribute from BGP") {
		t.Fatal("secRoutes is missing the \"Redistribute from BGP\" subcard heading")
	}
	if !strings.Contains(indexHTML, "cf.redistribute_bgp") || !strings.Contains(indexHTML, "cf.redistribute_bgp_metric") {
		t.Error("the subcard no longer reads cf.redistribute_bgp/cf.redistribute_bgp_metric from the loaded config")
	}
	if n := strings.Count(indexHTML, "op:'redistribute-bgp'"); n < 2 {
		t.Errorf("op:'redistribute-bgp' appears %d times, want at least 2 (the state-toggle and metric-cell edits)", n)
	}
	// The state toggle must read the metric back out of the row's own data
	// attribute (not hardcode 0), or double-clicking the state tag would
	// silently reset a nonzero metric every time it's flipped.
	if !strings.Contains(indexHTML, "metric: parseInt(tag.closest('tr').dataset.metric,10)||0") {
		t.Error("the state-toggle no longer preserves the row's current metric when posting redistribute-bgp")
	}
}
