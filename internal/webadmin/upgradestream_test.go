package webadmin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// readSource returns a file from this package, for assertions about server
// code that has no equivalent of indexHTML to inspect.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// A 17-peer rollout reported "The push could not be sent: NetworkError when
// attempting to fetch resource" after the canary succeeded. The canary was
// already up to date and so returned in moments; the fourteen behind it each
// had to build, and during that the connection carried no bytes at all.
//
// Two causes, both about silence:
//
//   - WriteHeader does not put bytes on the wire. net/http buffers until the
//     handler writes a body or returns, so response headers were withheld for
//     the whole rollout, and fetch() does not resolve until headers arrive.
//   - ReadTimeout is 30s server-wide and applies to the connection, not to the
//     direction that is idle, so it tore down a connection whose handler was
//     working normally.
//
// Both were reproduced against a real net/http server before fixing: with a
// client that gives up waiting for headers, as a browser does, the request
// failed with a transport error and no response; with the flush and the
// deadline lifted, headers arrived immediately and the stream completed.

func pushHandlerSrc(t *testing.T) string {
	t.Helper()
	src := readSource(t, "upgrade_push.go")
	i := strings.Index(src, "func (s *Server) handleUpgradePush(")
	if i < 0 {
		t.Fatal("handleUpgradePush not found")
	}
	j := strings.Index(src[i+1:], "\nfunc ")
	return src[i : i+1+j]
}

// The header has to reach the browser before the first peer is waited on.
func TestPushFlushesHeaderBeforeWaitingOnPeers(t *testing.T) {
	h := pushHandlerSrc(t)
	wh := strings.Index(h, "w.WriteHeader(http.StatusOK)")
	// Anchored on the keepalive goroutine rather than on the results loop:
	// that goroutine also calls Flush and is declared before the loop, so a
	// check that merely looked for a Flush somewhere in between passed
	// happily with the header flush deleted.
	ka := strings.Index(h, "keepaliveDone := make(chan struct{})")
	if wh < 0 || ka < 0 {
		t.Fatal("the push handler's shape changed")
	}
	flush := strings.Index(h[wh:], "flusher.Flush()")
	if flush < 0 || wh+flush > ka {
		t.Error("no Flush between WriteHeader and the rest of the handler; headers are buffered until the first peer finishes and fetch() never resolves")
	}
}

// The connection's read deadline must be lifted for a response that
// legitimately takes minutes. Its purpose — stopping a slow-loris trickling a
// request — is already served, because the body was fully read by spoolUpload.
func TestLongUpgradeHandlersLiftTheReadDeadline(t *testing.T) {
	if !strings.Contains(pushHandlerSrc(t), "rc.SetReadDeadline(time.Time{})") {
		t.Error("handleUpgradePush does not lift the read deadline; a rollout longer than ReadTimeout has its connection torn down")
	}
	// op() runs the local build, which is minutes for the same reason.
	up := readSource(t, "upgrade.go")
	i := strings.Index(up, "func (s *Server) op(")
	if i < 0 {
		t.Fatal("op not found")
	}
	body := up[i : i+2000]
	if !strings.Contains(body, "rc.SetReadDeadline(time.Time{})") {
		t.Error("op() does not lift the read deadline; a local build longer than ReadTimeout fails the same way a push did")
	}
	if strings.Index(body, "SetReadDeadline") > strings.Index(body, "s.upg.Op(name, body)") {
		t.Error("the deadline is lifted after the build runs, which is too late to help it")
	}
	// The premise: if ReadTimeout is ever removed, these lifts are dead code
	// and this test is misleading rather than protective.
	if !regexp.MustCompile(`ReadTimeout:\s+\d+ \* time\.Second`).MatchString(readSource(t, "webadmin.go")) {
		t.Error("the server no longer sets ReadTimeout; the deadline lifts above are now unnecessary and should be revisited")
	}
}

// Keepalives keep bytes moving during the long silences between results, and
// must be serialised against the result writer: two goroutines on one
// ResponseWriter can interleave a keepalive into the middle of a result line,
// and the client parses one JSON object per line.
func TestPushKeepaliveIsSerialisedAndIgnoredByTheClient(t *testing.T) {
	h := pushHandlerSrc(t)
	if !strings.Contains(h, "pushKeepaliveInterval") {
		t.Fatal("no keepalive on the push stream")
	}
	if n := strings.Count(h, "writeMu.Lock()"); n < 3 {
		t.Errorf("only %d guarded writes; the keepalive, the per-peer results and the final summary must all take the lock", n)
	}
	if !strings.Contains(uiFuncSrc(t, "drawUpgrade"), "if (obj.keepalive) return;") {
		t.Error("the client does not ignore keepalive lines, so each one renders as a peer result")
	}
}
