package tui

// The three small modal states a mutation can put the console into: a form
// to collect fields, a yes/no confirmation before anything destructive, and
// a result screen showing exactly what the shelled-out command (or the
// direct config commit) actually said. All three are mutually exclusive —
// opening one closes whichever of the others was open — and all three are
// dismissed the same conceptual way search is: Esc backs out without
// changing anything, and only Enter (or, for a form, submitting) commits.
//
// None of these decide *what* a mutation does. That lives in actions.go, one
// formSpec or rowAction per entity, built from the exact CLI usage strings
// verified against cmd/gravinet — this file only knows how to collect the
// fields a spec asks for and how to show what came back.

import "strings"

// fieldKind is the shape of one form field.
type fieldKind int

const (
	fieldText   fieldKind = iota // free text, edited by append/backspace — the same simple editing the search box already uses, so a form field behaves the way everything else typed into this console already does
	fieldBool               // toggled with space or ←/→
	fieldSelect              // cycled with ←/→ through a fixed list of options
)

// formField describes one collectible value. value is both the initial
// value (for an edit form, pre-filled from the row being edited) and, once
// the form is open, the field's current live value — formState edits it in
// place as the operator types.
type formField struct {
	key     string
	label   string
	kind    fieldKind
	value   string
	options []string // fieldSelect only
	help    string   // one line, shown under the focused field
}

// formSpec is a complete form: what to collect, and what to do with it.
// submit runs after the operator presses Enter with every field filled in as
// they left it; it is responsible for turning those values into an actual
// mutation (usually a runLeaf call building the right argv, occasionally
// commitConfig — see actions.go) and returning what happened.
type formSpec struct {
	title  string
	fields []formField
	submit func(m *model, values map[string]string) mutationResult
}

// formState is a formSpec in progress: a live copy of its fields (so editing
// never mutates the spec itself, which may be reused) and which one has
// focus.
type formState struct {
	spec  formSpec
	field []formField
	idx   int
}

func newFormState(spec formSpec) *formState {
	fs := &formState{spec: spec, field: append([]formField(nil), spec.fields...)}
	return fs
}

func (f *formState) values() map[string]string {
	out := make(map[string]string, len(f.field))
	for _, fl := range f.field {
		out[fl.key] = fl.value
	}
	return out
}

// confirmState is a pending yes/no question, asked before anything
// destructive (delete, unban, restart, apply an upgrade) actually runs.
type confirmState struct {
	message string
	yes     func(m *model) mutationResult
}

// resultState shows what a completed mutation actually said — a CLI leaf's
// own stdout/stderr, verbatim, or commitConfig's short status line. Full
// text rather than a truncated footer flash, because "added network corp"
// fits either way but "no bootstrap seed is embedded, share one out of
// band…" does not, and paraphrasing a message already written for a human to
// read would only make it worse.
type resultState struct {
	ok    bool
	lines []string
}

// openForm replaces whatever modal is open (if any) with spec, focused on
// its first field.
func (m *model) openForm(spec formSpec) {
	m.confirm, m.result = nil, nil
	m.form = newFormState(spec)
}

// openConfirm asks message, running yes only if the operator says y.
func (m *model) openConfirm(message string, yes func(m *model) mutationResult) {
	m.form, m.result = nil, nil
	m.confirm = &confirmState{message: message, yes: yes}
}

// showResult replaces whatever modal is open with a display of a completed
// mutation's outcome, and — on success — refreshes the snapshot and clears
// every cached lazy read, the same as pressing r, so the page the operator
// is looking at reflects the change immediately rather than on the next
// scheduled poll.
func (m *model) showResult(res mutationResult) {
	m.form, m.confirm = nil, nil
	m.result = &resultState{ok: res.ok, lines: splitLines(res.detail)}
	if res.ok {
		m.refresh()
	}
}

// closeModals dismisses whichever of form/confirm/result is open, with no
// side effect on the config or the daemon — the Esc path.
func (m *model) closeModals() {
	m.form, m.confirm, m.result = nil, nil, nil
}

// modalOpen reports whether any of the three is currently showing, which is
// what tells handleKey to route input here instead of to the page.
func (m *model) modalOpen() bool {
	return m.form != nil || m.confirm != nil || m.result != nil
}

// ---- key handling ---------------------------------------------------------

