package webadmin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gravinet/internal/mesh"
)

// fakeRemoteApplyPeer stands in for a peer's handleUpgradeRemoteApply: a
// minimal TLS server (pushSourceTo always dials "https://") that drains the
// multipart body and replies once release is closed. Using a real peer here
// would mean actually compiling a source archive on every test run; this
// isolates the thing under test — how handleUpgradePush reports results as
// they arrive — from the peer's own build pipeline.
func fakeRemoteApplyPeer(t *testing.T, release <-chan struct{}, reply map[string]any) *httptest.Server {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-release
		writeJSON(w, http.StatusOK, reply)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func portOf(t *testing.T, rawURL string) uint16 {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return uint16(p)
}

// TestPushStreamsResultsAsPeersFinish is the regression test for the fix
// itself: a fast peer's result must reach the client before a slow peer in
// the same push has finished, and the slow peer's own result (plus the
// trailing summary) must still show up once it does. Before this fix,
// handleUpgradePush buffered every result behind wg.Wait() and the fast
// peer's outcome was invisible until the slow one also completed — this
// test fails against that behavior (the first read blocks until the
// timeout) and passes against the streaming implementation.
func TestPushStreamsResultsAsPeersFinish(t *testing.T) {
	fastRelease := make(chan struct{})
	close(fastRelease) // the fast peer never actually waits
	fastSrv := fakeRemoteApplyPeer(t, fastRelease, map[string]any{"ok": true, "skipped": true, "already_on": "715"})

	slowRelease := make(chan struct{})
	slowSrv := fakeRemoteApplyPeer(t, slowRelease, map[string]any{"ok": true, "applied": "715", "restarting": true})

	be := &stubBackend{overlayAddr: netip.MustParseAddr("127.0.0.1")}
	be.managedPeers = []mesh.ManagedPeer{
		{NodeID: "fast", Overlay4: netip.MustParseAddr("127.0.0.1"), WebPort: portOf(t, fastSrv.URL), LastSeen: time.Now()},
		{NodeID: "slow", Overlay4: netip.MustParseAddr("127.0.0.1"), WebPort: portOf(t, slowSrv.URL), LastSeen: time.Now()},
	}
	srv := secServer(be)
	srv.upg = &UpgradeCtl{StateDir: t.TempDir()}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	cookie := sessionFor(t, ts)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	nf, _ := mw.CreateFormField("nodes")
	nf.Write([]byte(`["fast","slow"]`))
	sf, _ := mw.CreateFormFile("source", "gravinet-src.tgz")
	sf.Write(theSource())
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/upgrade/push", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200; body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", ct)
	}

	br := bufio.NewReader(resp.Body)
	firstLine := make(chan string, 1)
	firstErr := make(chan error, 1)
	go func() {
		line, err := br.ReadString('\n')
		if err != nil {
			firstErr <- err
			return
		}
		firstLine <- line
	}()

	// The fast peer already has its reply queued up; if this blocks, the
	// server is still waiting on the slow peer before writing anything.
	select {
	case line := <-firstLine:
		var res map[string]any
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			t.Fatalf("bad json line %q: %v", line, err)
		}
		if res["node"] != "fast" {
			t.Fatalf("first line = %v, want the fast peer's result first", res)
		}
		if res["ok"] != true || res["skipped"] != true {
			t.Fatalf("fast peer's result: %v", res)
		}
	case err := <-firstErr:
		t.Fatalf("reading the first line: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the fast peer's result never arrived while the slow peer was still running \u2014 " +
			"results are being buffered until the whole batch finishes, not streamed as each peer completes")
	}

	// Now let the slow peer finish and confirm its result, then the trailing
	// summary, both still arrive.
	close(slowRelease)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(rest)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d remaining line(s), want 2 (slow peer's result + summary): %q", len(lines), rest)
	}

	var slowRes map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &slowRes); err != nil {
		t.Fatalf("bad json line %q: %v", lines[0], err)
	}
	if slowRes["node"] != "slow" || slowRes["ok"] != true {
		t.Fatalf("slow peer's result: %v", slowRes)
	}

	var summary map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &summary); err != nil {
		t.Fatalf("bad summary line %q: %v", lines[1], err)
	}
	if summary["done"] != true {
		t.Fatalf("summary: %v, want done:true", summary)
	}
	if summary["pushed"] != float64(2) || summary["total"] != float64(2) {
		t.Fatalf("summary pushed/total: %v", summary)
	}
}

