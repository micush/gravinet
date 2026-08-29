package tui

// Turning a byte stream into key events.
//
// Terminal input is not a sequence of keys; it is a sequence of bytes in
// which some keys are one byte, some are three or six, and one of them —
// Escape — is a prefix of all the multi-byte ones. Every decoder has to deal
// with that ambiguity, and the two usual answers are a timer (wait a few
// milliseconds after ESC and call it a bare Escape if nothing follows) or a
// buffer (decode only what is unambiguously complete, keep the rest).
//
// This uses the buffer. A timer makes the decoder depend on wall-clock time,
// which makes it untestable without sleeping and makes Escape feel sluggish
// over a slow link — exactly the link this is most likely to be used over. A
// buffered decoder reading from a blocking source has the opposite property:
// a lone ESC sitting in the buffer is held until the next read, which on a
// real keyboard is the next keystroke. So Escape is bound as a secondary key
// everywhere it appears, never the only way out of anything, and the primary
// binding is always something unambiguous. See app.go's key table.

import (
	"bufio"
	"io"
)

// keyType enumerates the keys this package acts on. Anything unrecognized
// decodes to keyUnknown and is ignored by the model rather than guessed at.
type keyType int

const (
	keyUnknown keyType = iota
	keyRune            // an ordinary character; see key.r
	keyEnter
	keyEsc
	keyTab
	keyShiftTab
	keyBackspace
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyPgUp
	keyPgDn
	keyDelete
	keyCtrlC
	keyCtrlD
	keyCtrlL
	keyCtrlU
)

// key is one decoded keystroke.
type key struct {
	t keyType
	r rune
}

// String is for test failure messages, which are otherwise a pair of integers.
func (k key) String() string {
	switch k.t {
	case keyRune:
		return string(k.r)
	case keyEnter:
		return "Enter"
	case keyEsc:
		return "Esc"
	case keyTab:
		return "Tab"
	case keyShiftTab:
		return "ShiftTab"
	case keyBackspace:
		return "Backspace"
	case keyUp:
		return "Up"
	case keyDown:
		return "Down"
	case keyLeft:
		return "Left"
	case keyRight:
		return "Right"
	case keyHome:
		return "Home"
	case keyEnd:
		return "End"
	case keyPgUp:
		return "PgUp"
	case keyPgDn:
		return "PgDn"
	case keyDelete:
		return "Delete"
	case keyCtrlC:
		return "Ctrl-C"
	case keyCtrlD:
		return "Ctrl-D"
	case keyCtrlL:
		return "Ctrl-L"
	case keyCtrlU:
		return "Ctrl-U"
	}
	return "Unknown"
}

// keyReader decodes keys from a byte source.
type keyReader struct {
	br  *bufio.Reader
	buf []byte
}

func newKeyReader(r io.Reader) *keyReader {
	return &keyReader{br: bufio.NewReaderSize(r, 256)}
}

// next returns the next key, blocking until one is available. It reads more
// bytes only when what it holds cannot yet be resolved, which is what keeps a
// pasted escape sequence from being split across two reads and mis-decoded.
func (k *keyReader) next() (key, error) {
	for {
		if ky, n, ok := decodeKey(k.buf); ok {
			k.buf = k.buf[n:]
			return ky, nil
		}
		b, err := k.br.ReadByte()
		if err != nil {
			return key{}, err
		}
		k.buf = append(k.buf, b)
		// A buffer that has grown past any real escape sequence is holding
		// something this does not understand. Drop the leading byte and try
		// again rather than growing forever: on a terminal sending a report
		// nobody asked for (a mouse event, a bracketed-paste marker), the
		// alternative is that every subsequent keystroke is swallowed.
		if len(k.buf) > 16 {
			k.buf = k.buf[1:]
		}
	}
}

// decodeKey decodes the first key in b. ok is false when b is a proper prefix
// of a sequence that could still complete — that is the signal to read more,
// and the reason this is split out from keyReader: it is a pure function over
// a byte slice, which is how keys_test.go can exercise every sequence without
// a terminal.
func decodeKey(b []byte) (k key, n int, ok bool) {
	if len(b) == 0 {
		return key{}, 0, false
	}
	switch b[0] {
	case 0x03:
		return key{t: keyCtrlC}, 1, true
	case 0x04:
		return key{t: keyCtrlD}, 1, true
	case 0x0c:
		return key{t: keyCtrlL}, 1, true
	case 0x15:
		return key{t: keyCtrlU}, 1, true
	case '\r', '\n':
		return key{t: keyEnter}, 1, true
	case '\t':
		return key{t: keyTab}, 1, true
	case 0x7f, 0x08:
		return key{t: keyBackspace}, 1, true
	case 0x1b:
		return decodeEscape(b)
	}
	if b[0] < 0x20 {
		return key{t: keyUnknown}, 1, true
	}
	r, size := decodeRune(b)
	if size == 0 {
		return key{}, 0, false // incomplete UTF-8; wait for the rest
	}
	return key{t: keyRune, r: r}, size, true
}

