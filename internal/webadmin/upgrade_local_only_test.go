package webadmin

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

// TestUpgradeEndpointsRejectOverlayBypass proves the whole (now much smaller)
// /api/upgrade/* surface requires a real local session and does NOT accept
// authed()'s general Managed/Manager overlay bypass — the same property
// TestShellSettingRejectsOverlayBypass proves for the shell setting endpoint.
// Upgrades have no remote trigger at all, from anywhere, under any
// configuration, so no peer, however genuinely authorized as Manager, may
// reach any of these.
//
// This exists because that promise previously held for exactly one endpoint
// (the old handleUpgradeApply, which checked it inline) while
// state/fleet/rollout/local-apply/stage/rollback/home all relied solely on
// authed()'s bypass and so were still reachable by a Manager peer regardless
// of what the remote-access config said. This test is what would have caught
// that. state/fleet/rollout/apply (the manifest+sources variant) are gone
// entirely now — along with the mesh-distribution machinery they existed
// for — so they're not in this list; a request to any of them 404s, same as
// any other nonexistent route, which needs no dedicated test.
func TestUpgradeEndpointsRejectOverlayBypass(t *testing.T) {
	be := &stubBackend{
		managed:     true,
		overlayAddr: netip.MustParseAddr("10.42.0.5"),
		managerAddr: netip.MustParseAddr("10.42.0.5"), // caller IS a genuine manager peer
	}
	h := secServer(be).handler()

	endpoints := []struct {
		method, path string
	}{
		{"GET", "/api/upgrade"},
		{"POST", "/api/upgrade/local-apply"},
		{"POST", "/api/upgrade/stage"},
		{"POST", "/api/upgrade/stage-source"},
		{"POST", "/api/upgrade/rollback"},
	}
	for _, ep := range endpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		req.RemoteAddr = "10.42.0.5:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%s %s: Manager-peer overlay source with no session got %d, want 403 (upgradeLocalOnly must be the first check)", ep.method, ep.path, rec.Code)
		}
	}
}

// TestUpgradeDeletedEndpointsAreGone locks in that the mesh-distribution
// surface (fleet listing, rollout, peer artifact serving, the manifest+
// sources apply variant) was actually removed, not just made unreachable —
// a 404 here, not a 403, is the whole point: there is no handler left to gate.
func TestUpgradeDeletedEndpointsAreGone(t *testing.T) {
	be := &stubBackend{managed: true}
	h := secServer(be).handler()
	for _, path := range []string{
		"/api/upgrade/state", "/api/upgrade/fleet", "/api/upgrade/rollout",
		"/api/upgrade/blob", "/api/upgrade/apply",
	} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Errorf("%s: got %d, want 404 (this route should no longer be registered at all)", path, rec.Code)
		}
	}
}
