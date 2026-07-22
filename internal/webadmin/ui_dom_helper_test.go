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
	for _, cb := range []string{"enableCb", "autoCb", "asPrependCb"} {
		if !strings.Contains(indexHTML, cb+".onchange") {
			t.Errorf("%s has no .onchange handler — toggling it alone (touching nothing else) would never trigger a save", cb)
		}
	}
	// rowRouteList's add (picking a search result) and remove (the chip's ×
	// button) must each trigger a save themselves — the exact class of bug
	// this test exists for, just inside a search-to-add picker instead of a
	// single toggle. buildRouteChipPicker (the shared widget) calls its
	// onChange callback on both; rowRouteList must wire that callback to
	// scheduleSave, or the callback existing is meaningless.
	if !strings.Contains(indexHTML, "selSet.add(cidr); searchInp.value = ''; drawOpts(); drawChips(); onChange(Array.from(selSet));") {
		t.Error("buildRouteChipPicker's add-a-route action has no onChange call — nothing would ever be told a route was added")
	}
	if !strings.Contains(indexHTML, "selSet.delete(cidr); drawChips(); drawOpts(); onChange(Array.from(selSet));") {
		t.Error("buildRouteChipPicker's remove-a-route action has no onChange call — nothing would ever be told a route was removed")
	}
	if !strings.Contains(indexHTML, "buildRouteChipPicker(available, selected, () => scheduleSave(true));") {
		t.Error("rowRouteList doesn't wire buildRouteChipPicker's onChange to scheduleSave — adding/removing a route in the redistribute connected/static/mesh pickers alone would never trigger a save")
	}
}

// "Redistribute from BGP" subcard (config.Network.RedistributeBGPRoutes/
// RedistributeBGPMetric — BGP-into-mesh redistribution, the reverse of BGP's
// own "Redistribute mesh routes" toggle): that it exists, that its state
// toggle and metric cell both post to /api/network's redistribute-bgp op
// (not /api/route — this isn't a Route entry), and that toggling preserves
// the current metric rather than silently resetting it to 0.
func TestRedistributeFromBGPSubcard(t *testing.T) {
	if !strings.Contains(indexHTML, "Redistribute from BGP") {
		t.Fatal("secRoutes is missing the \"Redistribute from BGP\" subcard heading")
	}
	if !strings.Contains(indexHTML, "cf.redistribute_bgp_routes") || !strings.Contains(indexHTML, "cf.redistribute_bgp_metric") {
		t.Error("the subcard no longer reads cf.redistribute_bgp_routes/cf.redistribute_bgp_metric from the loaded config")
	}
	if !strings.Contains(indexHTML, "op:'redistribute-bgp'") {
		t.Error("the subcard no longer posts op:'redistribute-bgp'")
	}
	// Both the picker (add/remove) and the metric input must post the OTHER
	// one's current value alongside their own change — rbPostUpdate always
	// takes (routes, metric) together, since NetworkSetRedistributeBGPRoutes
	// takes both at once. A regression here would mean editing one silently
	// resets the other back to empty/0.
	if !strings.Contains(indexHTML, "rbPostUpdate(routes, rbMetric)") {
		t.Error("the route picker no longer sends the current metric alongside a route add/remove")
	}
	if !strings.Contains(indexHTML, "rbPostUpdate(rbPicker.get(), rbMetric)") {
		t.Error("the metric input no longer sends the current route selection alongside a metric edit")
	}
}

// TestBgpNeighborMd5CellIsEditable guards the fix for "double-clicking a
// neighbor's MD5 password does nothing, but double-clicking description then
// lets me change the password and save." Root cause: only cells with class
// .nbr-field got the startNbrEdit double-click handler, and the MD5 password
// cell is .nbr-pw-cell (it holds a masked value plus a reveal button, so it
// can't be a plain .nbr-field text cell). Double-clicking description started
// the row edit, whose form happens to include the password input — hence the
// odd workaround the user found. The fix wires startNbrEdit onto the pw cell
// directly. This scans the served JS for that wiring so the trigger can't be
// dropped again.
func TestBgpNeighborMd5CellIsEditable(t *testing.T) {
	// The password cell must carry an ondblclick that starts the row edit,
	// the same startNbrEdit the other neighbor fields use.
	if !strings.Contains(indexHTML, ".nbr-pw-cell')") {
		t.Fatal("neighbor render no longer selects .nbr-pw-cell to wire its editor")
	}
	// Specifically: a double-click on the pw cell starts the shared row edit.
	if !strings.Contains(indexHTML, "pwCell.ondblclick = () => startNbrEdit(tr)") {
		t.Error("the MD5 password cell no longer starts a row edit on double-click — " +
			"double-clicking it will do nothing, the exact bug this test guards against")
	}
	// The reveal button keeps its own single-click handler with
	// stopPropagation, so revealing the password doesn't also trigger the
	// cell's edit. If this regresses, single-click reveal and double-click
	// edit would collide.
	if !strings.Contains(indexHTML, "nbr-pw-toggle") || !strings.Contains(indexHTML, "e.stopPropagation()") {
		t.Error("the MD5 reveal button lost its stopPropagation single-click handler")
	}
}