// handleModalKey is reached instead of the ordinary page/rail handling
// whenever modalOpen() is true. Precedence — result, then confirm, then
// form — matches the order a mutation actually produces them: a form is
// open, submitting may open a confirm (for actions that ask twice — none do
// yet, but the path exists), and either one finishes by opening a result.
func (m *model) handleModalKey(k key) bool {
	switch {
	case m.result != nil:
		return m.handleResultKey(k)
	case m.confirm != nil:
		return m.handleConfirmKey(k)
	case m.form != nil:
		return m.handleFormKey(k)
	}
	return true
}

func (m *model) handleResultKey(k key) bool {
	if k.t == keyCtrlC || k.t == keyCtrlD {
		return false
	}
	// Any other key dismisses. A result is information, not a question —
	// nothing to type, nothing to get wrong by pressing the wrong key.
	m.result = nil
	return true
}

func (m *model) handleConfirmKey(k key) bool {
	switch {
	case k.t == keyCtrlC || k.t == keyCtrlD:
		return false
	case k.t == keyEsc || (k.t == keyRune && (k.r == 'n' || k.r == 'N')):
		m.confirm = nil
	case k.t == keyEnter || (k.t == keyRune && (k.r == 'y' || k.r == 'Y')):
		c := m.confirm
		m.confirm = nil
		m.showResult(c.yes(m))
	}
	return true
}

func (m *model) handleFormKey(k key) bool {
	f := m.form
	switch k.t {
	case keyCtrlC, keyCtrlD:
		return false
	case keyEsc:
		m.form = nil
		return true
	case keyEnter:
		spec := f.spec
		vals := f.values()
		m.form = nil
		m.showResult(spec.submit(m, vals))
		return true
	case keyTab, keyDown:
		f.idx = (f.idx + 1) % len(f.field)
		return true
	case keyShiftTab, keyUp:
		f.idx = (f.idx - 1 + len(f.field)) % len(f.field)
		return true
	}

	fl := &f.field[f.idx]
	switch fl.kind {
	case fieldBool:
		if k.t == keyLeft || k.t == keyRight || (k.t == keyRune && k.r == ' ') {
			fl.value = onOffBool(fl.value != "true")
		}
	case fieldSelect:
		if len(fl.options) > 0 && (k.t == keyLeft || k.t == keyRight) {
			cur := indexOf(fl.options, fl.value)
			delta := 1
			if k.t == keyLeft {
				delta = -1
			}
			cur = (cur + delta + len(fl.options)) % len(fl.options)
			fl.value = fl.options[cur]
		}
	case fieldText:
		switch {
		case k.t == keyBackspace:
			if r := []rune(fl.value); len(r) > 0 {
				fl.value = string(r[:len(r)-1])
			}
		case k.t == keyCtrlU:
			fl.value = ""
		case k.t == keyRune:
			fl.value += string(k.r)
		}
	}
	return true
}

func onOffBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func indexOf(opts []string, v string) int {
	for i, o := range opts {
		if o == v {
			return i
		}
	}
	return 0
}

// ---- rendering --------------------------------------------------------

// drawModal paints whichever of form/confirm/result is open, on top of
// everything else. Called last in model.draw, matching drawSearch's own
// place in the stack.
func (m *model) drawModal(s *Screen) {
	switch {
	case m.result != nil:
		m.drawResult(s)
	case m.confirm != nil:
		m.drawConfirm(s)
	case m.form != nil:
		m.drawForm(s)
	}
}

// modalRect picks a centered box of the given size, clamped to the screen.
func (m *model) modalRect(w, h int) (x, y, cw, ch int) {
	cw = min(w, m.w-4)
	ch = min(h, m.h-4)
	if cw < 10 {
		cw = min(10, m.w)
	}
	if ch < 3 {
		ch = min(3, m.h)
	}
	x = max(0, (m.w-cw)/2)
	y = max(0, (m.h-ch)/2)
	return
}

