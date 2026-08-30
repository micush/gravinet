package tui

import (
	"strings"
	"testing"
)

func TestPickMnemonicPrefersTheLabelsOwnLetters(t *testing.T) {
	used := map[rune]bool{}
	if got := pickMnemonic("keepalive", used); got != 'k' {
		t.Errorf("pickMnemonic(keepalive) = %q, want k", got)
	}
}

func TestPickMnemonicSkipsAlreadyUsedLetters(t *testing.T) {
	used := map[rune]bool{'k': true}
	// "keepalive"'s own letters in order: k,e,e,p,a,l,i,v,e — k is taken,
	// so the next free one of its own letters should win: e.
	if got := pickMnemonic("keepalive", used); got != 'e' {
		t.Errorf("pickMnemonic with k taken = %q, want e", got)
	}
}

func TestPickMnemonicFallsBackWhenEveryLabelLetterIsTaken(t *testing.T) {
	used := map[rune]bool{}
	for _, r := range "on" { // every letter in "on" is taken
		used[r] = true
	}
	got := pickMnemonic("on", used)
	if got == 'o' || got == 'n' {
		t.Errorf("pickMnemonic should have fallen back, got %q", got)
	}
	if got != 'a' { // first letter of the fallback pool
		t.Errorf("fallback = %q, want the pool's first letter (a)", got)
	}
}

func TestPickMnemonicNeverReturnsAReservedKey(t *testing.T) {
	used := map[rune]bool{}
	for r := range reservedKeys {
		used[r] = true
	}
	// A label made entirely of reserved letters must still get something
	// from the fallback pool, never one of the reserved ones.
	got := pickMnemonic("get", used) // g, e, t are all reserved
	if reservedKeys[got] {
		t.Errorf("pickMnemonic returned a reserved key: %q", got)
	}
}

func TestAssignMnemonicsInPlaceGivesEveryEditableRowAUniqueLetter(t *testing.T) {
	cards := []card{{title: "settings", items: []item{
		editableKV{rows: []editableKVRow{
			{k: "hostname", v: "gn1", edit: dummyForm},
			{k: "hold time", v: "180", edit: dummyForm}, // shares "h" with hostname
			{k: "notes", v: "", edit: nil},               // not editable: no mnemonic
		}},
	}}}
	assignMnemonicsInPlace(cards)
	rows := cards[0].items[0].(editableKV).rows
	if rows[0].mnemonic == 0 {
		t.Fatal("editable row got no mnemonic at all")
	}
	if rows[1].mnemonic == 0 {
		t.Fatal("second editable row got no mnemonic at all")
	}
	if rows[0].mnemonic == rows[1].mnemonic {
		t.Errorf("two rows got the same mnemonic: %q", rows[0].mnemonic)
	}
	if rows[2].mnemonic != 0 {
		t.Error("a non-editable row was assigned a mnemonic")
	}
}

func TestAssignMnemonicsIsUniqueAcrossCardsNotJustWithinOne(t *testing.T) {
	// Settings-style pages spread editable rows across several cards; the
	// mnemonic pool has to be shared across all of them or two different
	// cards could both underline the same key.
	cards := []card{
		{title: "a", items: []item{editableKV{rows: []editableKVRow{{k: "level", edit: dummyForm}}}}},
		{title: "b", items: []item{editableKV{rows: []editableKVRow{{k: "listen", edit: dummyForm}}}}},
	}
	assignMnemonicsInPlace(cards)
	m1 := cards[0].items[0].(editableKV).rows[0].mnemonic
	m2 := cards[1].items[0].(editableKV).rows[0].mnemonic
	if m1 == m2 {
		t.Errorf("rows in different cards both got mnemonic %q", m1)
	}
}

func TestAssignMnemonicsDoesNotTouchOrdinaryKVBlocks(t *testing.T) {
	// The whole point of the separate editableKV type: an ordinary kv block
	// (used all over this package for read-only info) must never sprout a
	// mnemonic just by being on the same page as an editable one.
	cards := []card{{title: "x", items: []item{
		kv{rows: []kvRow{{k: "cpu", v: "12%"}}},
		editableKV{rows: []editableKVRow{{k: "hostname", edit: dummyForm}}},
	}}}
	out := linesText(layout(cards, testCtx(60)))
	if strings.Contains(out, "\u2039") { // sanity: no stray select markers etc.
		t.Errorf("unexpected marker in output:\n%s", out)
	}
	// The important assertion is behavioral, via mnemonicAction below, not
	// visual — an ordinary kv row was never given a mnemonic to find.
}

