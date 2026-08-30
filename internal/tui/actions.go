package tui

// The dispatch layer between a keystroke and a mutation: 'a' opens a
// section's add form (if it has one), 'e' opens edit or runs the row's
// primary action, 'd' deletes with confirmation, space toggles.
//
// Registered per section in sectionActions (actions_mesh.go and friends, one
// file per rail group as the coverage grows), the same dispatch-table shape
// pageBuilders already uses for reads — a section with nothing registered
// here simply has no add/edit/delete/toggle, and the footer legend reflects
// exactly that rather than showing a key that would do nothing.

// rowAction is one thing that can be done to a selected row. Exactly one of
// edit or run is set: edit opens a form (for anything that collects values),
// run performs the action immediately, optionally behind a confirm dialog
// (for delete/unban/restart — anything where undoing a keystroke is more
// expensive than pressing one more key to avoid it).
type rowAction struct {
	label   string
	confirm string // non-empty: ask this before running (with run, never with edit)
	edit    func(m *model, row selRow) formSpec
	run     func(m *model, row selRow) mutationResult
}

// sectionActionSet is what one section registers: an add form (if any) and a
// function computing the row actions available for whichever row the cursor
// is currently on — a function rather than a flat map because which actions
// apply can depend on the row itself (Firewall's rule rows support move;
// its exemption rows don't; a disabled row's toggle reads "enable" instead
// of "disable").
type sectionActionSet struct {
	add func(m *model) formSpec
	row func(m *model, row selRow) map[rune]rowAction
}

// sectionActions is the registry. Keyed by section, the same key nav.go and
// pageBuilders use.
var sectionActions = map[string]sectionActionSet{}

// registerActions is called from each actions_*.go file's init, so the
// registry is built the same additive way pageBuilders is, and adding a
// group's actions never means editing a giant literal in this file.
func registerActions(section string, set sectionActionSet) {
	sectionActions[section] = set
}

// dispatchAdd opens the current section's add form, if it has one.
func (m *model) dispatchAdd() {
	set, ok := sectionActions[m.section]
	if !ok || set.add == nil {
		m.flash = "nothing to add here"
		return
	}
	m.openForm(set.add(m))
}

// dispatchRowAction runs whichever action key is bound to r on the currently
// selected row. 'e' and 'd' are pinned to specific slots (edit, delete) by
// convention so the footer legend can name them without asking every
// section what its own keys mean; space is the one truly per-section slot
// (usually a toggle, but not always — Bans has no toggle, so space simply
// does nothing there, which dispatchRowAction reports rather than silently
// ignoring).
func (m *model) dispatchRowAction(r rune) {
	set, ok := sectionActions[m.section]
	if !ok || set.row == nil {
		m.flash = "no actions on this page"
		return
	}
	row, ok := m.selectedRow()
	if !ok {
		m.flash = "nothing selected"
		return
	}
	actions := set.row(m, row)
	act, ok := actions[r]
	if !ok {
		m.flash = "no such action here"
		return
	}
	switch {
	case act.edit != nil:
		m.openForm(act.edit(m, row))
	case act.confirm != "":
		msg := act.confirm
		m.openConfirm(msg, func(m *model) mutationResult { return act.run(m, row) })
	case act.run != nil:
		m.showResult(act.run(m, row))
	}
}

// actionLegendSegments builds the footer's per-row action hint for the
// current selection as underline-ready segments — only the actions that
// actually apply to the row under the cursor right now, so the footer never
// advertises a key that would report "no such action here" if pressed.
//
// A row action's key is fixed by convention (e always edits, d always
// deletes or its page's equivalent — ban, restore, remove) rather than
// derived from its label, and the two frequently don't share a first
// letter: Peers' 'e' opens "notes", not "edit". footerKeySegment handles
// that split on its own — merging the key into the label when they do align
// (Networks' 'e' opens "edit"), showing the key as its own underlined token
// when they don't.
func (m *model) actionLegendSegments() []footerSegment {
	set, ok := sectionActions[m.section]
	if !ok || set.row == nil {
		return nil
	}
	row, ok := m.selectedRow()
	if !ok {
		return nil
	}
	actions := set.row(m, row)
	if len(actions) == 0 {
		return nil
	}
	// Fixed order rather than map iteration order, so the legend doesn't
	// visibly shuffle itself between frames.
	var segs []footerSegment
	for _, k := range []rune{'e', 'E', ' ', 'd'} {
		act, ok := actions[k]
		if !ok {
			continue
		}
		keyName := string(k)
		if k == ' ' {
			keyName = "space"
		}
		segs = append(segs, footerKeySegment(keyName, act.label))
	}
	return segs
}
