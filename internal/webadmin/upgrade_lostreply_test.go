package webadmin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// Applying an upgrade ends with the peer swapping its binary and restarting
// into it — which tears down the connection carrying the reply. The reply is
// lost precisely *because* the apply succeeded, so "no response" and "it
// worked" are indistinguishable at the pushing end.
//
// Before v918 that was read as a transport failure and retried. The retry
// arrived at a node now mid-trial from the first push, whose guard correctly
// refuses a second one — and a refusal is a real response, so it ended the
// retry loop and was reported as the peer's error. The first attempt's success
// was never mentioned. Because this runs on the canary first, one peer that
// had upgraded perfectly stopped the entire rollout with a message saying it
// had failed, which is what was reported from a 17-peer fleet:
//
//	x hw-macmini-macos
//	  an upgrade (916 -> 917) is already mid-trial on this node - started 1m0s ago
//	  the first peer failed - the rollout stopped there before touching anything else
//
// while that peer was, in the same screen, listed as running v917.

// restartingPeer serves the apply by hijacking and closing the connection —
// exactly what a peer restarting into its new binary does — and then reports
// the given version from /api/upgrade, as a peer that came back does.
func restartingPeer(t *testing.T, backAs map[string]any) (*httptest.Server, *int32) {
	t.Helper()
	var applies int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/upgrade" {
			writeJSON(w, http.StatusOK, backAs)
			return
		}
		atomic.AddInt32(&applies, 1)
		io.Copy(io.Discard, r.Body)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter doesn't support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}))
	t.Cleanup(ts.Close)
	return ts, &applies
}

func fastProbes(t *testing.T) {
	t.Helper()
	w, i := pushApplyProbeWindow, pushApplyProbeInterval
	pushApplyProbeWindow, pushApplyProbeInterval = 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { pushApplyProbeWindow, pushApplyProbeInterval = w, i })
}

func srcFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "src-*.tgz")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("archive")
	f.Close()
	return f.Name()
}

// A peer that comes back running the pushed version applied it. Counting that
// as a failure is what stopped the rollout; pushing again is worse, because
// the second push cannot succeed while the first is mid-trial.
func TestLostReplyFromAnUpgradedPeerCountsAsApplied(t *testing.T) {
	fastProbes(t)
	ts, applies := restartingPeer(t, map[string]any{"version": "917", "phase": "idle"})
	srv := testServer()

	status, _, err := srv.pushSourceToWithRetry("peer", testTarget(t, ts), srcFile(t), "deadbeef", "917")
	if err != nil {
		t.Fatalf("a peer that came back on the pushed version was reported as failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if n := atomic.LoadInt32(applies); n != 1 {
		t.Errorf("the apply was sent %d times; a peer that already applied must not be pushed again", n)
	}
}

// The same when it is holding the new version pending confirmation, which is
// the state the reported fleet was actually in: mid-trial, not yet committed.
func TestLostReplyFromAMidTrialPeerCountsAsApplied(t *testing.T) {
	fastProbes(t)
	ts, applies := restartingPeer(t, map[string]any{"version": "916", "phase": "pending", "to": "917"})
	srv := testServer()

	if _, _, err := srv.pushSourceToWithRetry("peer", testTarget(t, ts), srcFile(t), "deadbeef", "v917"); err != nil {
		t.Fatalf("a peer mid-trial on the pushed version was reported as failed: %v", err)
	}
	if n := atomic.LoadInt32(applies); n != 1 {
		t.Errorf("the apply was sent %d times; a mid-trial peer must not be pushed again", n)
	}
}

// A peer that is reachable and still on its old version did not apply, so the
// retry that this change guards must still happen. Without this the fix would
// turn every transport blip into a silent success.
func TestLostReplyFromAPeerThatDidNotApplyStillRetries(t *testing.T) {
	fastProbes(t)
	ts, applies := restartingPeer(t, map[string]any{"version": "916", "phase": "idle"})
	srv := testServer()
	back := pushRetryBackoff
	pushRetryBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { pushRetryBackoff = back })

	if _, _, err := srv.pushSourceToWithRetry("peer", testTarget(t, ts), srcFile(t), "deadbeef", "917"); err == nil {
		t.Error("a peer still on its old version was reported as applied")
	}
	if n := atomic.LoadInt32(applies); n < 2 {
		t.Errorf("the apply was sent %d time(s); a peer that did not apply must still be retried", n)
	}
}

// Version strings are compared with the v stripped: the archive reports 917
// while a tag and the UI both say v917, and a mismatch here silently disables
// the whole check rather than failing loudly.
func TestPeerLandedOnIgnoresTheVPrefix(t *testing.T) {
	for _, c := range []struct {
		version, phase, to, want string
		expect                   bool
	}{
		{"917", "idle", "", "v917", true},
		{"v917", "idle", "", "917", true},
		{"916", "pending", "917", "v917", true},
		{"916", "idle", "", "917", false},
		{"916", "pending", "918", "917", false},
		{"917", "idle", "", "", false}, // no known target disables the check
	} {
		if got := peerLandedOn(c.version, c.phase, c.to, c.want); got != c.expect {
			t.Errorf("peerLandedOn(%q,%q,%q,%q) = %v, want %v", c.version, c.phase, c.to, c.want, got, c.expect)
		}
	}
}
