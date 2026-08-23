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

// TestPeerOverlayEditRaisesNoDialogOnSuccess: editing a peer's overlay address
// must cost the operator no modal at all on the way through.
//
// It cost two, then one, then none, and the shrinking is the story. The commit
// path raised a confirm — "saves on that node now, takes effect on its next
// restart" — and then an alert repeating it on success. v875 removed the alert.
// v878 found the confirm's one substantive claim had been false since v857,
// which made the reload rebuild the network and apply the address immediately,
// and rewrote the wording. v879 removed the matching own-node confirm on Mesh >
// Networks once its restart went away.
//
// That a confirmation could go on stating something untrue for two releases is
// the argument against it, not for it: nobody was reading it. What it said that
// was worth saying — the write lands on the peer rather than here, and applies
// there at once — is in the cell's own tooltip now, where it appears *before*
// the operator commits instead of after they have typed an address and pressed
// Enter.
//
// Failure paths still interrupt. A save error and the wrong-family warning are
// the two cases the operator has not already been told about.
func TestPeerOverlayEditRaisesNoDialogOnSuccess(t *testing.T) {
	src := uiSrc(t)
	i := strings.Index(src, "function peerOverlayEdit")
	if i < 0 {
		t.Fatal("peerOverlayEdit not found")
	}
	body := src[i:]
	j := strings.Index(body, "\nfunction infoMeshPeers")
	if j < 0 {
		t.Fatal("could not find the end of peerOverlayEdit; the bound below is unreliable")
	}
	body = body[:j]

	if n := strings.Count(body, "confirm("); n != 0 {
		t.Errorf("peerOverlayEdit raises %d confirm dialogs, want none: the edit is deliberate, reversible, and explained by the cell's tooltip before it is made", n)
	}
	if strings.Contains(body, "noticeModal('Saved") {
		t.Error("a success notice is back")
	}
	if strings.Contains(body, "takes effect on its next restart") {
		t.Error("the pre-v857 restart claim is back somewhere in this editor; the reload rebuilds the network and applies the address immediately")
	}
	// The remaining notices must all be failure paths. v916 replaced native
	// alert() with noticeModal throughout; what is asserted is unchanged —
	// both failures still have to reach the operator, and nothing else may.
	for _, ok := range []string{
		"noticeModal((r.body && r.body.error) || 'save failed')",
		"noticeModal('That looks like an IPv'",
	} {
		if !strings.Contains(body, ok) {
			t.Errorf("expected failure notice is missing: %s", ok)
		}
	}
	if n := strings.Count(body, "noticeModal("); n != 2 {
		t.Errorf("peerOverlayEdit raises %d notices, want 2 (both failures)", n)
	}
	if strings.Contains(body, "alert(") {
		t.Error("a native alert is back; a suppressed dialog would drop this failure silently")
	}

	// Removing the dialog must not delete what it said. The tooltip is the only
	// place left that carries it.
	tip := src[strings.Index(src, "function overlayEditCellHTML"):]
	if e := strings.Index(tip, "\nfunction "); e > 0 {
		tip = tip[:e]
	}
	for _, want := range []string{
		"not this one",       // which node the write lands on
		"immediately",        // when it takes effect
		"drops and re-forms", // what that costs
	} {
		if !strings.Contains(tip, want) {
			t.Errorf("the overlay cell tooltip no longer says %q — the confirm was removed and its content went with it", want)
		}
	}
}
