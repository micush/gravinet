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

// A .warn button sits shoulder to shoulder with .ok, .danger and — in the
// Packet Capture and Logs bars — with buttons carrying no colour at all. It
// must be the same height as every one of them.
//
// v971 arranged that by naming .warn in a rule alongside .danger and .ok.
// That covered the coloured neighbours and missed the plain ones, which is
// what left Clear standing two pixels taller than Start and Download. The
// height now comes from .sm, so what this checks is the other half of it:
// that .warn contributes colour and nothing else, and so cannot drift from
// its neighbours whatever they are wearing.
func TestWarnSharesTheRowButtonSizing(t *testing.T) {
	css := stripCSSComments(indexHTML)
	if !strings.Contains(css, "button.warn { background:var(--warn); color:#0f1419; }") {
		t.Error("the warn rule is no longer colour-only; it may now size itself")
	}
	if !strings.Contains(css, "button.sm { height:25px;") {
		t.Error("small buttons no longer share one height, so warn cannot inherit it")
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

// One height for every small button, whichever colour it wears or does not.
//
// The sizing used to hang off button.sm.danger, .ok and .warn, so a small
// button with no colour class kept an auto height about two pixels shorter.
// That was invisible while coloured buttons only appeared in table row bars,
// where every sibling has a colour. Filling in the Keys, Packet Capture and
// Logs toolbars put them next to plain ones — Start and Download flanking an
// amber Clear — and three toolbars ended up with buttons at two heights.
//
// Pinned as the absence of the per-colour rule rather than only the presence
// of the shared one, because the way this comes back is somebody adding a
// fourth colour and giving it its own sizing beside the others.
func TestEverySmallButtonIsOneHeight(t *testing.T) {
	css := stripCSSComments(indexHTML)
	if !strings.Contains(css, "button.sm { height:25px;") {
		t.Error("button.sm does not set its own height; small buttons will size to their content")
	}
	// Comments are stripped first because the rules being forbidden here are
	// also named in the prose above them, explaining why they went away.
	for _, sel := range []string{
		"button.sm.danger", "button.sm.ok", "button.sm.warn", "button.sm.info",
	} {
		if strings.Contains(css, sel) {
			t.Errorf("%s sizes itself separately: a small button without a colour class "+
				"will not match its coloured neighbours", sel)
		}
	}
}

// stripCSSComments removes /* ... */ blocks so a test can assert on the rules
// this stylesheet actually has rather than on what its comments talk about.
func stripCSSComments(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		j := strings.Index(s[i:], "*/")
		if j < 0 {
			return b.String()
		}
		s = s[i+j+2:]
	}
}

// The toolbars where the mismatch actually showed. Each mixes buttons that
// carry a colour with buttons that do not, which is the arrangement the old
// per-colour sizing could not handle — and the arrangement that any future
// toolbar is most likely to reach for.
func TestToolbarsMixColouredAndPlainButtons(t *testing.T) {
	// Packet Capture builds its bar by hand: Start and Download plain,
	// Clear amber, all three .sm.
	for _, s := range []string{
		`class="sm">Start<`, `class="warn sm">Clear<`, `class="sm">Download<`,
	} {
		if !strings.Contains(indexHTML, s) {
			t.Errorf("the packet capture toolbar no longer has %s", s)
		}
	}
	// Logs goes through _rowButtons: three plain, one red.
	if !strings.Contains(indexHTML, "{ label:'Clear', cls:'danger'") {
		t.Error("the logs Clear button is no longer red")
	}
	if !strings.Contains(indexHTML, "{ label:'Refresh', cls:''") {
		t.Error("the logs Refresh button is no longer plain")
	}
}
