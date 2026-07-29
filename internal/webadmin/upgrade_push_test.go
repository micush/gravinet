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
