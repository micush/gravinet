package webadmin

import (
	"io"
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

// TestMeshTshootBundleRoundTrips checks the common case: self and every
// reachable peer succeed, and each one's text comes back under a distinct,
// hostname-derived member name — self gets the same "-local" suffix
// meshCaptureJob.bundle uses, so the two mesh-wide bundlers stay visually
// consistent about which member is "this node" at a glance.
func TestMeshTshootBundleRoundTrips(t *testing.T) {
	results := []meshTshootPeerResult{
		{Hostname: "gn-router1", Self: true, Txt: []byte("LOCAL BUNDLE TEXT")},
		{Hostname: "gn-office", NodeID: "n2", Txt: []byte("OFFICE BUNDLE TEXT")},
	}
	data, err := bundleMeshTshoot(results)
	if err != nil {
		t.Fatalf("bundleMeshTshoot: %v", err)
	}

	files := readTgz(t, data)
	if got := files["gn-router1-local.txt"]; got != "LOCAL BUNDLE TEXT" {
		t.Errorf("gn-router1-local.txt = %q, want %q (files: %v)", got, "LOCAL BUNDLE TEXT", files)
	}
	if got := files["gn-office.txt"]; got != "OFFICE BUNDLE TEXT" {
		t.Errorf("gn-office.txt = %q, want %q (files: %v)", got, "OFFICE BUNDLE TEXT", files)
	}
	if _, hasErrors := files["errors.txt"]; hasErrors {
		t.Error("errors.txt present despite no peer failures")
	}
}

// TestMeshTshootBundlePartialFailure checks that one peer erroring doesn't
// drop the others' bundles, and that the failure is named in errors.txt
// rather than silently vanishing — the same guarantee
// TestMeshCaptureBundlePartialFailure pins for the capture bundler.
func TestMeshTshootBundlePartialFailure(t *testing.T) {
	results := []meshTshootPeerResult{
		{Hostname: "gn-router1", Self: true, Txt: []byte("LOCAL BUNDLE TEXT")},
		{Hostname: "gn-laptop", NodeID: "n3", Err: errString("peer not reachable for management")},
	}
	data, err := bundleMeshTshoot(results)
	if err != nil {
		t.Fatalf("bundleMeshTshoot should still succeed overall with one working peer: %v", err)
	}

	files := readTgz(t, data)
	if got := files["gn-router1-local.txt"]; got != "LOCAL BUNDLE TEXT" {
		t.Errorf("successful peer's bundle missing/wrong: %q", got)
	}
	errTxt, ok := files["errors.txt"]
	if !ok {
		t.Fatal("errors.txt missing despite a failed peer")
	}
	if !strings.Contains(errTxt, "gn-laptop") || !strings.Contains(errTxt, "peer not reachable for management") {
		t.Errorf("errors.txt doesn't identify the failed peer/reason: %q", errTxt)
	}
}

// TestMeshTshootBundleAllFail checks the every-target-failed case returns an
// error and no archive at all, rather than a .tgz containing only
// errors.txt — mirrors TestMeshCaptureBundleAllFail.
func TestMeshTshootBundleAllFail(t *testing.T) {
	results := []meshTshootPeerResult{
		{Hostname: "gn-router1", Self: true, Err: errString("building bundle: disk full")},
	}
	data, err := bundleMeshTshoot(results)
	if data != nil {
		t.Error("archive produced despite every target failing")
	}
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("bundleMeshTshoot error = %v, want it to surface the one target's actual error", err)
	}
}

// TestMeshTshootBundleNameCollision checks that two peers whose hostnames
// sanitize to the same string still each get their own file instead of one
// silently overwriting the other — mirrors TestMeshCaptureBundleNameCollision.
func TestMeshTshootBundleNameCollision(t *testing.T) {
	results := []meshTshootPeerResult{
		{Hostname: "office node", NodeID: "n1", Txt: []byte("A")},
		{Hostname: "office/node", NodeID: "n2", Txt: []byte("B")},
	}
	data, err := bundleMeshTshoot(results)
	if err != nil {
		t.Fatalf("bundleMeshTshoot: %v", err)
	}
	files := readTgz(t, data)
	if len(files) != 2 {
		t.Fatalf("want 2 distinct members for colliding names, got %d: %v", len(files), files)
	}
	got := map[string]bool{}
	for name, content := range files {
		got[content] = true
		if !strings.HasPrefix(name, "office_node") {
			t.Errorf("unexpected member name %q for a sanitized collision", name)
		}
	}
	if !got["A"] || !got["B"] {
		t.Errorf("lost one peer's content in a name collision: %v", files)
	}
}

