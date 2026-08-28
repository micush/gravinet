package webadmin

import (
	"strings"
	"testing"
)

// A settings row's bottom border is a separator between rows. The last row in
// a card must not draw one, or the card ends on a rule with nothing under it.
//
// :last-child alone was not enough, and the gap is easy to reintroduce: it
// stops matching the moment anything follows the final row, which several
// cards do — Config history puts a snapshot count under its only row, and the
// dynamic DNS card puts the last run's outcome under its last row. Both drew a
// line across the bottom of the card between the row and the note.
func TestLastSettingsRowHasNoDanglingSeparator(t *testing.T) {
	css := between(t, indexHTML, ".settings-row {", "</style>")
	if !strings.Contains(css, ".settings-row:not(:has(~ .settings-row)) { border-bottom:0; }") {
		t.Error("a row with no following row still draws a separator, so any card ending in a note has a rule hanging across it")
	}
	// Kept as well, not replaced: it is the case that does not need :has().
	if !strings.Contains(css, ".settings-row:last-child { border-bottom:0; }") {
		t.Error(":last-child was dropped; the common case now depends on :has() alone")
	}
}

// The cards that provoked it, so a future edit that moves the note back above
// the rows — or adds a third such card — is still covered by the rule above
// rather than by luck.
func TestCardsThatEndWithANoteAfterTheirLastRow(t *testing.T) {
	// The dynamic DNS card was here too until v1000, when its last-run report
	// moved to the log. Its last row is now genuinely last, so :last-child
	// covers it — which is why the rule below is not the only thing keeping
	// that card tidy, and why this list is allowed to shrink.
	for _, c := range []struct{ name, sec, lastRow, note string }{
		{"config history", "function secSettingsGeneral(c)", "config-history-limit-row", "Currently holding "},
	} {
		body := between(t, indexHTML, c.sec, "\nfunction secSettingsSecurity")
		row := strings.Index(body, c.lastRow)
		note := strings.Index(body, c.note)
		if row < 0 || note < 0 {
			t.Errorf("%s: could not find the row and its note", c.name)
			continue
		}
		if note < row {
			t.Errorf("%s: the note moved above the row; harmless, but the separator rule is what makes either order look right", c.name)
		}
	}
}

// A settings row must carry a label, because the label is the only part of it
// that survives help mode being off — which is the default.
//
// Config history shipped with a description and no label, so the card rendered
// as a bare number box with nothing saying what the number governed. The shape
// that causes it is a label block whose first child is the description, and
// that shape is what this looks for: it is cheap to write by accident and
// invisible to anyone testing with help switched on.
func TestEverySettingsRowHasALabel(t *testing.T) {
	if strings.Contains(indexHTML, `$('<div><div class="settings-desc"`) {
		t.Error("a settings row's label block starts with its description, so the row shows nothing with help mode off")
	}
	// The row that had the bug, named so the fix is not undone by a rewrite
	// that happens to avoid the pattern above.
	sec := between(t, indexHTML, "function secSettingsGeneral(c)", "\nfunction secSettingsSecurity")
	chunk := between(t, sec, "config-history-limit-row", "chRow.appendChild")
	if !strings.Contains(chunk, `class="settings-label"`) {
		t.Error("the config history row has no label again")
	}
}

// Live status uses .hint and never .settings-desc.
//
// The two look identical with help mode on, which is how four lines of the
// dynamic DNS card — the loading line, the read error, what it would register,
// and what the last run did — shipped hidden by default in v992. The CSS
// comment on .settings-desc already states the rule; this is the part that
// notices when it is broken.
func TestLiveStatusIsNotMarkedAsHelpText(t *testing.T) {
	sec := between(t, indexHTML, "Dynamic DNS registration</h3>", "\nfunction secSettingsSecurity")
	for _, live := range []string{
		`$('<div><div class="hint">loading`,
		`<div class="hint">could not read this node`,
		"const facts = $('<div class=\"hint\"",
	} {
		if !strings.Contains(sec, live) {
			t.Errorf("live status is not using .hint: %s", live)
		}
	}
	// The snapshot count on the neighbouring card, same mistake, same fix.
	gen := between(t, indexHTML, "Config history</h3>", "Dynamic DNS registration</h3>")
	if !strings.Contains(gen, `<div class="hint" style="margin:10px 0 0">Currently holding `) {
		t.Error("the snapshot count is hidden with help mode off")
	}
}

// A placeholder is clipped at the width of its box, so it has to be short
// enough to finish. This field's said "none — updates are sent unsigned" in a
// 260px box and arrived as "none — updates are sent unsig": a sentence that
// stops mid-word says less than the single word it was reaching for.
//
// Still guarded now that the box is only a placeholder in the unset case,
// because that is the case the bug was in.
func TestTSIGPlaceholderFitsItsBox(t *testing.T) {
	sec := between(t, indexHTML, "Dynamic DNS registration</h3>", "\nfunction secSettingsSecurity")
	if !strings.Contains(sec, "inp.placeholder = 'unsigned'") {
		t.Error("the unset TSIG field no longer reads 'unsigned'")
	}
	// Roughly 29 characters fit. Anything approaching that is a sentence that
	// will be cut, which is the failure this replaced.
	for _, lit := range []string{"updates are sent unsigned", "type to replace it"} {
		if strings.Contains(sec, `placeholder="`+lit) || strings.Contains(sec, "placeholder = '"+lit) {
			t.Errorf("a sentence is back in the placeholder: %q — it belongs in the description", lit)
		}
	}
	// And the explanation it displaced is in the description, which has room.
	if !strings.Contains(sec, "Click the dots to reveal the key") {
		t.Error("the description no longer explains how to read the key back")
	}
}
