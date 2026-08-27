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

// The Keys bar is down to the two buttons that act on a selection of rows,
// and only one of them carries a colour. Generate fills ticked empty slots
// and is an ordinary action; Delete empties them for good and is red.
//
// Reveal and Copy used to be the amber pair here — key material leaving its
// hiding place, onto the screen and into the clipboard. Both are gone, along
// with Import, because a double-click on the key cell now reveals the key
// into a selected input: copy it from there, or paste a new one over it.
// TestKeysHasNoRowButtonForACellsJob is what keeps them gone.
func TestKeysToolbarColours(t *testing.T) {
	for _, c := range []struct{ label, cls string }{
		{"Generate", ""},
		{"Delete", "danger"},
	} {
		want := "{ label:'" + c.label + "',"
		i := strings.Index(indexHTML, want)
		if i < 0 {
			t.Errorf("the Keys toolbar has no %q button", c.label)
			continue
		}
		// Bounded at the entry's own closing brace rather than a fixed
		// window: Generate is the shortest spec in the app and a 120-char
		// slice from it runs into Delete's cls:'danger' on the next line.
		spec := indexHTML[i:]
		if end := strings.Index(spec, "},"); end >= 0 && end < 200 {
			spec = spec[:end]
		} else {
			spec = spec[:200]
		}
		if c.cls == "" {
			// Generate carries no cls at all rather than cls:'' — the accent
			// is the default, and the spec goes straight to title.
			if strings.Contains(spec, "cls:'") {
				t.Errorf("Generate picked up a colour class: %s", spec)
			}
			continue
		}
		if !strings.Contains(spec, "cls:'"+c.cls+"'") {
			t.Errorf("%s is not %s-coloured: %s", c.label, c.cls, spec)
		}
	}
}

// Reveal, Copy and Import were row buttons for something a row can do to
// itself, which is the argument that took Enable and Disable off this same
// toolbar in v974. The key cell reveals into an editable, pre-selected input
// on double-click; that one gesture is copy, replace and import at once, and
// on an empty slot it is import alone.
//
// Pinned as the absence of the buttons and the presence of the cell that
// replaced them, because removing the buttons is only a cleanup for as long
// as the cell still does their job.
func TestKeysHasNoRowButtonForACellsJob(t *testing.T) {
	for _, label := range []string{"Reveal", "Copy", "Import"} {
		if strings.Contains(indexHTML, "{ label:'"+label+"',") {
			t.Errorf("the Keys toolbar has a %s button again; the key cell does that on double-click", label)
		}
	}
	// The filled cell announces itself as revealable, and marks itself filled
	// so the handler knows to fetch a key before opening the editor.
	if !strings.Contains(indexHTML, `data-set="1" title="double-click to reveal, then copy it or paste a new one over it"`) {
		t.Error("the key cell no longer offers reveal-and-replace, and nothing else reveals or copies a key now")
	}
	// The empty cell is the import path: same handler, nothing to reveal.
	if !strings.Contains(indexHTML, `data-set="0" title="double-click to paste a key in"`) {
		t.Error("an empty slot no longer takes a pasted key, and nothing else imports one now")
	}
	// navigator.clipboard is deliberately not the copy path: this admin page
	// is routinely served over plain http, where that API does not exist.
	if strings.Contains(indexHTML, "navigator.clipboard.writeText") && strings.Contains(indexHTML, "Copy the selected key") {
		t.Error("the old clipboard copy and its window.prompt fallback are back")
	}
}

// Enablement belongs to the state cell, which is how every other table in the
// app does it — double-click the state to flip one row. Keys was the only one
// that also had buttons for it, so it was the only table offering two ways to
// do the same thing.
//
// The state cell itself is unchanged and still carries the toggle; what went
// away is the second route to it.
func TestKeysHasNoEnableDisableButtons(t *testing.T) {
	for _, label := range []string{"Enable", "Disable"} {
		if strings.Contains(indexHTML, "{ label:'"+label+"',") {
			t.Errorf("the Keys toolbar has a %s button again; enablement is the state cell's job", label)
		}
	}
	if !strings.Contains(indexHTML, `<td class="kstate" data-slot="'+k.slot+'" data-en="'+(k.enabled?1:0)+'" title="double-click to toggle">`) {
		t.Error("the key state cell no longer toggles, and nothing else enables a key now")
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
