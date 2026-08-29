package tui

import (
	"strings"
	"testing"
)

func TestFormTextFieldAppendAndBackspace(t *testing.T) {
	m := testModel()
	var submitted map[string]string
	m.openForm(formSpec{
		title: "t",
		fields: []formField{
			{key: "name", label: "name", kind: fieldText},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			submitted = v
			return mutationResult{ok: true, detail: "done"}
		},
	})
	for _, r := range "corp" {
		m.handleKey(key{t: keyRune, r: r})
	}
	m.handleKey(key{t: keyBackspace})
	m.handleKey(key{t: keyRune, r: 'q'}) // 'q' must NOT quit while a form is open
	if !m.modalOpen() {
		t.Fatal("'q' closed the console instead of being typed into the field")
	}
	m.handleKey(key{t: keyEnter})
	if submitted == nil {
		t.Fatal("submit was never called")
	}
	if submitted["name"] != "corq" {
		t.Errorf("field value = %q, want %q", submitted["name"], "corq")
	}
}

func TestFormBoolFieldToggles(t *testing.T) {
	m := testModel()
	var submitted map[string]string
	m.openForm(formSpec{
		title:  "t",
		fields: []formField{{key: "on", label: "on", kind: fieldBool, value: "false"}},
		submit: func(m *model, v map[string]string) mutationResult {
			submitted = v
			return mutationResult{ok: true}
		},
	})
	m.handleKey(key{t: keyRune, r: ' '})
	m.handleKey(key{t: keyEnter})
	if submitted["on"] != "true" {
		t.Errorf("bool field after one toggle = %q, want true", submitted["on"])
	}
}

func TestFormSelectFieldCycles(t *testing.T) {
	m := testModel()
	var submitted map[string]string
	m.openForm(formSpec{
		title: "t",
		fields: []formField{
			{key: "mode", label: "mode", kind: fieldSelect, value: "full", options: []string{"full", "partial"}},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			submitted = v
			return mutationResult{ok: true}
		},
	})
	m.handleKey(key{t: keyRight})
	m.handleKey(key{t: keyEnter})
	if submitted["mode"] != "partial" {
		t.Errorf("select after one cycle = %q, want partial", submitted["mode"])
	}
}

func TestFormSelectFieldWrapsAround(t *testing.T) {
	m := testModel()
	var submitted map[string]string
	m.openForm(formSpec{
		title:  "t",
		fields: []formField{{key: "x", label: "x", kind: fieldSelect, value: "a", options: []string{"a", "b"}}},
		submit: func(m *model, v map[string]string) mutationResult { submitted = v; return mutationResult{ok: true} },
	})
	m.handleKey(key{t: keyLeft}) // one step left from "a" should wrap to "b"
	m.handleKey(key{t: keyEnter})
	if submitted["x"] != "b" {
		t.Errorf("wrapped select = %q, want b", submitted["x"])
	}
}

func TestFormTabMovesBetweenFields(t *testing.T) {
	m := testModel()
	var submitted map[string]string
	m.openForm(formSpec{
		title: "t",
		fields: []formField{
			{key: "a", label: "a", kind: fieldText},
			{key: "b", label: "b", kind: fieldText},
		},
		submit: func(m *model, v map[string]string) mutationResult { submitted = v; return mutationResult{ok: true} },
	})
	m.handleKey(key{t: keyRune, r: '1'}) // into field a
	m.handleKey(key{t: keyTab})
	m.handleKey(key{t: keyRune, r: '2'}) // into field b
	m.handleKey(key{t: keyEnter})
	if submitted["a"] != "1" || submitted["b"] != "2" {
		t.Errorf("submitted = %+v", submitted)
	}
}

func TestFormEscCancelsWithoutSubmitting(t *testing.T) {
	m := testModel()
	called := false
	m.openForm(formSpec{
		title:  "t",
		fields: []formField{{key: "a", label: "a", kind: fieldText}},
		submit: func(m *model, v map[string]string) mutationResult { called = true; return mutationResult{ok: true} },
	})
	m.handleKey(key{t: keyRune, r: 'x'})
	m.handleKey(key{t: keyEsc})
	if called {
		t.Error("submit ran after Esc")
	}
	if m.modalOpen() {
		t.Error("the form is still open after Esc")
	}
}

func TestFormCtrlCQuitsEvenWithAFormOpen(t *testing.T) {
	// ISIG is cleared at the terminal layer, so Ctrl-C only ever reaches the
	// console as a key — every input mode has to honor it as quit, a form
	// included, or there is no way out of a stuck session at all.
	m := testModel()
	m.openForm(formSpec{title: "t", fields: []formField{{key: "a", kind: fieldText}}, submit: nil})
	if m.handleKey(key{t: keyCtrlC}) {
		t.Error("ctrl-c did not quit with a form open")
	}
}