// TestUpgradeResultsSurviveTheLocalPhaseRedraw pins the lifetime of the
// per-peer rollout results on System > Upgrade.
//
// The results are the only record that a rollout produced: once the peers have
// restarted onto the new build, nothing on the server still distinguishes the
// ones that applied from the ones that were already current or the ones that
// failed and why. They used to be destroyed at the worst possible moment —
// applyLocal finishes the local phase and calls drawUpgrade, which rebuilds the
// section and takes the results box with it, so the list vanished exactly when
// the last peer reported in and there was finally something to read.
//
// Pinned on the source rather than by rendering, since nothing here executes
// the page: what is checked is that the three halves of the mechanism are all
// still present, because any one of them alone silently does nothing.
func TestUpgradeResultsSurviveTheLocalPhaseRedraw(t *testing.T) {
	// 1. a rebuilt section repopulates its results box from the stash
	if !strings.Contains(indexHTML, "if (host._upgradeResults) resBox.innerHTML = host._upgradeResults;") {
		t.Error("drawUpgrade no longer restores stashed rollout results; the local phase's redraw wipes them again")
	}
	// 2. something writes the stash before that redraw happens
	if !strings.Contains(indexHTML, "const stashResults = () => { if (resBox) host._upgradeResults = resBox.innerHTML; };") {
		t.Error("stashResults is gone; there is nothing for drawUpgrade to restore")
	}
	if !strings.Contains(indexHTML, "stashResults();\n          await applyLocal();") {
		t.Error("the results are no longer stashed before applyLocal, which is the redraw that destroys them")
	}
	// 3. and the stash is cleared when a new upgrade begins, so one rollout's
	//    results never appear above the next one's.
	if !strings.Contains(indexHTML, "host._upgradeResults = '';") {
		t.Error("a new push no longer clears the previous rollout's stashed results")
	}
}

// TestSelfNoteIsReachableOutsideThePeersTable pins that a note on this node's
// own id resolves through the shared lookup every node-name tooltip uses.
//
// PeerNotes is one map keyed by node id, and neither the config nor the engine
// ever treated this node's own id specially — PeerSetNotes accepts it and
// SelfPeer reads it back. Mesh > peers renders it in its own notes column, so
// a self note could be written and seen there. But peerNotesFor, which is what
// every other node-name cell in the UI reaches a note through, searched only
// peers and disabled_peers. Self is in neither: it arrives on the network as
// `self`. So a note on your own node was stored correctly and displayed in
// exactly one table, and was invisible everywhere else.
func TestSelfNoteIsReachableOutsideThePeersTable(t *testing.T) {
	fn := indexHTML[strings.Index(indexHTML, "function peerNotesFor(netId, nodeId){"):]
	fn = fn[:strings.Index(fn, "\n}")]
	if !strings.Contains(fn, "status.self") {
		t.Errorf("peerNotesFor never consults the network's self entry, so a note on this node's own id resolves to nothing outside Mesh > peers:\n%s", fn)
	}
	// It must still prefer a real peer entry: self is the fallback, checked
	// after peers and disabled_peers, not instead of them.
	iPeers := strings.Index(fn, "status.peers")
	iSelf := strings.Index(fn, "status.self")
	if iPeers == -1 || iSelf < iPeers {
		t.Error("peerNotesFor checks self before the peer list; a real peer's note must win over a same-id self lookup")
	}
}

// TestRolloutHintsAreSpacedFromTheResultLines pins the spacing between a
// rollout's per-peer result lines and the notices interleaved with them.
//
// .hint carries a *negative* top margin — it is shaped to tuck a caption up
// under a heading, which is what it does everywhere else in the UI. Dropped
// into the results list it pulled itself 6px into the line above, so the
// closing verdict arrived hard against the last peer's line and read as one
// more result rather than as the end of the rollout. The seeds notice had
// been given an inline margin-top to escape exactly this; the rule covers all
// of them, so no notice depends on remembering the override.
func TestRolloutHintsAreSpacedFromTheResultLines(t *testing.T) {
	i := strings.Index(indexHTML, ".up-push-results > .hint {")
	if i < 0 {
		t.Fatal("the results box no longer overrides .hint's margin; its negative top margin pulls every notice into the line above")
	}
	rule := indexHTML[i:]
	rule = rule[:strings.Index(rule, "}")]
	if strings.Contains(rule, "margin:-") || strings.Contains(rule, "margin-top:-") {
		t.Errorf("the override is itself negative, which is the thing being overridden: %s", strings.TrimSpace(rule))
	}
	// The progress count is the first child and already sits below the box's
	// own margin; another 10px above it would open a gap at the top instead.
	if !strings.Contains(indexHTML, ".up-push-results > .hint:first-child { margin-top:0; }") {
		t.Error("the first-child exemption is gone; the progress line gains a top margin it does not need")
	}
	// If a notice keeps its own inline margin it silently stops tracking the
	// rule, which is how the two ended up disagreeing in the first place.
	if strings.Contains(indexHTML, "class=\"hint\" style=\"margin-top:6px\">upgrading seeds") {
		t.Error("the seeds notice still carries an inline margin-top, so it no longer tracks the shared rule")
	}
}