// TestManagerUpgradeUIWired guards the remote-upgrade UI surface: the local
// opt-in toggle and the manager push control must both be present and pointed
// at the right endpoints, and the local-only security endpoints must be in
// LOCAL_API so the browser never tries to run them against a selected peer.
func TestManagerUpgradeUIWired(t *testing.T) {
	// The opt-in toggle row, its endpoint, and its GET-on-render fetch.
	if !strings.Contains(indexHTML, "accept-manager-upg-row") {
		t.Error("the Accept-Manager-pushed-upgrades settings row is missing")
	}
	if !strings.Contains(indexHTML, "/api/upgrade/accept-manager") {
		t.Error("the opt-in toggle no longer references /api/upgrade/accept-manager")
	}
	// The push control and its endpoint.
	if !strings.Contains(indexHTML, "/api/upgrade/push") {
		t.Error("the push control no longer references /api/upgrade/push")
	}
	if !strings.Contains(indexHTML, "Push to managed peers") {
		t.Error("the Push-to-managed-peers card heading is missing")
	}
	// LOCAL_API must list the two local-only upgrade endpoints (accept-manager
	// is a security toggle; push is a fleet action) and must NOT list
	// remote-apply (the one peer-facing endpoint).
	localIdx := strings.Index(indexHTML, "const LOCAL_API")
	if localIdx < 0 {
		t.Fatal("LOCAL_API list not found")
	}
	localBlock := indexHTML[localIdx : localIdx+1500]
	if !strings.Contains(localBlock, "/api/upgrade/accept-manager") {
		t.Error("LOCAL_API is missing /api/upgrade/accept-manager — the security toggle could be proxied to a peer")
	}
	if !strings.Contains(localBlock, "/api/upgrade/push") {
		t.Error("LOCAL_API is missing /api/upgrade/push")
	}
	if strings.Contains(localBlock, "/api/upgrade/remote-apply") {
		t.Error("LOCAL_API must NOT contain /api/upgrade/remote-apply — it is the one peer-facing upgrade endpoint")
	}
}

// TestUpgradeUIHasNoDeadEndpoints catches the failure mode the test above
// missed: every assertion there is a "this string is present" check, so the UI
// kept passing while it called three endpoints that had been deleted from the
// mux. A stale fetch() is invisible until an operator clicks the button and
// gets a 404 mid-upgrade, which is the worst possible moment to discover it.
//
// Asserting absence is what makes route removal a compile-time-ish failure
// rather than a runtime one, so this lists the routes that no longer exist and
// fails if the UI still names any of them.
func TestUpgradeUIHasNoDeadEndpoints(t *testing.T) {
	gone := []string{
		"/api/upgrade/stage-source", // folded into /api/upgrade/source
		"/api/upgrade/stage",        // binary+manifest upload; binaries are never distributed
		"/api/upgrade/local-apply",  // applied a staged artifact id; nothing is staged now
	}
	for _, path := range gone {
		if strings.Contains(indexHTML, path+"'") || strings.Contains(indexHTML, path+"\"") {
			t.Errorf("the UI still calls %s, which is no longer registered in handler()", path)
		}
	}
	// The one it must call instead.
	if !strings.Contains(indexHTML, "/api/upgrade/source") {
		t.Error("the UI no longer references /api/upgrade/source — the source upload is the only local upgrade path")
	}
	// Signing is gone entirely; a UI branch keyed on it would render a form
	// with no server behind it.
	if strings.Contains(indexHTML, "signing_required") {
		t.Error("the UI still branches on signing_required, which handleUpgradeHome no longer reports")
	}
}