// decodeEscape handles everything starting with ESC: the CSI sequences the
// arrow/navigation keys arrive as, the SS3 sequences some terminals send for
// the same keys in application mode, and a bare ESC.
func decodeEscape(b []byte) (key, int, bool) {
	if len(b) == 1 {
		// Could be a bare Escape or the start of a sequence. Unresolvable
		// without more bytes; see the package note above on why this waits
		// rather than guessing on a timer.
		return key{}, 0, false
	}
	switch b[1] {
	case '[':
		return decodeCSI(b)
	case 'O':
		// SS3: ESC O <letter>, sent for the arrows and Home/End by terminals
		// in application cursor mode.
		if len(b) < 3 {
			return key{}, 0, false
		}
		switch b[2] {
		case 'A':
			return key{t: keyUp}, 3, true
		case 'B':
			return key{t: keyDown}, 3, true
		case 'C':
			return key{t: keyRight}, 3, true
		case 'D':
			return key{t: keyLeft}, 3, true
		case 'H':
			return key{t: keyHome}, 3, true
		case 'F':
			return key{t: keyEnd}, 3, true
		}
		return key{t: keyUnknown}, 3, true
	}
	// ESC followed by anything else: Alt-<key> on most terminals. Nothing
	// here binds Alt, so the ESC is reported and the following byte is left
	// to decode on its own — which means Alt-x behaves as Escape then x,
	// the conventional fallback.
	return key{t: keyEsc}, 1, true
}

// decodeCSI handles ESC [ ... — the control sequences. A CSI runs until a
// byte in the range 0x40..0x7e, so this scans for that terminator and then
// looks at what it found.
func decodeCSI(b []byte) (key, int, bool) {
	end := -1
	for i := 2; i < len(b); i++ {
		if b[i] >= 0x40 && b[i] <= 0x7e {
			end = i
			break
		}
	}
	if end < 0 {
		if len(b) > 12 {
			// Too long to be any sequence this knows; report it as unknown
			// so the reader makes progress instead of stalling.
			return key{t: keyUnknown}, len(b), true
		}
		return key{}, 0, false
	}
	params := string(b[2:end])
	n := end + 1
	switch b[end] {
	case 'A':
		return key{t: keyUp}, n, true
	case 'B':
		return key{t: keyDown}, n, true
	case 'C':
		return key{t: keyRight}, n, true
	case 'D':
		return key{t: keyLeft}, n, true
	case 'H':
		return key{t: keyHome}, n, true
	case 'F':
		return key{t: keyEnd}, n, true
	case 'Z':
		return key{t: keyShiftTab}, n, true
	case '~':
		// ESC [ <n> ~ — the numbered keys. The parameter may carry a
		// modifier after a semicolon (ESC [ 5 ; 2 ~ is Shift-PgUp); nothing
		// here binds modified navigation keys, so the modifier is dropped
		// and the base key is reported.
		switch numParam(params) {
		case 1, 7:
			return key{t: keyHome}, n, true
		case 3:
			return key{t: keyDelete}, n, true
		case 4, 8:
			return key{t: keyEnd}, n, true
		case 5:
			return key{t: keyPgUp}, n, true
		case 6:
			return key{t: keyPgDn}, n, true
		}
	}
	return key{t: keyUnknown}, n, true
}

// numParam reads the leading integer of a CSI parameter string, stopping at
// the first ';'. Returns -1 for anything that does not start with a digit.
func numParam(s string) int {
	v := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ';' {
			break
		}
		if s[i] < '0' || s[i] > '9' {
			return -1
		}
		if v < 0 {
			v = 0
		}
		v = v*10 + int(s[i]-'0')
	}
	return v
}

// decodeRune decodes one UTF-8 rune, returning size 0 when b holds only part
// of one. utf8.DecodeRune cannot express that case — it returns RuneError
// with size 1 both for a truncated sequence and for a genuinely invalid byte
// — and telling those apart is the whole job here: one means wait for more
// input, the other means drop a byte.
func decodeRune(b []byte) (rune, int) {
	c := b[0]
	var need int
	switch {
	case c < 0x80:
		return rune(c), 1
	case c&0xe0 == 0xc0:
		need = 2
	case c&0xf0 == 0xe0:
		need = 3
	case c&0xf8 == 0xf0:
		need = 4
	default:
		return '\uFFFD', 1 // invalid leading byte: consume it
	}
	if len(b) < need {
		return 0, 0 // truncated: wait
	}
	r := rune(c & (0x7f >> need))
	for i := 1; i < need; i++ {
		if b[i]&0xc0 != 0x80 {
			return '\uFFFD', 1 // malformed continuation: consume the lead byte
		}
		r = r<<6 | rune(b[i]&0x3f)
	}
	return r, need
}