// errString is a trivial error for tests that only need Error() to return a
// specific string, without pulling in errors.New at every call site.
type errString string

func (e errString) Error() string { return string(e) }

// TestExtractSingleTxtMemberRoundTrips proves a bundle built by
// packTshootTgz (the exact shape a real peer's /api/tshoot returns) comes
// back byte-for-byte through extractSingleTxtMember — this is the join
// between the mesh-wide bundler and every individual node's existing,
// unmodified tshoot endpoint, so a break here would silently corrupt every
// peer's entry in the mesh-wide download while /api/tshoot itself kept
// working fine.
func TestExtractSingleTxtMemberRoundTrips(t *testing.T) {
	const content = "gravinet troubleshooting bundle\n\n========== NODE ==========\nhello\n"
	tgz, err := packTshootTgz("gravinet-tshoot-20260801-000000.txt", content, time.Now())
	if err != nil {
		t.Fatalf("packTshootTgz: %v", err)
	}
	got, err := extractSingleTxtMember(tgz)
	if err != nil {
		t.Fatalf("extractSingleTxtMember: %v", err)
	}
	if string(got) != content {
		t.Errorf("content did not round-trip:\ngot:  %q\nwant: %q", got, content)
	}
}

// TestExtractSingleTxtMemberFallsBackForPlainText covers handleTshoot's own
// documented fallback path (packTshootTgz failing serves the raw bundle
// text, unarchived) — a peer hit that path, or predates this feature and
// never archives at all, extractSingleTxtMember should hand the bytes back
// unchanged rather than failing the whole peer over an archive format that
// carries no real information loss either way.
func TestExtractSingleTxtMemberFallsBackForPlainText(t *testing.T) {
	const content = "gravinet troubleshooting bundle\n(archiving failed; this is unarchived plain text)\n"
	got, err := extractSingleTxtMember([]byte(content))
	if err != nil {
		t.Fatalf("extractSingleTxtMember: %v", err)
	}
	if string(got) != content {
		t.Errorf("plain-text fallback not passed through unchanged:\ngot:  %q\nwant: %q", got, content)
	}
}

// TestFetchTshootOneSelfUsesBuildTshootText proves the self leg of the
// fan-out goes through the exact same bundle-building code the single-node
// /api/tshoot endpoint uses (buildTshootText), rather than a second,
// possibly-drifting implementation.
func TestFetchTshootOneSelfUsesBuildTshootText(t *testing.T) {
	srv, be, _ := newTestServer(t)
	be.hostname = "gn-selfhost"

	got, err := srv.fetchTshootOne("", true)
	if err != nil {
		t.Fatalf("fetchTshootOne(self): %v", err)
	}
	want, _ := srv.buildTshootText()
	if string(got) != want {
		t.Error("fetchTshootOne(self) did not return buildTshootText's own output verbatim")
	}
	if !strings.Contains(string(got), "gravinet troubleshooting bundle") {
		t.Errorf("self bundle missing its own header: %q", got)
	}
}

