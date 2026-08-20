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
	// Three, not the four this wanted before v782: Mesh > peers' *editable*
	// cell moved to overlayEditCellHTML, which renders the same two stacked
	// families but tags each one so a double-click can target the family under
	// the cursor. That was its own instance of this same bug — the cell showed
	// both addresses while the editor behind it could only ever load
	// p.overlay, so a dual-stack peer's v6 address was visible and not
	// editable. What this test actually guards is "no render site collapsed
	// back to one family," so the editable site is still checked, just against
	// the helper it now uses.
	if n := strings.Count(indexHTML, "overlayCellHTML(p)"); n < 3 {
		t.Errorf("overlayCellHTML(p) appears %d times in indexHTML, want at least 3 (its own definition, "+
			"Mesh > peers' non-editable overlay cell, and Monitor > mesh peers' overlay cell) "+
			"— a render site may have regressed back to esc(p.overlay), which only ever shows one address family", n)
	}
	if !strings.Contains(indexHTML, "function overlayEditCellHTML(p)") {
		t.Fatal("overlayEditCellHTML helper is missing from indexHTML — Mesh > peers' editable overlay cell has no renderer")
	}
	if !strings.Contains(indexHTML, "overlayEditCellHTML(p)+'</td>'") {
		t.Error("Mesh > peers' editable overlay cell no longer calls overlayEditCellHTML — if it fell back to esc(p.overlay) a dual-stack peer's v6 address is gone from the one table that can edit it")
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
// exist for it the same way. Enable BGP itself is no longer a rowTog
// checkbox at all — it's the title pill (bgpEnabled + ondblclick) — so it's
// excluded from this loop and checked by its own assertion instead, for
// the identical "read into the payload but never wired to trigger a save"
// risk this test guards against everywhere else.
func TestBGPEditorTogglesSaveOnChange(t *testing.T) {
	for _, cb := range []string{"autoCb", "asPrependCb"} {
		if !strings.Contains(indexHTML, cb+".onchange") {
			t.Errorf("%s has no .onchange handler — toggling it alone (touching nothing else) would never trigger a save", cb)
		}
	}
	if !strings.Contains(indexHTML, "bgpEnabled = !bgpEnabled;") || !strings.Contains(indexHTML, "pill.ondblclick") {
		t.Error("the BGP title pill's ondblclick doesn't flip bgpEnabled — toggling it would never change what gets saved")
	}
	if !strings.Contains(indexHTML, "enabled: bgpEnabled,") {
		t.Error("doSave's payload doesn't read bgpEnabled — the pill's flip would never reach the server")
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

// TestBGPEditorHasTitlePill guards the SNMP/LLDP/Syslog-style
// enable/disable pill placement decision: like those pages, Enable BGP
// must be the title pill next to the page's own <h2>, not an inline
// checkbox row — and it must be looked up (not recreated) so
// renderBgpEditor's second call per page load (the live-FRR import
// reflection in secBgp's load()) never leaves two pills stacked on the
// same title.
func TestBGPEditorHasTitlePill(t *testing.T) {
	if strings.Contains(indexHTML, "'Enable BGP'") {
		t.Error("\"Enable BGP\" is still a rowTog row; it should be the title pill instead, like SNMP/LLDP/Syslog")
	}
	if !strings.Contains(indexHTML, "host.parentElement.querySelector('h2.sec')") {
		t.Error("the BGP editor's pill isn't looked up via host.parentElement's h2.sec")
	}
	if !strings.Contains(indexHTML, "let pill = h2.querySelector('.pill.tag-toggle');") {
		t.Error("the BGP editor's pill isn't reused across renderBgpEditor's two calls per load — a second render would stack a duplicate pill")
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
	// The standalone "Push to managed peers" card was merged into the Upload
	// card: the peer picker now sits with the upload, an empty selection
	// upgrades this node and a non-empty one pushes the same source to the
	// selected peers. The push endpoint above must still be wired, but the
	// separate card must not come back.
	if strings.Contains(indexHTML, "Push to managed peers") {
		t.Error("the standalone Push-to-managed-peers card is back; its picker belongs in the Upload card now")
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

// TestPushUsesTheSharedChipPicker keeps the push target selector on the same
// widget as the redistribute pickers (Redistribute connected/static/mesh on the
// BGP editor, Redistribute from BGP on Mesh Routes) rather than letting it drift
// back to a bespoke checkbox list. The reason is not consistency for its own
// sake: a checkbox list is fine for three peers and unusable at thirty, which is
// exactly the size where a fleet push starts to matter, and buildRouteChipPicker
// already solves that with search-to-narrow plus a bounded chip list.
func TestPushUsesTheSharedChipPicker(t *testing.T) {
	// The picker now lives in the Upload card, so anchor on that heading and
	// bound the block at the next section, which still covers the picker
	// construction and the Upgrade button's handler.
	pushIdx := strings.Index(indexHTML, "<h3>[gravinet] updates</h3>")
	if pushIdx < 0 {
		t.Fatal("the Upload card is missing")
	}
	block := indexHTML[pushIdx:]
	if end := strings.Index(block, "// infoMetrics renders"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "buildRouteChipPicker(") {
		t.Error("the Upload card no longer builds its peer selector with buildRouteChipPicker — " +
			"a bespoke list here drifts from the redistribute pickers and stops scaling past a handful of peers")
	}
	if strings.Contains(block, "up-push-peer") {
		t.Error("the Upload card still contains the old per-peer checkbox class (up-push-peer)")
	}
	if !strings.Contains(block, "labelOf") {
		t.Error("the peer picker must pass labelOf — peers are keyed by node_id but have to read as hostnames")
	}
	// The picker is keyed on node ids, so the push body must send what get()
	// returns rather than anything derived from the on-screen label.
	if !strings.Contains(block, "peerPicker.get()") {
		t.Error("the push handler no longer reads its target list from the picker's get()")
	}
}

// TestUpgradeAllThenLocalOption guards the "all peers, then this node" rollout
// mode: the picker offers a single item that pushes to every reachable peer and
// then, only if every one applied, upgrades the local (control-plane) node last.
// The things that make it safe rather than a footgun are asserted here — it
// drives both endpoints (fleet push, then local source apply), it gates the
// local phase on every peer having applied (so a bad build reverts on the peers
// before it can reach the node the operator is logged into), it holds
// configured seeds back until last and pushes them one at a time rather than
// batched with the rest of the fleet or with each other (see mesh.ManagedPeer.
// IsSeed's doc comment for why: losing more than one rendezvous point at once
// risks a peer mid-reconnect finding no way back into the mesh), and — the
// canary policy — only the first *non-seed* peer's failure stops the rollout
// before it reaches anyone else; a later peer failing no longer costs every
// other healthy peer its chance to upgrade too (replaced the old "stop on any
// failure" rule, which meant one unrelated peer's transient hiccup could block
// every configured seed from ever being attempted at all). The canary must
// never be a seed unless the target set is nothing but seeds — a real fleet
// hit this exact bug: nodes[0] alone, with no seed check, picked a seed that
// happened to sort alphabetically first among peer hostnames, pushing it
// first instead of last.
func TestUpgradeAllThenLocalOption(t *testing.T) {
	if !strings.Contains(indexHTML, "all peers, then this node") {
		t.Error("the Upgrade picker no longer offers the 'all peers, then this node' option")
	}
	up := strings.Index(indexHTML, "<h3>[gravinet] updates</h3>")
	if up < 0 {
		t.Fatal("the Upload card is missing")
	}
	block := indexHTML[up:]
	if end := strings.Index(block, "// infoMetrics renders"); end > 0 {
		block = block[:end]
	}
	for _, want := range []string{
		"/api/upgrade/push",                  // the fleet push endpoint
		"/api/upgrade/source",                // the local-apply endpoint, upgraded last
		"allThenLocal",                       // the mode flag the handler branches on
		"appliedAll.length === nodes.length", // the "every peer applied" gate on the local phase
		"is_seed",                            // seed status, read from the cluster/targets data
		"pushBatch",                          // the shared per-batch push+stream helper
		"canaryFailed",                       // the sole stop-the-rollout condition
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the all-peers-then-local flow is missing %q", want)
		}
	}
	// The regression test itself: the canary must be selected by excluding
	// seeds first (nodes.find(id => !isSeedId(id))), not just nodes[0] with
	// no seed check at all — and rest must be computed by excluding whatever
	// the canary actually turned out to be, not by assuming it was
	// positionally first (nodes.slice(1) would silently reintroduce the bug
	// the moment the canary isn't nodes[0]).
	if !strings.Contains(block, "const canary = nodes.find(id => !isSeedId(id)) || nodes[0];") {
		t.Error("the canary is no longer seed-excluded — a seed sorting first in the target list could be pushed first instead of last again")
	}
	if !strings.Contains(block, "const rest = nodes.filter(id => id !== canary);") {
		t.Error("'rest' is no longer computed by excluding the actual canary — expected a filter keyed on the canary's real identity, not its list position")
	}
	// The canary must be pushed alone, by itself, before anything else in
	// the list is touched at all.
	if !strings.Contains(block, "pushBatch([canary])") {
		t.Error("the canary is no longer pushed as its own single-element batch ahead of everyone else")
	}
	// Seeds must be pushed one at a time (a single-element batch per call),
	// not folded into the same batched call as the rest of the fleet.
	if !strings.Contains(block, "pushBatch([seedId])") {
		t.Error("seeds are no longer pushed one at a time — expected a pushBatch call with a single-element node list")
	}
	// Only the canary can stop the rollout early — a failure anywhere in the
	// "rest" (non-seed batch or seed loop) must not gate anything that
	// follows it. Guarded by absence: neither of these should exist anymore.
	for _, mustNotHave := range []string{
		"stoppedEarly",                            // the old blanket stop-on-any-failure flag
		"if (nonSeedNodes.length){",               // the old (unconditional-push, but gate-setting) non-seed phase
		"if (!stoppedEarly && seedNodes.length){", // the old seed phase, gated on the non-seed batch's success
	} {
		if strings.Contains(block, mustNotHave) {
			t.Errorf("found %q — the canary-only stop condition may have regressed back toward stop-on-any-failure", mustNotHave)
		}
	}
	// The canary's own result must be checked, and checked before anything
	// else in the list is pushed — "rest" (everything after the canary)
	// must not be touched until canaryFailed has been determined.
	canaryIdx := strings.Index(block, "const canaryApplied = await pushBatch([canary]);")
	restIdx := strings.Index(block, "if (!canaryFailed && rest.length){")
	if canaryIdx < 0 || restIdx < 0 || restIdx < canaryIdx {
		t.Error("expected the canary to be pushed and checked before any of 'rest' is attempted")
	}
}

// TestNetworkBoolTogglesAutoRestart guards the relay and self-seed toggles
// (Settings > Network — per-network config, config.Network.AllowRelay/
// SelfSeed, so one row pair per network rather than folded into the rest of
// that page's genuinely node-global settings): neither NetworkSetAllowRelay
// nor NetworkSetSelfSeed is hot-reloadable, so both must go through edit()'s
// autoRestart=true path (the same mechanism GeoIP/UPnP/remote-shell already
// use — see quietRestart) rather than a manual "restartPending" flag the
// operator would have to notice and act on themselves. An earlier version
// did exactly that, in the wrong place (the Networks list) to boot; this
// exists so neither mistake can quietly happen again.
func TestNetworkBoolTogglesAutoRestart(t *testing.T) {
	nf := strings.Index(indexHTML, "function secSettingsNetwork(c) {")
	if nf < 0 {
		t.Fatal("secSettingsNetwork is missing")
	}
	block := indexHTML[nf:]
	if end := strings.Index(block, "\nfunction secSettingsPerformance"); end > 0 {
		block = block[:end]
	}
	for _, want := range []string{
		"allow_relay", // the relay op
		"self_seed",   // the self-seed op
		"rsCfgs",      // the per-network iteration this section is built from
	} {
		if !strings.Contains(block, want) {
			t.Errorf("secSettingsNetwork is missing %q", want)
		}
	}
	// The actual save calls: autoRestart=true is the third argument to
	// edit(), not a bare payload object followed by nothing (which would
	// leave the setting merely restart-pending).
	for _, want := range []string{
		"edit('/api/network', { op:'allow_relay', net:cf.id, enabled:want }, true)",
		"edit('/api/network', { op:'self_seed', net:cf.id, enabled:want }, true)",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("relay/self-seed toggle no longer routes through edit()'s autoRestart=true — expected %q", want)
		}
	}
	// Must not have regressed back to a hand-rolled restart flag for either.
	if strings.Contains(block, "state.restartPending = true") {
		t.Error("secSettingsNetwork sets state.restartPending directly somewhere — the relay/self-seed toggles should rely on edit()'s built-in handling instead")
	}
	// Must not have regressed back onto the Networks list page (secNetworks)
	// — these are per-network config, but this page's own settings-row
	// layout, not that table's columns.
	nwf := strings.Index(indexHTML, "function secNetworks(c) {")
	if nwf < 0 {
		t.Fatal("secNetworks is missing")
	}
	nwBlock := indexHTML[nwf:]
	if end := strings.Index(nwBlock, "\nfunction secPeers"); end > 0 {
		nwBlock = nwBlock[:end]
	}
	if strings.Contains(nwBlock, "allow_relay") || strings.Contains(nwBlock, "self_seed") {
		t.Error("relay/self-seed appear back on the Networks list page (secNetworks) — they belong in Settings > Network instead")
	}
}

// TestMeshToggleAutoRestart guards the Mesh > Networks "mesh" (full/partial)
// toggle the same way TestNetworkBoolTogglesAutoRestart guards relay/self-
// seed: config.Network.Mesh is not hot-reloadable (NetworkSetMesh's doc
// comment), so the save must go through edit()'s autoRestart=true path
// (quietRestart) rather than a bare api() call that persists the new value
// but never actually restarts the node to apply it — a real bug an earlier
// version of this toggle had, caught only by hand rather than by a test.
// Unlike relay/self-seed (Settings > Network, per TestNetworkBoolTogglesAuto-
// Restart), this control belongs on the Networks list itself (secNetworks):
// it's the topology of the network, displayed and edited right alongside
// subnet/MTU/address, not a node-local preference.
func TestMeshToggleAutoRestart(t *testing.T) {
	nwf := strings.Index(indexHTML, "function secNetworks(c) {")
	if nwf < 0 {
		t.Fatal("secNetworks is missing")
	}
	block := indexHTML[nwf:]
	if end := strings.Index(block, "\nfunction secPeers"); end > 0 {
		block = block[:end]
	}
	for _, want := range []string{
		"data-meshtoggle", // the toggle element itself
		"mesh_mode",       // the op name
	} {
		if !strings.Contains(block, want) {
			t.Errorf("secNetworks is missing %q", want)
		}
	}
	// The actual save call: autoRestart=true is the third argument to
	// edit(), not a bare api() call (which would silently persist the
	// change without ever restarting the node to apply it) and not a
	// manual state.restartPending flag (which would leave the operator to
	// notice a banner and act on it themselves).
	if !strings.Contains(block, "edit('/api/network', { op:'mesh_mode', net, mode:want }, true)") {
		t.Error("mesh toggle no longer routes through edit()'s autoRestart=true path — it will save the new mesh mode without ever restarting the node to apply it")
	}
	if strings.Contains(block, "state.restartPending = true") {
		t.Error("secNetworks sets state.restartPending directly somewhere — the mesh toggle should rely on edit()'s built-in handling instead")
	}
}

// TestMeshTogglePendingFeedback guards two things about the mesh toggle's
// confirm-to-restart window, added after an operator reported it looking
// frozen: (1) it gives immediate in-progress feedback the moment the
// operator confirms, rather than leaving the toggle showing its old value
// unchanged for however long the actual restart+reconnect takes (several
// real seconds — quietRestart polls /api/ping up to 20 times at 1s
// intervals); and (2) failure recovery restores the toggle's properties in
// place rather than replacing the element outright (e.g. via outerHTML) —
// an earlier draft of this used outerHTML, which silently drops
// ondblclick (a JS property, not serialized markup) and leaves the toggle
// permanently unresponsive to further clicks after any failed attempt.
func TestMeshTogglePendingFeedback(t *testing.T) {
	nwf := strings.Index(indexHTML, "function secNetworks(c) {")
	if nwf < 0 {
		t.Fatal("secNetworks is missing")
	}
	block := indexHTML[nwf:]
	if end := strings.Index(block, "\nfunction secPeers"); end > 0 {
		block = block[:end]
	}
	for _, want := range []string{
		"pointerEvents = 'none'", // ignore a rapid second click while in flight
	} {
		if !strings.Contains(block, want) {
			t.Errorf("secNetworks is missing %q — the mesh toggle no longer gives immediate in-progress feedback", want)
		}
	}
	if strings.Contains(block, "s.outerHTML") {
		t.Error("secNetworks assigns s.outerHTML somewhere in the mesh toggle's handler — this silently drops the ondblclick handler (a JS property, not markup) and leaves the toggle dead after any failed attempt; restore individual properties (className/textContent/title) in place instead")
	}
}

// TestBgpNeighborFiltersPillWired guards the BGP neighbor editor's filters
// UI: a clickable per-neighbor summary pill that opens a shared panel with
// two CIDR chip editors (filter in/out), rather than raw comma-separated
// text crammed into a table cell — the panel must actually be built from
// buildCidrChipEditor, its edits must reach the neighbors array (not just
// sit in the widget), and the outer save payload must carry filter_in/
// filter_out through to /api/bgp/config. Four checkpoints a silent typo in
// any of the wiring between them would otherwise slip through undetected.
func TestBgpNeighborFiltersPillWired(t *testing.T) {
	// The per-row summary pill and its single-click (not double-click, since
	// it opens a panel rather than editing a value in place — see
	// TestBgpNeighborMd5CellIsEditable for the precedent this follows).
	if !strings.Contains(indexHTML, "data-nbrfilters") {
		t.Fatal("neighbor render is missing the filters pill")
	}
	if !strings.Contains(indexHTML, "if (openIdx === idx) closeFiltersPanel(); else openFiltersPanel(idx);") {
		t.Error("the filters pill no longer opens/closes the shared filters panel")
	}
	// The panel must live outside the <table> — a sibling in nbrBody, not a
	// second <tr> per neighbor. A per-row detail row would get silently
	// detached from its neighbor by enhanceTable's column sort, which moves
	// any colspan row to the end of the table as a "placeholder" (the same
	// mechanism the empty-state row relies on).
	if !strings.Contains(indexHTML, "nbrBody.appendChild(filtersPanel)") {
		t.Fatal("the filters panel is no longer appended to nbrBody (outside the table)")
	}
	if strings.Contains(indexHTML, "nbr-filters-detail") {
		t.Error("a per-neighbor <tr> detail row reappeared — enhanceTable's sort will detach it from its neighbor (see filtersPanel's own doc comment)")
	}
	// The panel must actually be built from the free-text chip editor, fed
	// this neighbor's current filter_in/filter_out, and write edits back
	// into the neighbors array so doSave picks them up.
	if !strings.Contains(indexHTML, "function buildCidrChipEditor(") {
		t.Fatal("buildCidrChipEditor is missing")
	}
	if !strings.Contains(indexHTML, "buildCidrChipEditor(n.filter_in,") || !strings.Contains(indexHTML, "buildCidrChipEditor(n.filter_out,") {
		t.Error("openFiltersPanel no longer builds a chip editor for filter_in/filter_out")
	}
	if !strings.Contains(indexHTML, "neighbors[idx].filter_in = v;") || !strings.Contains(indexHTML, "neighbors[idx].filter_out = v;") {
		t.Error("the chip editors' onChange no longer writes back into the neighbors array")
	}
	// A basic peer/AS/description/password edit must not clobber an existing
	// neighbor's filters — this is the regression this feature's own row-edit
	// form is most likely to reintroduce, since filter_in/filter_out live
	// outside that form entirely now.
	if !strings.Contains(indexHTML, "filter_in: idx != null ? neighbors[idx].filter_in : [],") {
		t.Error("the row-edit form no longer preserves an existing neighbor's filter_in across a basic edit")
	}
	// And the outer payload sent to /api/bgp/config must carry each
	// neighbor's filter_in/filter_out through.
	if !strings.Contains(indexHTML, "filter_in:n.filter_in||[], filter_out:n.filter_out||[]") {
		t.Error("the BGP config save payload no longer includes each neighbor's filter_in/filter_out")
	}
}
