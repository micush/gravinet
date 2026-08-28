package webadmin

import (
	"strings"
	"testing"
)

// A mesh-wide capture must not touch the Capture tab's own state on this node.
//
// The local leg used s.capture — the single active capture the Capture tab is
// bound to — because it was the same in-process path handleCaptureStart
// already used. begin() resets the buffer, repoints iface at the overlay
// device and sets running, so clicking "Capture all peers" reached up into
// the card above it: a capture running there was killed, anything captured
// there was discarded, and the tab was left sitting on a mesh0 capture nobody
// started on it. Reported as packet capture starting on mesh0 by itself once
// the .tgz came down.
//
// Pinned at the source. The failure is a shared pointer being reached for, not
// a value that comes out wrong, and the alternative is a test that opens real
// capture handles on interfaces this host does not have.
func TestMeshCaptureDoesNotTouchTheSharedCaptureState(t *testing.T) {
	// Comments stripped: the explanation inside this branch names s.capture
	// repeatedly, saying why it is not used. A scan that read those would be
	// grading the prose. stripJSComments drops whole-line // comments, which
	// Go and JS spell identically.
	fn := stripJSComments(between(t, mustRead("meshcapture.go"), "func captureOnePeer(", "\n\ttarget, err := s.resolveManagedTarget"))

	if strings.Contains(fn, "s.capture") {
		t.Error("the local mesh-capture leg still drives s.capture; it will reset the operator's own capture buffer, repoint its interface, and leave the Capture tab on a capture nobody started there")
	}
	if !strings.Contains(fn, "cs := newCaptureState()") {
		t.Fatal("the local leg does not open a capture state of its own")
	}
	// Every call in the leg has to be on that private state, not just the
	// first: one stray s.capture.stop() would still kill the operator's.
	for _, m := range []string{"cs.begin(", "cs.addEpoch(", "cs.setLinktype(", "cs.setHandle(", "cs.stop()", "cs.writePcap("} {
		if !strings.Contains(fn, m) {
			t.Errorf("the local leg does not call %s on its own capture state", m)
		}
	}
}

// The card's description told operators the opposite, which is worse than
// silence: it named the takeover as intended behaviour, so anyone who noticed
// it had been told not to report it.
func TestMeshCaptureHintSaysTheLocalTabIsLeftAlone(t *testing.T) {
	src := mustRead("ui.go")
	if strings.Contains(src, "Ending any capture already running or shown on a touched peer") {
		t.Error("the mesh capture card still describes ending a capture on this node's own tab as a side effect; it no longer does that")
	}
	if !strings.Contains(src, "own Capture tab above is left alone") {
		t.Error("the mesh capture card does not say this node's own Capture tab is untouched")
	}
	// Still honest about remote peers, which do go through their own
	// /api/capture/start and so are still disturbed.
	if !strings.Contains(src, "peer\\u2019s is not") {
		t.Error("the hint no longer warns that a remote peer's Capture tab is still ended")
	}
}

// Every button reachable from the search box, and the name each is reached by.
//
// The chain for each is the same one tshoot established: an index entry, a key
// on the button, and the two agreeing. They are far apart in the file and both
// are bare strings, so a typo in either gives a search result that lands on
// the right section and points at nothing — which looks enough like working to
// go unnoticed.
func TestSearchableButtonsAreWiredEndToEnd(t *testing.T) {
	src := mustRead("ui.go")
	idx := jsFunc(t, "function buildSearchIndex(")

	for _, tc := range []struct {
		btn     string // the key, on both sides
		section string
		queries []string
	}{
		{"tshoot", "logs", []string{"tshoot", "troubleshoot"}},
		{"unban", "bans", []string{"unban", "ban", "unblock"}},
		{"keygen", "keys", []string{"generate", "rotate", "key"}},
		{"nettoken", "networks", []string{"token", "join", "invite"}},
		{"mpeerinfo", "mesh-peers", []string{"info", "detail", "inspect"}},
		{"mpeershell", "mesh-peers", []string{"shell", "console", "terminal"}},
	} {
		t.Run(tc.btn, func(t *testing.T) {
			// The button carries the key, so enhanceTable renders it as
			// data-tbar-btn and there is something to find in the DOM.
			if !strings.Contains(src, "key:'"+tc.btn+"'") {
				t.Fatalf("no toolbar button is keyed %q, so the search result has nothing to land on", tc.btn)
			}
			// The index asks for that exact key, in that section.
			call := actionEntry(t, idx, tc.btn)
			if got := jsStringLiterals(call)[2]; got != tc.section {
				t.Errorf("the entry targets section %q, want %q", got, tc.section)
			}
			// And each word someone would plausibly type finds it.
			// searchIndexQuery is a substring test over label+extraHay
			// lowercased, so this asks it the same question the box would.
			lits := jsStringLiterals(call)
			hay := strings.ToLower(lits[0] + " " + lits[len(lits)-1])
			for _, q := range tc.queries {
				if !strings.Contains(hay, q) {
					t.Errorf("searching %q would not find this button: not a substring of %q", q, hay)
				}
			}
		})
	}
}

// actionEntry returns the add() call whose match targets btn, comments
// stripped — the prose around these entries names the very words being
// searched for, and a test reading that would be grading the explanation.
func actionEntry(t *testing.T, idx, btn string) string {
	t.Helper()
	want := "btn:'" + btn + "'"
	for _, chunk := range strings.Split(idx, "add(")[1:] {
		end := strings.Index(chunk, ");")
		if end < 0 {
			continue
		}
		call := stripJSComments(chunk[:end])
		if strings.Contains(call, want) && strings.Contains(call, "kind:'action'") {
			return call
		}
	}
	t.Fatalf("buildSearchIndex has no action entry for the %q button", btn)
	return ""
}
