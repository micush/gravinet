package webadmin

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// ---- auth ----

func TestPBKDF2Deterministic(t *testing.T) {
	a := pbkdf2SHA256([]byte("pw"), []byte("salt"), 1000, 32)
	b := pbkdf2SHA256([]byte("pw"), []byte("salt"), 1000, 32)
	if !bytes.Equal(a, b) {
		t.Fatal("PBKDF2 must be deterministic")
	}
	c := pbkdf2SHA256([]byte("pw2"), []byte("salt"), 1000, 32)
	if bytes.Equal(a, c) {
		t.Fatal("different passwords must yield different keys")
	}
	if len(a) != 32 {
		t.Fatalf("wrong key length: %d", len(a))
	}
}

func TestLocalAuthRoundTrip(t *testing.T) {
	cred, err := GenerateCredential("admin", "s3cret", 10000)
	if err != nil {
		t.Fatal(err)
	}
	auth := NewLocalAuth([]config.AdminUser{cred})
	if !auth.Authenticate("admin", "s3cret") {
		t.Fatal("correct password should authenticate")
	}
	if auth.Authenticate("admin", "wrong") {
		t.Fatal("wrong password must fail")
	}
	if auth.Authenticate("nobody", "s3cret") {
		t.Fatal("unknown user must fail")
	}
}

// ---- server ----

type stubBackend struct {
	banned                  []string
	editedBan               string
	editedNotes             string
	managed                 bool
	manager                 bool
	managerAddr             netip.Addr // stub: addresses matching this count as "caller is a manager"
	hostname                string
	managedPeers            []mesh.ManagedPeer
	overlayAddr             netip.Addr
	overlayPathReason       string // non-empty makes OverlayPathHealthy fail with this reason
	fwAddCalls              int
	fwDelCalls              int
	fwMoveCalls             int
	fwRules                 []mesh.FirewallRule
	fwExempts               []mesh.ExemptInfo
	fwObjects               []mesh.FirewallObject
	fwServices              []mesh.FirewallService
	fwObjSetCalls           int
	fwSvcSetCalls           int
	fwResetCounterCalls     []uint64
	fwResetCounterCallCount int
	disabledPeers           []mesh.DisabledPeerInfo
	resetCalls              []uint64
	floodKeyCalls           int
	floodKeyErr             error
	lastFloodNet            uint64
	lastFloodKeyB64         string
	lastFloodLabel          string
	lastFloodExpNs          int64
	lastFloodSlot           int
	retractKeyCalls         int
	retractKeyErr           error
	lastRetractKeyB64       string
	natClass                string // empty defaults to "cone" in NATStatusStrings, matching the old hardcoded value
	natPublic               string // empty defaults to "203.0.113.5:51820"
}

