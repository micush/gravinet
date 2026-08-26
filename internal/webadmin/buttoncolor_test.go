package webadmin

import (
	"strings"
	"testing"
)

// Every toolbar button carries a colour. The Keys, Packet Capture and Logs
// bars each shipped with buttons left as .ghost — transparent and bordered,
// the styling the rest of the app uses for cancel and close — so actions that
// do something sat looking like actions that decline.
//
// cls:'ghost' is checked across every _rowButtons spec rather than only the
// two bars that had them: the styling is what a new toolbar reaches for by
// default, and the point of this test is that the next one does not.
func TestNoToolbarButtonIsGhostStyled(t *testing.T) {
	if strings.Contains(indexHTML, "cls:'ghost'") {
		t.Error("a toolbar button is still ghost-styled")
	}
	// The raw-HTML toolbars (Packet Capture builds its own bar rather than
	// going through _rowButtons) would not be caught by the check above.
	for _, s := range []string{`class="ghost sm">Clear<`, `class="ghost sm">Download<`} {
		if strings.Contains(indexHTML, s) {
			t.Errorf("a packet-capture toolbar button is still ghost-styled: %s", s)
		}
	}
}

// Ghost itself is not removed — it is what cancel, close and dismiss use, and
// those are quiet deliberately. A change that filled those in too would have
// made every editor's cancel compete with its save.
func TestGhostSurvivesForDecliningButtons(t *testing.T) {
	if !strings.Contains(indexHTML, "button.ghost {") {
		t.Fatal("the ghost style itself was removed")
	}
	if !strings.Contains(indexHTML, `class="ghost sm ne-cancel"`) {
		t.Error("an editor's cancel button is no longer ghost-styled")
	}
}

// The Keys bar is the one with something to say beyond "this is a button".
// Enable turns a key on and takes the same green .on already means in the
// table below it; Disable turns one off; Reveal and Copy take key material
// out of hiding, one onto the screen and one into the clipboard. None of
// those are destructive enough to be red, which is why they had no colour —
// amber is the one added for them.
func TestKeysToolbarColours(t *testing.T) {
	for _, c := range []struct{ label, cls string }{
		{"Enable", "ok"},
		{"Disable", "warn"},
		{"Reveal", "warn"},
		{"Copy", "warn"},
		{"Delete", "danger"},
	} {
		want := "{ label:'" + c.label + "',"
		i := strings.Index(indexHTML, want)
		if i < 0 {
			t.Errorf("the Keys toolbar has no %q button", c.label)
			continue
		}
		spec := indexHTML[i : i+120]
		if !strings.Contains(spec, "cls:'"+c.cls+"'") {
			t.Errorf("%s is not %s-coloured: %s", c.label, c.cls, spec)
		}
	}
}

// The two Clear buttons are deliberately different colours. The captured
// packets can be captured again and clearing them asks nothing; the log file
// cannot be got back and clearing it asks first. Pinned because "make the two
// Clears match" is exactly the tidy-looking change that would erase the
// distinction.
func TestTheTwoClearButtonsDiffer(t *testing.T) {
	if !strings.Contains(indexHTML, "{ label:'Clear', cls:'danger', title:'clear the log file'") {
		t.Error("the log file's Clear is not danger-coloured")
	}
	if !strings.Contains(indexHTML, `class="warn sm">Clear<`) {
		t.Error("the packet buffer's Clear is not warn-coloured")
	}
	if !strings.Contains(indexHTML, "confirmModal('Clear the log file?") {
		t.Error("the log file's Clear no longer confirms, which is what made it the red one")
	}
}

// Amber is defined for both themes, and takes dark ink rather than the #fff
// its siblings use — white on amber measures 2.5:1 on the dark theme, which
// is not readable. A later edit "fixing" the odd one out would undo that.
func TestWarnIsThemedAndTakesDarkInk(t *testing.T) {
	if !strings.Contains(indexHTML, "--warn:#d29922;") {
		t.Error("the dark theme has no --warn")
	}
	if !strings.Contains(indexHTML, "--warn:#bf8700;") {
		t.Error("the light theme has no --warn")
	}
	if !strings.Contains(indexHTML, "button.warn { background:var(--warn); color:#0f1419; }") {
		t.Error("warn buttons no longer take dark ink")
	}
}

// A .warn button sits shoulder to shoulder with .ok and .danger in the Keys
// bar, which are sized explicitly. Left out of that rule it would be the one
// button in the row standing a pixel taller.
func TestWarnSharesTheRowButtonSizing(t *testing.T) {
	if !strings.Contains(indexHTML, "button.sm.danger, button.sm.ok, button.sm.warn {") {
		t.Error("warn is not in the row-button sizing rule")
	}
}

// Download is blue in Logs because it is blue in Config History, which had
// this same ghost problem and answered it with the plain accent. A second
// scheme for the same label in a different card would be worse than either
// one applied consistently.
func TestDownloadKeepsTheAccentEverywhere(t *testing.T) {
	if !strings.Contains(indexHTML, "{ label:'Download', cls:'', title:'download the log file'") {
		t.Error("the log's Download is not accent-coloured")
	}
	if !strings.Contains(indexHTML, "{ label:'Download', cls:'', right:true, title:'download the ticked snapshot") {
		t.Error("config history's Download changed colour, so the two no longer agree")
	}
	if !strings.Contains(indexHTML, `class="sm">Download<`) {
		t.Error("packet capture's Download is not accent-coloured")
	}
}
