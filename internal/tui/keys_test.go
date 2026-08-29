package tui

import (
	"bytes"
	"testing"
)

func TestDecodeKeySingleBytes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want keyType
	}{
		{"\x03", keyCtrlC},
		{"\x04", keyCtrlD},
		{"\x0c", keyCtrlL},
		{"\x15", keyCtrlU},
		{"\r", keyEnter},
		{"\n", keyEnter},
		{"\t", keyTab},
		{"\x7f", keyBackspace},
		{"\x08", keyBackspace},
	} {
		k, n, ok := decodeKey([]byte(c.in))
		if !ok || n != 1 || k.t != c.want {
			t.Errorf("decode(%q) = %v n=%d ok=%v, want %v", c.in, k, n, ok, key{t: c.want})
		}
	}
}

func TestDecodeKeyRunes(t *testing.T) {
	k, n, ok := decodeKey([]byte("q"))
	if !ok || n != 1 || k.t != keyRune || k.r != 'q' {
		t.Errorf("decode(q) = %v n=%d ok=%v", k, n, ok)
	}
	// Multi-byte UTF-8 arrives one byte at a time over a slow link, so a
	// truncated sequence must ask for more rather than decode a replacement
	// character and swallow the rest.
	if _, _, ok := decodeKey([]byte("\xc3")); ok {
		t.Error("a truncated two-byte rune should not resolve")
	}
	k, n, ok = decodeKey([]byte("\xc3\xa9"))
	if !ok || n != 2 || k.r != '\u00e9' {
		t.Errorf("decode(é) = %v n=%d ok=%v", k, n, ok)
	}
}

func TestDecodeCSISequences(t *testing.T) {
	for _, c := range []struct {
		in   string
		want keyType
	}{
		{"\x1b[A", keyUp},
		{"\x1b[B", keyDown},
		{"\x1b[C", keyRight},
		{"\x1b[D", keyLeft},
		{"\x1b[H", keyHome},
		{"\x1b[F", keyEnd},
		{"\x1b[Z", keyShiftTab},
		{"\x1b[3~", keyDelete},
		{"\x1b[5~", keyPgUp},
		{"\x1b[6~", keyPgDn},
		{"\x1b[1~", keyHome},
		{"\x1b[4~", keyEnd},
		// Application cursor mode (SS3), which some terminals use for the
		// same keys.
		{"\x1bOA", keyUp},
		{"\x1bOB", keyDown},
		{"\x1bOH", keyHome},
		{"\x1bOF", keyEnd},
	} {
		k, n, ok := decodeKey([]byte(c.in))
		if !ok || n != len(c.in) || k.t != c.want {
			t.Errorf("decode(%q) = %v n=%d ok=%v, want %v consuming %d", c.in, k, n, ok, key{t: c.want}, len(c.in))
		}
	}
}

func TestDecodeCSIWithAModifierFallsBackToTheBaseKey(t *testing.T) {
	// Shift-PgUp is ESC [ 5 ; 2 ~. Nothing binds modified navigation keys, so
	// the modifier is dropped rather than the whole sequence being unknown —
	// which would make the key do nothing at all.
	k, n, ok := decodeKey([]byte("\x1b[5;2~"))
	if !ok || k.t != keyPgUp || n != 6 {
		t.Errorf("modified PgUp = %v n=%d ok=%v", k, n, ok)
	}
}

func TestBareEscapeWaitsForMoreInput(t *testing.T) {
	// This is the whole design decision in keys.go: a lone ESC is a prefix of
	// every arrow key, so it is held rather than guessed at on a timer. It
	// must not resolve on its own.
	if _, _, ok := decodeKey([]byte("\x1b")); ok {
		t.Error("a bare ESC resolved without knowing whether more was coming")
	}
	if _, _, ok := decodeKey([]byte("\x1b[")); ok {
		t.Error("an incomplete CSI resolved")
	}
	// ESC followed by an ordinary key is Alt-<key> on most terminals, and
	// nothing binds Alt: report the Escape and leave the rest to decode on
	// its own, which is the conventional fallback.
	k, n, ok := decodeKey([]byte("\x1bx"))
	if !ok || k.t != keyEsc || n != 1 {
		t.Errorf("ESC x = %v n=%d ok=%v, want Esc consuming 1", k, n, ok)
	}
}

func TestKeyReaderSplitsASequenceAcrossReads(t *testing.T) {
	// The reader buffers, so an arrow key arriving one byte at a time (a slow
	// ssh link, exactly where this matters) still decodes as one key rather
	// than as Escape, [, A.
	kr := newKeyReader(bytes.NewReader([]byte("\x1b[Aq")))
	k, err := kr.next()
	if err != nil || k.t != keyUp {
		t.Fatalf("first key = %v, %v", k, err)
	}
	k, err = kr.next()
	if err != nil || k.t != keyRune || k.r != 'q' {
		t.Fatalf("second key = %v, %v", k, err)
	}
}

func TestKeyReaderDoesNotStallOnGarbage(t *testing.T) {
	// A terminal sending a report nobody asked for (a mouse event, a
	// bracketed-paste marker) must not swallow every subsequent keystroke.
	kr := newKeyReader(bytes.NewReader([]byte("\x1b[<0;12;34Mq")))
	var got []key
	for i := 0; i < 4; i++ {
		k, err := kr.next()
		if err != nil {
			break
		}
		got = append(got, k)
	}
	found := false
	for _, k := range got {
		if k.t == keyRune && k.r == 'q' {
			found = true
		}
	}
	if !found {
		t.Errorf("the q after an unrecognized report never arrived; got %v", got)
	}
}

func TestNumParam(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"5", 5}, {"15", 15}, {"5;2", 5}, {"", -1}, {"x", -1},
	} {
		if got := numParam(c.in); got != c.want {
			t.Errorf("numParam(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
