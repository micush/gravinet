package webadmin

import (
	"strings"
	"testing"
)

// The fleet rollout on System > Upgrade ran inside try/finally with no catch.
// A throw — fetch rejecting, the response stream breaking, an archive that had
// been replaced on disk since it was picked — unwound through the finally,
// which re-enabled the button and restored its label, and then vanished as an
// unhandled rejection. What the operator saw was a rollout that had never
// started: progress frozen at "0 of N", the button back to Upgrade, and no
// error anywhere. That is indistinguishable from the button not working, which
// is exactly how it was reported.

// matchingClose returns the index of the brace closing the one at open.
func matchingClose(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// No try in the rollout handler may be closed by finally alone. This is the
// structural version of the bug: it fails for any new try/finally added
// anywhere in the handler, not just the three that were fixed.
func TestUpgradeRolloutHasNoCatchlessTry(t *testing.T) {
	src := uiFuncSrc(t, "drawUpgrade")
	start := strings.Index(src, "upgradeBtn.onclick = async () => {")
	if start < 0 {
		t.Fatal("the upgrade click handler is not in drawUpgrade any more")
	}
	h := src[start:]
	if end := matchingClose(h, strings.Index(h, "{")); end > 0 {
		h = h[:end]
	}
	for i := 0; ; {
		j := strings.Index(h[i:], "try {")
		if j < 0 {
			break
		}
		open := i + j + len("try ")
		close := matchingClose(h, open)
		if close < 0 {
			t.Fatal("unbalanced braces while scanning the handler")
		}
		tail := strings.TrimLeft(h[close+1:], " \t\n")
		if !strings.HasPrefix(tail, "catch") {
			line := strings.Count(h[:open], "\n") + 1
			t.Errorf("try at handler line %d is not followed by catch (found %.20q); a throw there re-enables the button and disappears",
				line, tail)
		}
		i = open
	}
}

// Each failure path has to put something where the operator is already
// looking. The finally re-enables the button either way, so a path that
// returns without writing leaves the page looking idle and untouched.
func TestUpgradeFailurePathsAreVisible(t *testing.T) {
	src := uiFuncSrc(t, "drawUpgrade")
	for _, want := range []string{
		// request never left the browser, or the file could not be read
		"The push could not be sent: ",
		// stream broke part-way: some peers reported, the rest are unknown
		"The connection dropped while the rollout was running: ",
		// local apply, same unreadable-file failure mode
		"The upload could not be sent: ",
		// backstop for anything not anticipated
		"The rollout stopped unexpectedly: ",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("no message for a failure path: %q", want)
		}
	}
	// The two that mean "nothing was pushed" must say so, since the remedy
	// differs from the mid-rollout case where peers may already have changed.
	push := src[strings.Index(src, "The push could not be sent: "):]
	if !strings.Contains(push[:400], "Nothing was pushed") {
		t.Error("a send failure does not say that nothing was pushed, which is the difference that decides whether to check the fleet first")
	}
	drop := src[strings.Index(src, "The connection dropped while the rollout was running: "):]
	if !strings.Contains(drop[:500], "may or may not have been upgraded") {
		t.Error("a dropped connection does not warn that some peers' state is unknown")
	}
}

// The picked file is re-read for each batch and its absence is an error rather
// than an undefined quietly appended to the form. A File is a handle to a
// path, read when the body is streamed rather than when it was picked, so a
// build replaced under the same name is the likely cause of the throw this
// whole change is about.
func TestUpgradeRereadsThePickedFile(t *testing.T) {
	src := uiFuncSrc(t, "drawUpgrade")
	if !strings.Contains(src, "const srcFile = () => {") || !strings.Contains(src, "fd.append('source', srcFile());") {
		t.Error("the push no longer re-reads the picked file through srcFile")
	}
	if strings.Contains(src, "fd.append('source', fileIn.files[0])") {
		t.Error("the push appends fileIn.files[0] directly again; an absent file becomes the string 'undefined' server-side")
	}
	if !strings.Contains(src, "no longer available. Pick the source archive again.") {
		t.Error("no message naming re-picking the file, which is the remedy for the case that produced this bug")
	}
}

// The backstop rethrows, so an unanticipated failure is still reportable from
// the console rather than being swallowed by the very handler added to
// surface it.
func TestUpgradeBackstopRethrows(t *testing.T) {
	src := uiFuncSrc(t, "drawUpgrade")
	i := strings.Index(src, "The rollout stopped unexpectedly: ")
	if i < 0 {
		t.Fatal("no backstop catch")
	}
	if !strings.Contains(src[i:i+700], "throw e;") {
		t.Error("the backstop swallows the error; it should report it and rethrow so it still reaches the console")
	}
}
