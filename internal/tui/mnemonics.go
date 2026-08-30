package tui

// Assigning a mnemonic to every directly-editable field on a page, and
// looking one up when a key is pressed.
//
// This is deliberately scoped to kv rows — a page's top-level scalar
// settings (Settings itself, a hostname, an enabled flag) — and not to
// table rows. A table can hold an unbounded number of rows (fifty firewall
// rules is unremarkable), and there is no alphabet long enough to give each
// one its own permanent letter; those already have the faster navigation
// this scope doesn't need to duplicate — move the cursor, press e. A page's
// named settings are a small, fixed set that fits on screen at once, which
// is exactly the situation a one-keystroke jump earns its keep in.

// reservedKeys are single-rune bindings that mean the same thing on every
// page, regardless of what's on screen — the row-action keys (actions.go),
// and every plain navigation/command key handleRune already switches on.
// Mnemonics are never assigned any of these, so a key's meaning never
// depends on which page happens to be showing: 'e' is always "edit the
// selected row" or nothing, never sometimes that and sometimes "jump to the
// expires field."
var reservedKeys = map[rune]bool{
	'a': true, 'e': true, 'd': true, ' ': true, 'E': true, // row actions
	'q': true, 'j': true, 'k': true, 'g': true, 'G': true, // movement/quit
	'r': true, 't': true, 'n': true, 'N': true, '?': true, '/': true, // commands
}

// assignMnemonicsInPlace walks cards in the same order layout() renders
// them and gives every kv row with a non-nil edit a unique mnemonic,
// writing it into that row's own mnemonic field — see kvRow's comment for
// why mutating in place, rather than a side table, is both simpler and
// exactly as correct here.
//
// Assignment prefers a letter already in the row's own label (the first of
// its own letters that's still free), so the underline usually falls on a
// letter the label already reads as its natural first initial — "keepalive"
// gets 'k' before it gets anything else. Only once no letter in the label
// itself is free does a row fall back to the next open letter from a fixed
// pool, rendered as a "[x] " prefix by layoutKV rather than an underline
// with nothing under it.
//
// This is called fresh every time it's needed — before a page is laid out,
// and before a keypress is matched against it — never cached, for the same
// reason nothing else derived from buildPage's output is cached: cards is
// rebuilt fresh every time anyway, and assignment over a rebuilt slice can
// never disagree with what was just drawn.
func assignMnemonicsInPlace(cards []card) {
	used := map[rune]bool{}
	for r := range reservedKeys {
		used[r] = true
	}
	for ci := range cards {
		for ii := range cards[ci].items {
			t, ok := cards[ci].items[ii].(editableKV)
			if !ok {
				continue
			}
			for ri := range t.rows {
				if t.rows[ri].edit == nil {
					continue
				}
				m := pickMnemonic(t.rows[ri].k, used)
				t.rows[ri].mnemonic = m
				used[m] = true
			}
		}
	}
}

// mnemonicFallbackPool is scanned only once a row's own label has nothing
// free left in it — lowercase first (matching what a label's own letters
// would produce, so a fallback doesn't visually stand out as a different
// kind of mnemonic), then uppercase.
const mnemonicFallbackPool = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// pickMnemonic chooses one unused rune for label, preferring one of the
// label's own letters over the fallback pool, and marks nothing used
// itself — the caller does that, once it has committed to the choice.
func pickMnemonic(label string, used map[rune]bool) rune {
	for _, r := range label {
		lr := unicodeToLower(r)
		if lr < 'a' || lr > 'z' {
			continue // only letters make sensible one-key mnemonics
		}
		if !used[lr] {
			return lr
		}
	}
	for _, r := range mnemonicFallbackPool {
		if !used[r] {
			return r
		}
	}
	// The fallback pool is 62 characters; a page with more than 62
	// individually mnemonic-worthy settings does not exist today and
	// should be split into more pages before it does. Rather than assign a
	// colliding key silently, mark this row as having no mnemonic at all —
	// layoutKV's fallback-prefix rendering only fires for edit != nil rows
	// with a nonzero mnemonic, so a zero value here just means this one
	// field is reachable by scrolling to it, not by a shortcut.
	return 0
}

// mnemonicAction finds the kv row on the current page whose mnemonic is r,
// and returns the form it opens, if any. Recomputes cards and assignment
// fresh — see assignMnemonicsInPlace's own comment on why that's the right
// call here rather than a cache.
func (m *model) mnemonicAction(r rune) (formSpec, bool) {
	if reservedKeys[r] {
		return formSpec{}, false
	}
	cards := m.currentCards()
	assignMnemonicsInPlace(cards)
	for _, cd := range cards {
		for _, it := range cd.items {
			t, ok := it.(editableKV)
			if !ok {
				continue
			}
			for _, row := range t.rows {
				if row.edit != nil && row.mnemonic == r {
					return row.edit(m), true
				}
			}
		}
	}
	return formSpec{}, false
}
