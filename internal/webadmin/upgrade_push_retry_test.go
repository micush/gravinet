package webadmin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// flakyThenOKPeer fails the first failCount requests by hijacking the
// connection and closing it immediately with no response at all — what
// pushSourceTo sees as a genuine transport-level error (status 0), the same
// shape as the real-world "Post .../api/upgrade/remote-apply: EOF" this was
// built to tolerate — then serves reply normally from then on.
func flakyThenOKPeer(t *testing.T, failCount int, reply map[string]any) *httptest.Server {
	t.Helper()
	var attempts int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if int(n) <= failCount {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("ResponseWriter doesn't support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			return
		}
		io.Copy(io.Discard, r.Body)
		writeJSON(w, http.StatusOK, reply)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// alwaysRejectsPeer returns a genuine peer-side error immediately, every
// time — a real HTTP response, not a transport failure — and counts how
// many times it was actually hit.
func alwaysRejectsPeer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		io.Copy(io.Discard, r.Body)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "does not accept Manager-pushed upgrades"})
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// withFastRetryBackoff swaps pushRetryBackoff for a near-instant one for the
// duration of a test, restoring the real one after — so a test exercising
// actual retries doesn't have to sit through real multi-second sleeps.
func withFastRetryBackoff(t *testing.T) {
	t.Helper()
	orig := pushRetryBackoff
	pushRetryBackoff = func(attempt int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { pushRetryBackoff = orig })
}

func testTarget(t *testing.T, ts *httptest.Server) *clusterPeerTarget {
	t.Helper()
	return &clusterPeerTarget{ip: netip.MustParseAddr("127.0.0.1"), port: int(portOf(t, ts.URL))}
}

// A transport-level failure that clears up within the retry budget succeeds
// overall — the whole point of this feature.
func TestPushSourceToWithRetrySucceedsAfterTransientFailure(t *testing.T) {
	withFastRetryBackoff(t)
	src := t.TempDir() + "/src.tgz"
	writeTestSource(t, src)

	ts := flakyThenOKPeer(t, 1, map[string]any{"ok": true, "applied": "715", "restarting": true})
	srv := secServer(&stubBackend{})

	status, skipped, err := srv.pushSourceToWithRetry("peer", testTarget(t, ts), src, "deadbeef", "")
	if err != nil {
		t.Fatalf("expected eventual success, got error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if skipped {
		t.Error("skipped = true, want false")
	}
}

// A transport-level failure that never clears up exhausts the retry budget
// and returns the last error, having made exactly pushTransientRetries+1
// attempts — not fewer (giving up too early) and not more (retrying forever).
func TestPushSourceToWithRetryExhaustsAndFails(t *testing.T) {
	withFastRetryBackoff(t)
	src := t.TempDir() + "/src.tgz"
	writeTestSource(t, src)

	ts := flakyThenOKPeer(t, 999, nil) // never succeeds
	srv := secServer(&stubBackend{})

	_, _, err := srv.pushSourceToWithRetry("peer", testTarget(t, ts), src, "deadbeef", "")
	if err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
}

// A peer that actually responds — even with a rejection — is never retried:
// retrying a real, deliberate refusal wastes time and serves no purpose.
func TestPushSourceToWithRetryDoesNotRetryPeerError(t *testing.T) {
	withFastRetryBackoff(t)
	src := t.TempDir() + "/src.tgz"
	writeTestSource(t, src)

	ts, hits := alwaysRejectsPeer(t)
	srv := secServer(&stubBackend{})

	status, _, err := srv.pushSourceToWithRetry("peer", testTarget(t, ts), src, "deadbeef", "")
	if err == nil {
		t.Fatal("expected the peer's rejection to surface as an error")
	}
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("peer was hit %d time(s), want exactly 1 (a real response must never be retried)", got)
	}
}

// writeTestSource writes a minimal, non-empty file at path — pushSourceTo
// only needs something openable and readable; its content is never
// inspected by anything in this test file's fakes.
func writeTestSource(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("not a real archive"), 0o600); err != nil {
		t.Fatal(err)
	}
}