func (s *stubBackend) NetworkIDs() []uint64 { return []uint64{0x1234} }
func (s *stubBackend) Interfaces() []mesh.IfaceInfo {
	return []mesh.IfaceInfo{{NetworkID: 0x1234, Name: "lan", Iface: "mesh0"}}
}
func (s *stubBackend) NATStatusStrings() (string, string) {
	class, public := s.natClass, s.natPublic
	if class == "" {
		class = "cone"
	}
	if public == "" {
		public = "203.0.113.5:51820"
	}
	return class, public
}
func (s *stubBackend) ListPeers(uint64) []mesh.PeerInfo {
	return []mesh.PeerInfo{{NodeID: "peerX", Hostname: "hostx", Overlay4: "10.0.0.9"}}
}
func (s *stubBackend) ListBans(uint64) []mesh.BanInfo { return nil }
func (s *stubBackend) DisabledPeers(uint64) []mesh.DisabledPeerInfo {
	return s.disabledPeers
}
func (s *stubBackend) Routes(uint64) []mesh.RouteInfo { return nil }
func (s *stubBackend) BanNode(_ uint64, t, _ string) error {
	s.banned = append(s.banned, t)
	return nil
}
func (s *stubBackend) UnbanNode(uint64, string) error { return nil }
func (s *stubBackend) EditBanNotes(_ uint64, t, notes string) error {
	s.editedBan = t
	s.editedNotes = notes
	return nil
}
func (s *stubBackend) ForceUnban(uint64, string) error { return nil }
func (s *stubBackend) ResetNetwork(id uint64) error {
	s.resetCalls = append(s.resetCalls, id)
	return nil
}
func (s *stubBackend) FirewallRules(uint64) ([]mesh.FirewallRule, error) { return s.fwRules, nil }
func (s *stubBackend) FirewallExemptsFor(uint64) []mesh.ExemptInfo       { return s.fwExempts }
func (s *stubBackend) FirewallAdd(_ uint64, r mesh.FirewallRule, _ int) (mesh.FirewallRule, error) {
	s.fwAddCalls++
	return r, nil
}
func (s *stubBackend) FirewallDelete(uint64, []uint64) error  { s.fwDelCalls++; return nil }
func (s *stubBackend) FirewallMove(uint64, uint64, int) error { s.fwMoveCalls++; return nil }
func (s *stubBackend) FirewallObjectsList() ([]mesh.FirewallObject, error) {
	return s.fwObjects, nil
}
func (s *stubBackend) SetFirewallObjects(o []mesh.FirewallObject) error {
	s.fwObjects = o
	s.fwObjSetCalls++
	return nil
}
func (s *stubBackend) FirewallServicesList() ([]mesh.FirewallService, error) {
	return s.fwServices, nil
}
func (s *stubBackend) SetFirewallServices(v []mesh.FirewallService) error {
	s.fwServices = v
	s.fwSvcSetCalls++
	return nil
}
func (s *stubBackend) FirewallResetCounters(_ uint64, ids []uint64) error {
	s.fwResetCounterCallCount++
	s.fwResetCounterCalls = append(s.fwResetCounterCalls, ids...)
	return nil
}
func (s *stubBackend) FloodKey(netID uint64, keyB64, label string, expNano int64, slot int) error {
	s.floodKeyCalls++
	s.lastFloodNet, s.lastFloodKeyB64, s.lastFloodLabel, s.lastFloodExpNs, s.lastFloodSlot =
		netID, keyB64, label, expNano, slot
	return s.floodKeyErr
}
func (s *stubBackend) RetractKey(netID uint64, keyB64 string) error {
	s.retractKeyCalls++
	s.lastRetractKeyB64 = keyB64
	return s.retractKeyErr
}
func (s *stubBackend) ManagedPeers(time.Duration) []mesh.ManagedPeer {
	return s.managedPeers
}
func (s *stubBackend) Managed() bool { return s.managed }

// LogLevel: the stub reports a fixed level; handleLogLevel's writes go through
// mutateConfig, which the stub already exercises.
func (s *stubBackend) LogLevel() string { return "info" }
func (s *stubBackend) Manager() bool    { return s.manager }
func (s *stubBackend) IsManagerAddr(ip netip.Addr) bool {
	return s.managerAddr.IsValid() && ip == s.managerAddr
}
func (s *stubBackend) Hostname() string        { return s.hostname }
func (s *stubBackend) SelfID() string          { return "self-node-id" }
func (s *stubBackend) SelfOverlay() netip.Addr { return netip.MustParseAddr("10.99.0.1") }
func (s *stubBackend) SelfPeer(uint64) (mesh.PeerInfo, bool) {
	return mesh.PeerInfo{NodeID: "self-node-id", Hostname: s.hostname, Overlay4: "10.99.0.1"}, true
}
func (s *stubBackend) OverlayContains(ip netip.Addr) bool {
	return s.overlayAddr.IsValid() && ip == s.overlayAddr
}
func (s *stubBackend) OverlayReachable(ip netip.Addr) bool {
	return s.overlayAddr.IsValid() && ip == s.overlayAddr
}
func (s *stubBackend) OverlayPathHealthy(dst netip.Addr) (bool, string) {
	if s.overlayPathReason != "" {
		return false, s.overlayPathReason
	}
	return true, ""
}

