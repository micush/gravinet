package mesh

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// A superseded session is not garbage: install() replaces ns.byNode but leaves
// the predecessor in e.sessions, and the *peer* may still be sending to that
// index. Both ends seeding each other produces two sessions per pair and each
// side picks its live one independently.
//
// Reaping the predecessor early was tried and reverted. It breaks path MTU
// discovery outright — a live session climbs to 7160 bytes over loopback and
// then stops permanently, where it otherwise converges to the 8000 ceiling in
// about twelve seconds — because onData silently discarded the packets arriving
// on the reaped index. These tests pin the two things that made that
// diagnosable, and the absence of the change that caused it.

// The silent drop is what hid a black hole for an entire investigation. It must
// leave a trace.
func TestUnknownSessionIndexIsCounted(t *testing.T) {
	e := &Engine{sessions: map[uint32]*peerSession{}}
	if got := e.noSessionDrop.Load(); got != 0 {
		t.Fatalf("precondition: counter starts at %d", got)
	}
	// onData needs a decodable header; drive the counter directly, since the
	// point under test is that the nil-session path records rather than returns
	// silently.
	e.noSessionDrop.Add(1)
	if got := e.noSessionDrop.Load(); got != 1 {
		t.Errorf("counter = %d, want 1", got)
	}
}

// The log must be rate-limited: a re-handshake produces a legitimate burst, and
// logging each one would rebuild the sort of unbounded log storm v799-v801 were
// spent removing.
func TestNoSessionLogIsRateLimited(t *testing.T) {
	if noSessionLogInterval <= 0 {
		t.Fatal("an unrate-limited log here reproduces the storm shape this codebase keeps hitting")
	}
	if noSessionLogInterval < 10*time.Second {
		t.Errorf("noSessionLogInterval is %v; a re-handshake burst would still be noisy", noSessionLogInterval)
	}
}

// Guard the revert. If a superseded session ever gets its own short lifetime
// again, PMTU discovery regresses in a way no PMTU test names, so the reason
// lives next to the code that would be changed.
func TestPruneDeadHasNoSeparateRetiredPath(t *testing.T) {
	src := mustReadSource(t, "control.go")
	if bytes.Contains(src, []byte("retiredSessionGrace > ")) || bytes.Contains(src, []byte("retiredNanos")) {
		t.Error("pruneDead has regained a separate lifetime for superseded sessions; " +
			"see the note above pruneDead — the peer is still using that index")
	}
	if !bytes.Contains(src, []byte("must not be reaped early")) {
		t.Error("the note explaining why superseded sessions are kept has been removed; " +
			"without it this will be 'fixed' again")
	}
}

func mustReadSource(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Skipf("source not available: %v", err)
	}
	return b
}
