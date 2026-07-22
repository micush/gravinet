package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

func secServer(be *stubBackend) *Server {
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	return New(wcfg, be, logx.Default())
}

// TestAuthedOverlayBypassNeedsOverlaySource: on a managed node the credential-free
// overlay bypass must require a structural overlay source address. A request from
// any other source (e.g. an attacker's underlay IP a malicious peer advertised)
// must still be rejected, and the bypass must not apply at all when managed is off.
// It also proves the three distinct failure reasons are each reported precisely
// (previously all three collapsed into one generic "not authenticated", which left
// a peer-to-peer caller like speedtest with no way to tell "you're not in managed
// mode" apart from "your source address doesn't look like a real overlay peer").
// The caller here is always a manager (managerAddr matches), isolating the
// overlay-source dimension from the separate manager-gate tested in
// TestAuthedOverlayBypassNeedsCallerIsManager.
func TestAuthedOverlayBypassNeedsOverlaySource(t *testing.T) {
	be := &stubBackend{managed: true, overlayAddr: netip.MustParseAddr("10.42.0.5"), managerAddr: netip.MustParseAddr("10.42.0.5")}
	h := secServer(be).handler()
	do := func(remote string) (int, string) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		errStr, _ := out["error"].(string)
		return rec.Code, errStr
	}
	if code, _ := do("10.42.0.5:1234"); code != http.StatusOK {
		t.Fatalf("overlay source on managed node should be allowed; got %d", code)
	}
	code, msg := do("203.0.113.5:1234")
	if code != http.StatusUnauthorized {
		t.Fatalf("non-overlay source must be rejected; got %d", code)
	}
	if !strings.Contains(msg, "203.0.113.5") {
		t.Fatalf("error should name the actual observed source address: %q", msg)
	}
	if !strings.Contains(msg, "overlay subnet") {
		t.Fatalf("error should explain it's a subnet-membership failure, not just say \"not authenticated\": %q", msg)
	}

	be.managed = false
	code, msg = do("10.42.0.5:1234")
	if code != http.StatusUnauthorized {
		t.Fatalf("with managed off, overlay source must still log in; got %d", code)
	}
	if !strings.Contains(msg, "not in managed mode") {
		t.Fatalf("error should say this node isn't in managed mode, not the address-mismatch reason: %q", msg)
	}
}

// TestAuthedOverlayBypassNeedsCallerIsManager: being Managed is not enough on
// its own — the caller must also resolve to a node currently advertising
// Manager mode. A valid overlay source that isn't a manager gets a distinct,
// actionable error (not lumped in with "not authenticated" or the
// subnet-membership failure), and flipping the caller to a manager is what
// makes the bypass succeed.
func TestAuthedOverlayBypassNeedsCallerIsManager(t *testing.T) {
	be := &stubBackend{managed: true, overlayAddr: netip.MustParseAddr("10.42.0.5")}
	h := secServer(be).handler()
	do := func() (int, string) {
		req := httptest.NewRequest("GET", "/api/status", nil)
		req.RemoteAddr = "10.42.0.5:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		errStr, _ := out["error"].(string)
		return rec.Code, errStr
	}
	// Valid overlay source, but no manager caller configured yet: rejected.
	code, msg := do()
	if code != http.StatusUnauthorized {
		t.Fatalf("overlay source that isn't a manager must be rejected; got %d", code)
	}
	if !strings.Contains(msg, "manager mode") {
		t.Fatalf("error should explain the caller isn't in manager mode: %q", msg)
	}
	// Same source, now advertising manager: bypass succeeds.
	be.managerAddr = netip.MustParseAddr("10.42.0.5")
	if code, _ := do(); code != http.StatusOK {
		t.Fatalf("overlay source that is a manager should be allowed; got %d", code)
	}
}

// TestProxyRejectsNonOverlayTarget: the management proxy must refuse to connect
// to a managed peer whose advertised address is not a real overlay address
// (SSRF guard), while allowing a legitimate overlay target through the guard.
func TestProxyRejectsNonOverlayTarget(t *testing.T) {
	be := &stubBackend{
		managed:     true,
		overlayAddr: netip.MustParseAddr("127.0.0.1"), // stub treats only this as "in overlay"
		managedPeers: []mesh.ManagedPeer{
			{NodeID: "EVIL", Hostname: "evil", Overlay4: netip.MustParseAddr("10.99.0.1"), WebPort: 80, LastSeen: time.Now(), Connected: true},
			{NodeID: "GOOD", Hostname: "good", Overlay4: netip.MustParseAddr("127.0.0.1"), WebPort: 1, LastSeen: time.Now(), Connected: true},
		},
	}
	ts := httptest.NewServer(secServer(be).handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	proxyCode := func(node string) (int, string) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/proxy?node="+node+"&path=%2Fapi%2Fstatus", nil)
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b map[string]any
		json.NewDecoder(resp.Body).Decode(&b)
		errStr, _ := b["error"].(string)
		return resp.StatusCode, errStr
	}

	// Poisoned target (127.0.0.1) must be refused by the overlay guard.
	if code, _ := proxyCode("EVIL"); code != http.StatusForbidden {
		t.Fatalf("proxy to non-overlay target should be 403, got %d", code)
	}
	// Legitimate overlay target passes the guard (it then fails to *connect*,
	// since nothing is listening — but that's 502, not the 403 guard rejection).
	if code, _ := proxyCode("GOOD"); code == http.StatusForbidden {
		t.Fatalf("proxy to a valid overlay target must not be blocked by the SSRF guard (got 403)")
	}
}