func newTestServer(t *testing.T) (*Server, *stubBackend, *httptest.Server) {
	t.Helper()
	cred, _ := GenerateCredential("admin", "pw", 10000)
	cfg := config.WebAdmin{
		AuthMode: "local",
		Users:    []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900},
	}
	be := &stubBackend{}
	srv := New(cfg, be, logx.Default())
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return srv, be, ts
}

func login(t *testing.T, ts *httptest.Server, user, pass string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"User": user, "Pass": pass})
	resp, err := http.Post(ts.URL+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestUnauthenticatedRejected(t *testing.T) {
	_, _, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

// TestStatusIncludesSelfPeer verifies /api/status reports this node's own
// identity per network (via SelfPeer) alongside the ordinary peers list — the
// backend half of showing the current node in the web UI's peers table
// instead of it being the one node missing from its own peer list.
func TestStatusIncludesSelfPeer(t *testing.T) {
	_, be, ts := newTestServer(t)
	be.hostname = "myhost"

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
	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.AddCookie(cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()

	var status struct {
		Nets []struct {
			ID   string        `json:"id"`
			Self mesh.PeerInfo `json:"self"`
		} `json:"nets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if len(status.Nets) != 1 {
		t.Fatalf("expected 1 network, got %d", len(status.Nets))
	}
	self := status.Nets[0].Self
	if self.NodeID != "self-node-id" || self.Hostname != "myhost" || self.Overlay4 != "10.99.0.1" {
		t.Fatalf("unexpected self peer: %+v", self)
	}
}

func TestLoginThrottleLockout(t *testing.T) {
	_, _, ts := newTestServer(t)
	// Three wrong attempts, then the next is locked out (429).
	for i := 0; i < 3; i++ {
		r := login(t, ts, "admin", "nope")
		if r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, r.StatusCode)
		}
	}
	r := login(t, ts, "admin", "nope")
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 lockout, got %d", r.StatusCode)
	}
	// Even the correct password is locked out now.
	r = login(t, ts, "admin", "pw")
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 even for correct creds while locked, got %d", r.StatusCode)
	}
}

func TestLoginSessionAndAPI(t *testing.T) {
	_, be, ts := newTestServer(t)

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
		t.Fatal("login did not set a session cookie")
	}

	// Authenticated status returns the network.
	req, _ := http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.AddCookie(cookie)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("authed status: expected 200, got %d", r.StatusCode)
	}
	var status struct {
		Nets []struct {
			ID    string          `json:"id"`
			Peers []mesh.PeerInfo `json:"peers"`
		} `json:"nets"`
	}
	json.NewDecoder(r.Body).Decode(&status)
	// Zero-padded to 16 hex chars, matching every other network ID in the
	// system (config file, /api/config) — not "1234". That used to be the
	// literal bug: /api/status formatted ids without zero-padding, so an id
	// with a leading zero nibble wouldn't match the padded form stored in
	// config, and things like network deletion would fail with "no network
	// named" for a network that very much existed.
	if len(status.Nets) != 1 || status.Nets[0].ID != "0000000000001234" {
		t.Fatalf("unexpected status payload: %+v", status)
	}

	// Ban through the API reaches the backend.
	banBody, _ := json.Marshal(map[string]any{"Net": "1234", "Node": "peerX", "Notes": "test"})
	req, _ = http.NewRequest("POST", ts.URL+"/api/ban", bytes.NewReader(banBody))
	req.AddCookie(cookie)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("ban: expected 200, got %d", r.StatusCode)
	}
	if len(be.banned) != 1 || be.banned[0] != "peerX" {
		t.Fatalf("backend did not record ban: %v", be.banned)
	}

	// Editing a ban's notes through the API reaches the backend.
	notesBody, _ := json.Marshal(map[string]any{"Net": "1234", "Node": "peerX", "Notes": "updated"})
	req, _ = http.NewRequest("POST", ts.URL+"/api/ban/notes", bytes.NewReader(notesBody))
	req.AddCookie(cookie)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("ban/notes: expected 200, got %d", r.StatusCode)
	}
	if be.editedBan != "peerX" || be.editedNotes != "updated" {
		t.Fatalf("backend did not record notes edit: node=%q notes=%q", be.editedBan, be.editedNotes)
	}

	// Reset through the API reaches the backend with the resolved network id.
	resetBody, _ := json.Marshal(map[string]any{"Net": "1234"})
	req, _ = http.NewRequest("POST", ts.URL+"/api/network/reset", bytes.NewReader(resetBody))
	req.AddCookie(cookie)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("reset: expected 200, got %d", r.StatusCode)
	}
	if len(be.resetCalls) != 1 || be.resetCalls[0] != 0x1234 {
		t.Fatalf("backend did not record reset: %v", be.resetCalls)
	}

	// Reset with an unresolvable network is rejected before reaching the backend.
	badReset, _ := json.Marshal(map[string]any{"Net": "not-hex"})
	req, _ = http.NewRequest("POST", ts.URL+"/api/network/reset", bytes.NewReader(badReset))
	req.AddCookie(cookie)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("reset with bad net: expected 400, got %d", r.StatusCode)
	}
	if len(be.resetCalls) != 1 {
		t.Fatalf("bad reset request should not reach the backend: %v", be.resetCalls)
	}

	// Logout invalidates the session.
	req, _ = http.NewRequest("POST", ts.URL+"/api/logout", nil)
	req.AddCookie(cookie)
	http.DefaultClient.Do(req)
	req, _ = http.NewRequest("GET", ts.URL+"/api/status", nil)
	req.AddCookie(cookie)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: expected 401, got %d", r.StatusCode)
	}
}

func TestPingUnauthenticatedAndBootID(t *testing.T) {
	srv, _, ts := newTestServer(t)

	// Reachable WITHOUT a session (the UI polls it after logout/restart).
	resp, err := http.Get(ts.URL + "/api/ping")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ping without auth: expected 200, got %d", resp.StatusCode)
	}
	var body struct {
		OK   bool   `json:"ok"`
		Boot string `json:"boot"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode ping: %v", err)
	}
	if body.Boot == "" {
		t.Fatalf("ping returned empty boot id")
	}
	if body.Boot != srv.bootID {
		t.Fatalf("ping boot id %q != server bootID %q", body.Boot, srv.bootID)
	}

	// A different process (new Server) must report a different boot id, which is
	// how the admin UI detects a restart regardless of timing.
	srv2, _, _ := newTestServer(t)
	if srv2.bootID == srv.bootID {
		t.Fatalf("two server instances share a boot id %q; restart cannot be detected", srv.bootID)
	}
}

func TestSelfSignedCert(t *testing.T) {
	certPEM, keyPEM, err := genSelfSignedPEM("127.0.0.1:8443")
	if err != nil {
		t.Fatalf("self-signed cert generation failed: %v", err)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("generated cert is not a valid keypair: %v", err)
	}

	// selfSignedCert persists and then reuses the same cert across calls.
	dir := t.TempDir()
	s := &Server{cfg: config.WebAdmin{Listen: "127.0.0.1:8443"}, log: logx.Default(), configPath: filepath.Join(dir, "config.json")}
	c1, err := s.selfSignedCert()
	if err != nil {
		t.Fatalf("selfSignedCert: %v", err)
	}
	c2, err := s.selfSignedCert()
	if err != nil {
		t.Fatalf("selfSignedCert (reuse): %v", err)
	}
	if !bytes.Equal(c1.Certificate[0], c2.Certificate[0]) {
		t.Errorf("cert changed across calls; should be persisted and reused")
	}
}