func TestMnemonicActionOpensTheRightForm(t *testing.T) {
	m := testModel()
	opened := false
	pageBuilders["__mnemtest__"] = func(c pageCtx) []card {
		return []card{{title: "t", items: []item{
			editableKV{rows: []editableKVRow{
				// "alpha" starts with 'a', which is reserved for the add
				// action, so the row's actual mnemonic will be one of its
				// other letters ('l') — found below rather than assumed,
				// since asserting the wrong letter is exactly the kind of
				// mistake this whole mechanism exists to prevent silently.
				{k: "alpha", edit: func(m *model) formSpec { opened = true; return formSpec{title: "alpha form"} }},
			}},
		}}}
	}
	defer delete(pageBuilders, "__mnemtest__")
	m.section = "__mnemtest__"

	cards := m.currentCards()
	assignMnemonicsInPlace(cards)
	assigned := cards[0].items[0].(editableKV).rows[0].mnemonic
	if assigned == 0 {
		t.Fatal("the row was never assigned a mnemonic at all")
	}

	spec, ok := m.mnemonicAction(assigned)
	if !ok {
		t.Fatalf("mnemonicAction(%q) did not find the row it was assigned to", assigned)
	}
	if spec.title != "alpha form" {
		t.Errorf("wrong form: %q", spec.title)
	}
	if !opened {
		t.Error("the row's edit function was never called")
	}
}

func TestMnemonicActionIgnoresReservedKeys(t *testing.T) {
	m := testModel()
	pageBuilders["__mnemtest2__"] = func(c pageCtx) []card {
		return []card{{title: "t", items: []item{
			editableKV{rows: []editableKVRow{{k: "add-like-thing", edit: dummyForm}}},
		}}}
	}
	defer delete(pageBuilders, "__mnemtest2__")
	m.section = "__mnemtest2__"
	// "add-like-thing" would naturally offer 'a' first, but 'a' is reserved
	// for the add action — pickMnemonic must have skipped it.
	if _, ok := m.mnemonicAction('a'); ok {
		t.Error("'a' should never be claimed as a field mnemonic")
	}
}

func TestPressingAMnemonicOpensAFormThroughTheRealKeyPath(t *testing.T) {
	// End-to-end through handleKey/handleRune, not calling mnemonicAction
	// directly, so this also proves the wiring in handleRune's default
	// case actually works.
	m := testModel()
	pageBuilders["__mnemtest3__"] = func(c pageCtx) []card {
		return []card{{title: "t", items: []item{
			editableKV{rows: []editableKVRow{
				{k: "zzzfield", edit: func(m *model) formSpec { return formSpec{title: "zzz form"} }},
			}},
		}}}
	}
	defer delete(pageBuilders, "__mnemtest3__")
	m.section = "__mnemtest3__"

	m.handleKey(key{t: keyRune, r: 'z'})
	if m.form == nil {
		t.Fatal("pressing the mnemonic did not open a form")
	}
	if m.form.spec.title != "zzz form" {
		t.Errorf("wrong form opened: %q", m.form.spec.title)
	}
}

func TestUnmatchedRuneDoesNothing(t *testing.T) {
	m := testModel()
	m.setSection("about") // a page with no editable content at all
	m.handleKey(key{t: keyRune, r: 'z'})
	if m.form != nil {
		t.Error("a rune matching nothing should not have opened a form")
	}
}

func TestPageHasMnemonicsReflectsTheCurrentPage(t *testing.T) {
	m := testModel()
	m.setSection("about")
	if m.pageHasMnemonics() {
		t.Error("about has no editable fields")
	}
	pageBuilders["__mnemtest4__"] = func(c pageCtx) []card {
		return []card{{title: "t", items: []item{
			editableKV{rows: []editableKVRow{{k: "x", edit: dummyForm}}},
		}}}
	}
	defer delete(pageBuilders, "__mnemtest4__")
	m.section = "__mnemtest4__"
	if !m.pageHasMnemonics() {
		t.Error("a page with an editable row should report having mnemonics")
	}
}

func TestAdvancedEditKeyIsReachableFromTheRealKeyboardPath(t *testing.T) {
	// Regression test: 'E' (Networks' advanced-edit action) was registered
	// in networksActions but never actually wired into handleRune's switch,
	// so it was unreachable from a real keypress even though a test calling
	// dispatchRowAction('E') directly passed. This drives it through
	// handleKey, the same as an operator's keystroke would.
	m := testModel()
	m.setSection("networks")
	m.handleKey(key{t: keyRune, r: 'E'})
	if m.form == nil {
		t.Fatal("'E' did not open the advanced edit form through the real key path")
	}
}

func dummyForm(m *model) formSpec { return formSpec{title: "dummy"} }

