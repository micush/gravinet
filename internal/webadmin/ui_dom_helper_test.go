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
