package webadmin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// upgradeTestServer builds a Server whose UpgradeCtl has a real state directory
// (uploads are spooled there) and a recording stub for the apply "Op" — so
// these tests exercise the remote-apply GATES and the digest check without
// actually compiling or swapping anything, which a unit test can't do. applied
// reports whether the apply op was reached, which is the precise line between
// "the gates let this through" and "they didn't".
func upgradeTestServer(t *testing.T, be *stubBackend, accept bool) (*Server, *atomic.Bool) {
	t.Helper()
	var applied atomic.Bool
	srv := secServer(be)
	srv.upg = &UpgradeCtl{
		StateDir:              t.TempDir(),
		AcceptManagerUpgrades: func() bool { return accept },
		Op: func(op string, body []byte) ([]byte, error) {
			if op == "apply" {
				applied.Store(true)
			}
			return []byte(`{"ok":true}`), nil
		},
	}
	return srv, &applied
}

// theSource is a stand-in for a gravinet source archive. Its bytes never reach
// an extractor in these tests — the apply op is stubbed — so what matters is
// only that the digest over them is computed and checked.
func theSource() []byte { return []byte("pretend gravinet source tarball") }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pushBody builds the multipart body a Manager sends: the declared digest
// first, then the archive bytes.
func pushBody(t *testing.T, sum string, source []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormField("sha256")
	fw.Write([]byte(sum))
	aw, _ := mw.CreateFormFile("source", "gravinet-src.tgz")
	aw.Write(source)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func postPush(t *testing.T, srv *Server, body *bytes.Buffer, ct, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/upgrade/remote-apply", body)
	req.Header.Set("Content-Type", ct)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)
	return rec
}

func directManager(addr string) *stubBackend {
	a := netip.MustParseAddr(addr)
	return &stubBackend{
		managed:             true,
		overlayAddr:         a,
		managerAddr:         a,
		managerNeighborAddr: a,
	}
}

// With accept_manager_upgrades off (the default), even a genuine
// directly-connected Manager over the overlay is refused, and the apply op is
// never reached. This is the property that keeps the feature invisible until
// explicitly enabled.
func TestRemoteApplyRejectedWhenNotOptedIn(t *testing.T) {
	srv, applied := upgradeTestServer(t, directManager("10.42.0.5"), false /* opted out */)
	src := theSource()

	body, ct := pushBody(t, digestOf(src), src)
	rec := postPush(t, srv, body, ct, "10.42.0.5:1234")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("opted-out node: got %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "accept_manager_upgrades") {
		t.Errorf("403 body should name the setting to enable: %s", rec.Body.String())
	}
	if applied.Load() {
		t.Fatal("apply op was reached despite opt-out — the gate leaked")
	}
}

// Opted in, and the source is a valid overlay address that IsManagerAddr would
// accept — but it is known as a manager only through gossip, with no live
// direct session. Ordinary management tolerates that; causing code to be built
// and run as root does not.
func TestRemoteApplyRejectsGossipOnlyManager(t *testing.T) {
	addr := netip.MustParseAddr("10.42.0.5")
	be := &stubBackend{
		managed:     true,
		overlayAddr: addr,
		managerAddr: addr, // gossip-labeled manager...
		// ...but managerNeighborAddr is unset: no live direct session.
	}
	srv, applied := upgradeTestServer(t, be, true)
	src := theSource()

	body, ct := pushBody(t, digestOf(src), src)
	rec := postPush(t, srv, body, ct, "10.42.0.5:1234")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("gossip-only manager: got %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gossip") {
		t.Errorf("the refusal should explain that gossip-level manager identity is insufficient: %s", rec.Body.String())
	}
	if applied.Load() {
		t.Fatal("apply reached for a gossip-only manager")
	}
}

// A request that did not arrive over the overlay is refused even if its
// address would otherwise match a direct manager neighbor.
func TestRemoteApplyRejectsNonOverlaySource(t *testing.T) {
	be := directManager("10.42.0.5")
	be.overlayAddr = netip.MustParseAddr("10.42.0.5")
	srv, applied := upgradeTestServer(t, be, true)
	src := theSource()

	body, ct := pushBody(t, digestOf(src), src)
	rec := postPush(t, srv, body, ct, "192.0.2.77:1234") // underlay address

	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-overlay source: got %d, want 403", rec.Code)
	}
	if applied.Load() {
		t.Fatal("apply reached from a non-overlay source")
	}
}