func (m *model) drawForm(s *Screen) {
	f := m.form
	w := 66
	h := len(f.field)*2 + 4
	x, y, w, h := m.modalRect(w, h)

	panel := style{}.withBg(m.pal.panel).withFg(m.pal.fg)
	s.Fill(x, y, w, h, ' ', panel)
	titleSt := style{}.withFg(m.pal.fg).withBold().withBg(m.pal.panel)
	s.PrintPad(x+1, y, w-2, strings.ToUpper(f.spec.title), titleSt)
	s.Fill(x, y+1, w, 1, boxH, style{}.withFg(m.pal.line).withBg(m.pal.panel))

	row := y + 2
	labelW := 0
	for _, fl := range f.field {
		if n := len(fl.label); n > labelW {
			labelW = n
		}
	}
	if labelW > w-10 {
		labelW = w - 10
	}
	for i, fl := range f.field {
		focused := i == f.idx
		labelSt := style{}.withFg(m.pal.mut).withBg(m.pal.panel)
		valSt := style{}.withFg(m.pal.fg).withBg(m.pal.panel)
		if focused {
			labelSt = style{}.withFg(m.pal.acc).withBold().withBg(m.pal.panel)
			valSt = style{}.withFg(m.pal.fg).withBg(m.pal.hover)
		}
		if row >= y+h-1 {
			break // more fields than the box has room for at this height; scrolled forms aren't needed at today's field counts
		}
		s.PrintPad(x+1, row, labelW+1, fl.label+":", labelSt)
		display := fieldDisplay(fl)
		if focused && fl.kind == fieldText {
			display += "\u2588"
		}
		s.PrintPad(x+2+labelW, row, w-3-labelW, display, valSt)
		row++
		if focused && fl.help != "" {
			s.PrintPad(x+2+labelW, row, w-3-labelW, fl.help, style{}.withFg(m.pal.mut).withBg(m.pal.panel).withDim())
		}
		row++
	}
	s.Fill(x, y+h-1, w, 1, ' ', panel)
	s.PrintPad(x+1, y+h-1, w-2, "enter submit  tab next field  \u2190\u2192 toggle/cycle  esc cancel",
		style{}.withFg(m.pal.mut).withBg(m.pal.panel))
}

// fieldDisplay renders one field's current value for the form.
func fieldDisplay(fl formField) string {
	switch fl.kind {
	case fieldBool:
		if fl.value == "true" {
			return "[x] on"
		}
		return "[ ] off"
	case fieldSelect:
		return "\u2039 " + fl.value + " \u203a"
	default:
		return fl.value
	}
}

func (m *model) drawConfirm(s *Screen) {
	lines := wrap(m.confirm.message, 50)
	w, h := 56, len(lines)+4
	x, y, w, h := m.modalRect(w, h)

	panel := style{}.withBg(m.pal.panel).withFg(m.pal.fg)
	s.Fill(x, y, w, h, ' ', panel)
	s.PrintPad(x+1, y, w-2, "CONFIRM", style{}.withFg(m.pal.warn).withBold().withBg(m.pal.panel))
	s.Fill(x, y+1, w, 1, boxH, style{}.withFg(m.pal.line).withBg(m.pal.panel))
	for i, l := range lines {
		if y+2+i >= y+h-1 {
			break
		}
		s.PrintPad(x+1, y+2+i, w-2, l, panel)
	}
	s.PrintPad(x+1, y+h-1, w-2, "y confirm   n / esc cancel", style{}.withFg(m.pal.mut).withBg(m.pal.panel))
}

func (m *model) drawResult(s *Screen) {
	r := m.result
	title, titleSt := "OK", style{}.withFg(m.pal.ok).withBold()
	if !r.ok {
		title, titleSt = "FAILED", style{}.withFg(m.pal.danger).withBold()
	}
	var lines []string
	for _, l := range r.lines {
		lines = append(lines, wrap(l, 60)...)
	}
	if len(lines) == 0 {
		lines = []string{"(no output)"}
	}
	w, h := 66, len(lines)+4
	x, y, w, h := m.modalRect(w, h)

	panel := style{}.withBg(m.pal.panel).withFg(m.pal.fg)
	s.Fill(x, y, w, h, ' ', panel)
	s.PrintPad(x+1, y, w-2, title, titleSt.withBg(m.pal.panel))
	s.Fill(x, y+1, w, 1, boxH, style{}.withFg(m.pal.line).withBg(m.pal.panel))
	for i, l := range lines {
		if y+2+i >= y+h-1 {
			break
		}
		s.PrintPad(x+1, y+2+i, w-2, l, panel)
	}
	s.PrintPad(x+1, y+h-1, w-2, "press any key to continue", style{}.withFg(m.pal.mut).withBg(m.pal.panel))
}