// TestFetchTshootOneRemoteUnwrapsPeerBundle proves the remote leg dials a
// peer's ordinary, unmodified /api/tshoot (a bare httptest mux stands in for
// it, same pattern TestCaptureOnePeerReportsIfaceBeforeDeadline uses for the
// capture fan-out's remote leg) and correctly unwraps the .tgz it gets back
// down to the original text — not a second, tshoot-mesh-specific endpoint on
// the peer's side.
func TestFetchTshootOneRemoteUnwrapsPeerBundle(t *testing.T) {
	const peerContent = "gravinet troubleshooting bundle\n\n========== NODE ==========\nfrom the peer\n"
	peerTgz, err := packTshootTgz("gravinet-tshoot-20260801-000000.txt", peerContent, time.Now())
	if err != nil {
		t.Fatalf("packTshootTgz: %v", err)
	}

	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tshoot", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(peerTgz)
	})
	peer := httptest.NewTLSServer(mux) // TLS: fetchTshootOne always dials "https://", same as handleProxy/captureOnePeer
	defer peer.Close()

	peerURL, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	peerPort, err := strconv.Atoi(peerURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	srv, be, _ := newTestServer(t)
	be.overlayAddr = netip.MustParseAddr("127.0.0.1") // so resolveManagedTarget's SSRF guard (OverlayContains) accepts the loopback stand-in
	be.managedPeers = []mesh.ManagedPeer{{
		NodeID: "peer-b", Hostname: "gn-peerb",
		Overlay4: netip.MustParseAddr("127.0.0.1"), WebPort: uint16(peerPort),
		LastSeen: time.Now(), Connected: true,
	}}

	got, err := srv.fetchTshootOne("peer-b", false)
	if err != nil {
		t.Fatalf("fetchTshootOne(remote): %v", err)
	}
	if string(got) != peerContent {
		t.Errorf("remote bundle not unwrapped correctly:\ngot:  %q\nwant: %q", got, peerContent)
	}
	if gotPath != "/api/tshoot" {
		t.Errorf("dialed peer path = %q, want /api/tshoot", gotPath)
	}
}

// TestHandleMeshTshootEndToEnd exercises the actual registered route: log
// in, fan out to self plus one fake managed peer, and confirm the download
// is a valid outer .tgz containing both members with no errors.txt — the
// full path a real click of "All mesh peers" takes, not just the pieces in
// isolation above.
func TestHandleMeshTshootEndToEnd(t *testing.T) {
	const peerContent = "gravinet troubleshooting bundle\n\n========== NODE ==========\nfrom gn-peerb\n"
	peerTgz, err := packTshootTgz("gravinet-tshoot-20260801-000000.txt", peerContent, time.Now())
	if err != nil {
		t.Fatalf("packTshootTgz: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tshoot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(peerTgz)
	})
	peer := httptest.NewTLSServer(mux)
	defer peer.Close()

	peerURL, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	peerPort, err := strconv.Atoi(peerURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	_, be, ts := newTestServer(t)
	be.hostname = "gn-selfhost"
	be.overlayAddr = netip.MustParseAddr("127.0.0.1")
	be.managedPeers = []mesh.ManagedPeer{{
		NodeID: "peer-b", Hostname: "gn-peerb",
		Overlay4: netip.MustParseAddr("127.0.0.1"), WebPort: uint16(peerPort),
		LastSeen: time.Now(), Connected: true,
	}}

	resp := login(t, ts, "admin", "pw")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login should succeed, got %d", resp.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie from login")
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/tshoot/mesh", nil)
	req.AddCookie(cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/tshoot/mesh = %d, want 200", r.StatusCode)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	files := readTgz(t, body)
	if got := files["gn-selfhost-local.txt"]; !strings.Contains(got, "gravinet troubleshooting bundle") {
		t.Errorf("self member missing or wrong: %q (files: %v)", got, files)
	}
	if got := files["gn-peerb.txt"]; got != peerContent {
		t.Errorf("peer member = %q, want %q (files: %v)", got, peerContent, files)
	}
	if _, hasErrors := files["errors.txt"]; hasErrors {
		t.Errorf("errors.txt present despite both targets succeeding: %v", files)
	}
}

// TestHandleMeshTshootNoManagedPeersStillReturnsSelf proves a node with no
// managed peers at all (the common single-node case) still succeeds — the
// fan-out's target list always includes self, so "no peers to fan out to"
// is not the same as "nothing to download".
func TestHandleMeshTshootNoManagedPeersStillReturnsSelf(t *testing.T) {
	_, be, ts := newTestServer(t)
	be.hostname = "gn-lonely"

	resp := login(t, ts, "admin", "pw")
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/tshoot/mesh", nil)
	req.AddCookie(cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/tshoot/mesh with no managed peers = %d, want 200", r.StatusCode)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	files := readTgz(t, body)
	if _, ok := files["gn-lonely-local.txt"]; !ok {
		t.Errorf("self member missing when there are no managed peers to fan out to: %v", files)
	}
}