// The one correct remote path: opted in, live direct Manager neighbor, digest
// matches. This must reach apply — a test suite that only proves things are
// refused would pass just as well if the endpoint refused everything.
func TestRemoteApplyAcceptsDirectManagerNeighbor(t *testing.T) {
	srv, applied := upgradeTestServer(t, directManager("10.42.0.5"), true)
	src := theSource()

	body, ct := pushBody(t, digestOf(src), src)
	rec := postPush(t, srv, body, ct, "10.42.0.5:1234")

	if rec.Code != http.StatusOK {
		t.Fatalf("direct manager neighbor: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !applied.Load() {
		t.Fatal("apply not reached on the one path that should work")
	}
}

// The Manager's word about content is never trusted over the hash: bytes that
// don't match the declared digest are refused and never compiled.
func TestRemoteApplyRejectsDigestMismatch(t *testing.T) {
	srv, applied := upgradeTestServer(t, directManager("10.42.0.5"), true)

	// Declare the digest of one thing, send another.
	body, ct := pushBody(t, digestOf(theSource()), []byte("TOTALLY DIFFERENT BYTES"))
	rec := postPush(t, srv, body, ct, "10.42.0.5:1234")

	if rec.Code == http.StatusOK {
		t.Fatalf("digest mismatch was accepted (got 200) — the content integrity gate is broken")
	}
	if !strings.Contains(rec.Body.String(), "hashes to") {
		t.Errorf("the refusal should report both digests: %s", rec.Body.String())
	}
	if applied.Load() {
		t.Fatal("apply reached with a mismatched-digest archive")
	}
}

// Ordering is load-bearing, not cosmetic. A digest that arrives *after* the
// bytes it describes is a digest computed over content this node already
// accepted — which is not a check a substituted stream would fail. The handler
// must refuse the archive outright rather than buffer it and compare later.
func TestRemoteApplyRejectsSourceBeforeDigest(t *testing.T) {
	srv, applied := upgradeTestServer(t, directManager("10.42.0.5"), true)
	src := theSource()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	aw, _ := mw.CreateFormFile("source", "gravinet-src.tgz") // archive first
	aw.Write(src)
	fw, _ := mw.CreateFormField("sha256") // digest afterwards
	fw.Write([]byte(digestOf(src)))
	mw.Close()

	rec := postPush(t, srv, &buf, mw.FormDataContentType(), "10.42.0.5:1234")

	if rec.Code == http.StatusOK {
		t.Fatalf("an archive that arrived before its digest was accepted (got 200)")
	}
	if !strings.Contains(rec.Body.String(), "must arrive before") {
		t.Errorf("the refusal should explain the required ordering: %s", rec.Body.String())
	}
	if applied.Load() {
		t.Fatal("apply reached for an archive whose digest arrived too late")
	}
}

// A push carrying no archive at all must not reach apply.
func TestRemoteApplyRejectsEmptyPush(t *testing.T) {
	srv, applied := upgradeTestServer(t, directManager("10.42.0.5"), true)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormField("sha256")
	fw.Write([]byte(digestOf(theSource())))
	mw.Close()

	rec := postPush(t, srv, &buf, mw.FormDataContentType(), "10.42.0.5:1234")

	if rec.Code == http.StatusOK {
		t.Fatalf("a push with no source archive was accepted (got 200)")
	}
	if applied.Load() {
		t.Fatal("apply reached with no archive")
	}
}

// A locally logged-in operator can also reach remote-apply (so the endpoint is
// testable and usable on-box), independent of the manager path.
func TestRemoteApplyAcceptsLocalSession(t *testing.T) {
	srv, applied := upgradeTestServer(t, &stubBackend{managed: false}, true)
	src := theSource()

	body, ct := pushBody(t, digestOf(src), src)
	req := httptest.NewRequest("POST", "/api/upgrade/remote-apply", body)
	req.Header.Set("Content-Type", ct)
	tok := srv.newSession("admin")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("local session: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !applied.Load() {
		t.Fatal("apply not reached for a valid local session")
	}
}

// The manager-side push endpoint must refuse a request arriving over the
// overlay from a peer (even a manager), because driving a fleet rollout is done
// from the node you're on — "node A tells node B to push across B's mesh" is
// the two-managers confusion the proxy blocklist exists to prevent.
func TestPushIsLocalOnly(t *testing.T) {
	srv, _ := upgradeTestServer(t, directManager("10.42.0.5"), true)

	req := httptest.NewRequest("POST", "/api/upgrade/push", strings.NewReader("nodes=y"))
	req.RemoteAddr = "10.42.0.5:1234" // manager peer over overlay, no local session
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("push over overlay: got %d, want 403 (push is local-only)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "log in directly") {
		t.Errorf("push 403 should carry the local-only message: %s", rec.Body.String())
	}
}

// The local source upload is likewise local-only: it is the one door a peer
// must never reach, since it has no opt-in gate of its own.
func TestSourceUploadIsLocalOnly(t *testing.T) {
	srv, applied := upgradeTestServer(t, directManager("10.42.0.5"), true)

	req := httptest.NewRequest("POST", "/api/upgrade/source", bytes.NewReader(theSource()))
	req.RemoteAddr = "10.42.0.5:1234"
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("source upload over overlay: got %d, want 403", rec.Code)
	}
	if applied.Load() {
		t.Fatal("a peer reached the local source upload")
	}
}
