package webadmin

// The Mesh > peers overlay cell rendered both of a dual-stack peer's addresses
// while its inline editor only ever loaded one of them (p.overlay, which is
// overlay4-preferred), so the v6 address was displayed and not editable. These
// tests pin the shape that fixes it. They assert on ui.go's source because
// that's where the UI is — the same approach ui_dom_helper_test.go takes — and
// because the failure mode being guarded against is a silent regression in the
// wiring, not in behavior a Go-level API can be called to observe.

import (
	"os"
	"strings"
	"testing"
)

func uiSrc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("ui.go")
	if err != nil {
		t.Fatalf("read ui.go: %v", err)
	}
	return string(b)
}

// TestOverlayEditCellTagsBothFamilies: the editable cell must emit a slot for
// each family, including one whose address is currently empty — a single-stack
// peer's missing half was unreachable for exactly the same reason the v6 half
// of a dual-stack peer was.
func TestOverlayEditCellTagsBothFamilies(t *testing.T) {
	src := uiSrc(t)
	i := strings.Index(src, "function overlayEditCellHTML")
	if i < 0 {
		t.Fatal("overlayEditCellHTML is gone; Mesh > peers has no per-family overlay editing")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	for _, want := range []string{"data-ov-fam", "p.overlay4", "p.overlay6"} {
		if !strings.Contains(body, want) {
			t.Errorf("overlayEditCellHTML no longer references %s", want)
		}
	}
	if !strings.Contains(body, "slot('4'") || !strings.Contains(body, "slot('6'") {
		t.Error("overlayEditCellHTML must render a slot for both families, unconditionally")
	}
}

// TestPeerOverlayEditTakesFamilyExplicitly: the family must come from the slot
// that was double-clicked, not from sniffing the typed value. Sniffing is what
// made the v6 address technically-reachable-but-undiscoverable, and it's the
// wrong rule once slots exist: a v6 address typed into the v4 slot is a
// mistake to report, not an instruction to retarget the write.
func TestPeerOverlayEditTakesFamilyExplicitly(t *testing.T) {
	src := uiSrc(t)
	if !strings.Contains(src, "function peerOverlayEdit(td, n, p, fam)") {
		t.Error("peerOverlayEdit must take the family explicitly as a parameter")
	}
	i := strings.Index(src, "function peerOverlayEdit")
	if i < 0 {
		t.Fatal("peerOverlayEdit not found")
	}
	body := src[i:]
	if j := strings.Index(body, "\ninp.onblur"); j > 0 {
		body = body[:j]
	}
	if strings.Contains(body, "targetsV6") {
		t.Error("peerOverlayEdit still infers the family from the typed value (targetsV6)")
	}
	if !strings.Contains(body, "if (v6) body.address6 = v; else body.address4 = v;") {
		t.Error("peerOverlayEdit must submit the family it was opened for")
	}
	if !strings.Contains(body, "p.overlay6 : p.overlay4") {
		t.Error("peerOverlayEdit must seed the editor from the family's own address, not p.overlay")
	}
}

// TestPeersWiresOverlayPerFamily: the double-click has to land on the family
// slots, not the whole cell — a cell-level handler is what forced every
// double-click into the v4 editor regardless of which line was clicked.
func TestPeersWiresOverlayPerFamily(t *testing.T) {
	src := uiSrc(t)
	if !strings.Contains(src, "querySelectorAll('[data-ov-fam]')") {
		t.Error("Mesh > peers does not wire the per-family overlay slots")
	}
	if !strings.Contains(src, "peerOverlayEdit(td, n, p, slot.dataset.ovFam)") {
		t.Error("the overlay double-click must pass the clicked slot's family through")
	}
	if strings.Contains(src, "peerOverlayEdit(td, n, p);") {
		t.Error("a family-less peerOverlayEdit call survives; that call site edits v4 only")
	}
}

// The peers table displays a bare overlay address — Overlay4/Overlay6 come off
// the live mesh session, which has no prefix to report — while
// NetworkSetAddress accepts only a CIDR, and only at the network's own subnet
// prefix length. Seeding the editor with the bare value handed the operator a
// string that could not be saved as-is, with nothing on screen saying what
// length to add (v783).

// TestOverlayPrefixLenReadsSubnet: the length must come from the network's
// subnet, since that's the only length NetworkSetAddress will accept.
func TestOverlayPrefixLenReadsSubnet(t *testing.T) {
	src := uiSrc(t)
	i := strings.Index(src, "function overlayPrefixLen")
	if i < 0 {
		t.Fatal("overlayPrefixLen is gone; the overlay editor no longer knows the required prefix length")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	for _, want := range []string{"cfgOf(", "c.subnet6", "c.subnet4"} {
		if !strings.Contains(body, want) {
			t.Errorf("overlayPrefixLen no longer references %s", want)
		}
	}
	if !strings.Contains(body, "return i < 0 ? '' : sub.slice(i + 1);") {
		t.Error("overlayPrefixLen must return '' when the subnet carries no prefix, not a guess")
	}
}

// TestOverlayWithPrefixLeavesTypedLengthAlone: a value that already has a
// prefix is passed through untouched. Rewriting a wrong length would hide the
// rejection that explains why it was wrong.
func TestOverlayWithPrefixLeavesTypedLengthAlone(t *testing.T) {
	src := uiSrc(t)
	i := strings.Index(src, "function overlayWithPrefix")
	if i < 0 {
		t.Fatal("overlayWithPrefix not found")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "addr.indexOf('/') >= 0") {
		t.Error("overlayWithPrefix must leave an already-prefixed value alone")
	}
	if !strings.Contains(body, "!len") {
		t.Error("overlayWithPrefix must no-op when no prefix length is known")
	}
}

// TestPeerOverlayEditSeedsAndNormalizesCIDR: the editor opens on the CIDR form
// and accepts a bare address back, adding the prefix on the way out.
func TestPeerOverlayEditSeedsAndNormalizesCIDR(t *testing.T) {
	src := uiSrc(t)
	i := strings.Index(src, "function peerOverlayEdit")
	if i < 0 {
		t.Fatal("peerOverlayEdit not found")
	}
	body := src[i:]
	if j := strings.Index(body, "\ninp.onblur"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "const cur = overlayWithPrefix(bare, plen);") {
		t.Error("the editor must open pre-filled with the CIDR form, not the bare address")
	}
	if !strings.Contains(body, "overlayWithPrefix(typed, plen)") {
		t.Error("commit must normalize a bare typed address to a CIDR before sending")
	}
	// The family sniff has to test what was typed, not the normalized value:
	// the appended "/NN" adds no colon on a v4 network today, so checking the
	// wrong string would pass now and trap later.
	if !strings.Contains(body, "const looksV6 = typed.includes(':');") {
		t.Error("the family mismatch check must test the typed value, not the prefix-appended one")
	}
	if strings.Contains(body, "if (v.toLowerCase() !== 'none' && v !== '')") {
		t.Error("the pre-v783 clearing test survives; it tests the normalized value")
	}
}

// TestPeerOverlayEditPromptsOnce: changing a peer's overlay address must cost
// the operator exactly one modal.
//
// The commit path used to raise two: a confirm ("saves on that node now, takes
// effect on its next restart") and then, after a successful save, an alert
// saying the same thing again. The second one asked for a click and told the
// operator nothing they had not just read and agreed to — and it is the only
// success alert on the page, so it was inconsistent as well as redundant.
//
// The confirm is the one that stays: it carries the consequence and offers a
// way out. Success is reported the way every other inline editor here reports
// it, by refreshing the row. Failure alerts are deliberately not counted
// against this — an error is the case the operator has *not* been warned about.
func TestPeerOverlayEditPromptsOnce(t *testing.T) {
	src := uiSrc(t)
	i := strings.Index(src, "function peerOverlayEdit")
	if i < 0 {
		t.Fatal("peerOverlayEdit not found")
	}
	body := src[i:]
	if j := strings.Index(body, "\nfunction infoMeshPeers"); j > 0 {
		body = body[:j]
	} else {
		t.Fatal("could not find the end of peerOverlayEdit; the bound below is unreliable")
	}

	if n := strings.Count(body, "confirm("); n != 1 {
		t.Errorf("peerOverlayEdit raises %d confirm dialogs, want exactly 1", n)
	}
	if strings.Contains(body, "alert('Saved") {
		t.Error("the success alert survives: a confirm the operator already answered, followed by a modal repeating it, is two clicks for one edit")
	}
	// The remaining alerts must all be failure paths. Anything else is a new
	// success popup wearing different words.
	for _, ok := range []string{
		"alert((r.body && r.body.error) || 'save failed')",
		"alert('That looks like an IPv'",
	} {
		if !strings.Contains(body, ok) {
			t.Errorf("expected failure alert is missing: %s", ok)
		}
	}
	if n := strings.Count(body, "alert("); n != 2 {
		t.Errorf("peerOverlayEdit raises %d alerts, want 2 (both failures)", n)
	}
}
