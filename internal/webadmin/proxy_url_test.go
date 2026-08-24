package webadmin

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// CodeQL's go/request-forgery flagged handleProxy: the outgoing URL was built
// as "https://" + hostport + path, and path comes straight off the query
// string. It was not exploitable — the host comes from resolveManagedTarget,
// which matches a node id against the live managed-peer set, takes the address
// and port from the peer record and then requires OverlayContains — but the
// concatenation made that safety conditional on a check several lines away
// rather than structural, and unprovable locally by a reader or a tool.
//
// v919 builds the URL from parts. These tests pin what that guarantees.

// proxyPeer stands up a peer that records the request line it received.
func proxyPeer(t *testing.T) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotPath, gotHost string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotHost = r.Host
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	t.Cleanup(ts.Close)
	return ts, &gotPath, &gotHost
}

// proxyThrough wires a manager A that lists the given peer as managed, and
// returns the response to a proxy call for the given ?path=.
func proxyThrough(t *testing.T, peer *httptest.Server, path string) *http.Response {
	t.Helper()
	cred, _ := GenerateCredential("admin", "pw", 10000)
	cfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	be := &stubBackend{hostname: "node-a"}
	be.overlayAddr = netip.MustParseAddr("127.0.0.1")
	be.managed = true

	pURL, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(pURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	be.managedPeers = []mesh.ManagedPeer{{
		NodeID:   "peer-b",
		WebPort:  uint16(port),
		Overlay4: netip.MustParseAddr("127.0.0.1"),
	}}
	srv := New(cfg, be, logx.Default())
	ts := httptest.NewTLSServer(srv.handler())
	t.Cleanup(ts.Close)

	req := httptest.NewRequest(http.MethodGet, "/api/proxy?node=peer-b&path="+url.QueryEscape(path), nil)
	rec := httptest.NewRecorder()
	srv.handleProxy(rec, req)
	return rec.Result()
}

// An absolute URL in ?path= must be refused. This is already true before v919
// — "https://…" fails the leading-slash check and "//host/…" fails the /api/
// prefix check — and the parse guard added alongside the URL rebuild is a
// backstop rather than the thing doing the work. The behaviour is worth
// pinning regardless of which layer enforces it, but this test does not
// demonstrate the new guard: removing it leaves the test passing.
func TestProxyRejectsAnAbsoluteURLAsPath(t *testing.T) {
	peer, _, _ := proxyPeer(t)
	for _, bad := range []string{
		"https://internal.example.com/api/status",
		"//internal.example.com/api/status",
		"http://127.0.0.1:1/api/status",
	} {
		resp := proxyThrough(t, peer, bad)
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("path %q was accepted (status %d); an absolute URL is never a proxyable API path", bad, resp.StatusCode)
		}
	}
}

// The host that goes out is the one resolveManagedTarget produced, never
// anything derived from the caller's input.
func TestProxyAlwaysDialsTheResolvedPeer(t *testing.T) {
	peer, _, gotHost := proxyPeer(t)
	resp := proxyThrough(t, peer, "/api/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200", resp.StatusCode)
	}
	pURL, _ := url.Parse(peer.URL)
	if *gotHost != pURL.Host {
		t.Errorf("request arrived with Host %q, want the resolved peer %q", *gotHost, pURL.Host)
	}
}

// Query strings still reach the peer intact: they were carried inside the
// concatenated string before, and are now a separate RawQuery field, which is
// exactly the kind of change that silently drops them.
func TestProxyPreservesTheQueryString(t *testing.T) {
	peer, gotPath, _ := proxyPeer(t)
	resp := proxyThrough(t, peer, "/api/logs?level=ERROR&n=50")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200", resp.StatusCode)
	}
	if *gotPath != "/api/logs?level=ERROR&n=50" {
		t.Errorf("peer received %q, want the path and query unchanged", *gotPath)
	}
}

// The authority is assigned by handleProxy, not concatenated. This is the
// structural property the CodeQL finding was about: a reader can see in one
// place that Host comes from the resolved target.
func TestProxyBuildsItsURLFromParts(t *testing.T) {
	src := readSource(t, "cluster.go")
	i := strings.Index(src, "func (s *Server) handleProxy(")
	if i < 0 {
		t.Fatal("handleProxy not found")
	}
	body := src[i : i+6000]
	// Scan code only: the comment above the fix quotes the old expression to
	// explain it, and matching that would fail against the very code it
	// documents.
	for _, line := range strings.Split(body, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "//") {
			continue
		}
		if strings.Contains(s, `"https://" + hostport`) {
			t.Errorf("the proxy URL is concatenated from the caller's path again (%s); build it from neturl.URL so the authority cannot come from input", s)
		}
	}
	if !strings.Contains(body, "Host:     hostport,") {
		t.Error("the outgoing URL's Host is not assigned from the resolved target")
	}
	if !strings.Contains(body, "req.URL.Host != hostport") {
		t.Error("no post-parse check that the request still targets the resolved peer")
	}
}