func TestSuccessfulSubmitRefreshesAndShowsAResult(t *testing.T) {
	m := testModel()
	before := m.snap
	m.openForm(formSpec{
		title:  "t",
		fields: []formField{{key: "a", kind: fieldText}},
		submit: func(m *model, v map[string]string) mutationResult {
			return mutationResult{ok: true, detail: "added network corp"}
		},
	})
	m.handleKey(key{t: keyEnter})
	if m.form != nil {
		t.Error("the form should have closed")
	}
	if m.result == nil || !m.result.ok {
		t.Fatalf("expected a successful result, got %+v", m.result)
	}
	if m.snap == before {
		t.Error("a successful mutation should have triggered a refresh (a new snapshot)")
	}
}

func TestFailedSubmitDoesNotRefresh(t *testing.T) {
	m := testModel()
	before := m.snap
	m.openForm(formSpec{
		title:  "t",
		fields: []formField{{key: "a", kind: fieldText}},
		submit: func(m *model, v map[string]string) mutationResult {
			return mutationResult{ok: false, detail: "no network named \"x\""}
		},
	})
	m.handleKey(key{t: keyEnter})
	if m.result == nil || m.result.ok {
		t.Fatalf("expected a failed result, got %+v", m.result)
	}
	if m.snap != before {
		t.Error("a failed mutation should not have refreshed — nothing changed")
	}
}

func TestResultDismissesOnAnyKey(t *testing.T) {
	m := testModel()
	m.result = &resultState{ok: true, lines: []string{"ok"}}
	m.handleKey(key{t: keyRune, r: 'z'})
	if m.result != nil {
		t.Error("the result should have dismissed on an arbitrary key")
	}
}

func TestConfirmRunsOnYAndCancelsOnN(t *testing.T) {
	m := testModel()
	ran := false
	m.openConfirm("delete corp?", func(m *model) mutationResult {
		ran = true
		return mutationResult{ok: true, detail: "deleted"}
	})
	m.handleKey(key{t: keyRune, r: 'n'})
	if ran {
		t.Fatal("'n' should not have run the action")
	}
	if m.confirm != nil {
		t.Error("'n' should have closed the confirm dialog")
	}

	ran2 := false
	m.openConfirm("delete corp?", func(m *model) mutationResult {
		ran2 = true
		return mutationResult{ok: true, detail: "deleted"}
	})
	m.handleKey(key{t: keyRune, r: 'y'})
	if !ran2 {
		t.Error("'y' should have run the action")
	}
	if m.result == nil {
		t.Error("confirming should have produced a result")
	}
}

func TestConfirmEscCancelsWithoutRunning(t *testing.T) {
	m := testModel()
	ran := false
	m.openConfirm("delete corp?", func(m *model) mutationResult { ran = true; return mutationResult{ok: true} })
	m.handleKey(key{t: keyEsc})
	if ran || m.confirm != nil {
		t.Error("esc should cancel without running the action")
	}
}

func TestOpeningAModalClosesAnyOther(t *testing.T) {
	m := testModel()
	m.openForm(formSpec{title: "a"})
	m.openConfirm("b?", nil)
	if m.form != nil {
		t.Error("opening a confirm should have closed the form")
	}
	m.result = &resultState{ok: true}
	m.openForm(formSpec{title: "c"})
	if m.result != nil {
		t.Error("opening a form should have closed a result")
	}
}

func TestModalInputIsRoutedAheadOfOrdinaryPageKeys(t *testing.T) {
	// While a form is open, 'r' must be typed into the focused field, not
	// trigger a refresh; 'q' must be typed, not quit; arrows must move
	// within the form, not the rail or the page. This is the single dispatch
	// check (modalOpen() first in handleKey) that guarantees all of that at
	// once, so one test on the general property stands in for repeating it
	// per key.
	m := testModel()
	startSection := m.section
	m.openForm(formSpec{title: "t", fields: []formField{{key: "a", kind: fieldText}}})
	for _, k := range []key{{t: keyRune, r: 'r'}, {t: keyRune, r: 't'}, {t: keyRune, r: '?'}, {t: keyUp}, {t: keyDown}} {
		m.handleKey(k)
	}
	if m.section != startSection {
		t.Errorf("a page navigation key leaked through the open form; section = %q", m.section)
	}
	if !m.modalOpen() {
		t.Error("the form closed on its own from ordinary typing")
	}
}

func TestDrawFormRendersFieldsAndDoesNotPanicAtSmallSizes(t *testing.T) {
	m := testModel()
	m.openForm(formSpec{
		title: "add network",
		fields: []formField{
			{key: "name", label: "name", kind: fieldText, value: "corp"},
			{key: "relay", label: "allow relay", kind: fieldBool, value: "true"},
			{key: "mode", label: "mode", kind: fieldSelect, value: "full", options: []string{"full", "partial"}},
		},
	})
	for _, sz := range [][2]int{{10, 5}, {40, 12}, {120, 40}} {
		out := drawText(m, sz[0], sz[1])
		if sz[0] >= 40 && !strings.Contains(out, "ADD NETWORK") {
			t.Errorf("size %v: form title missing:\n%s", sz, out)
		}
	}
}
