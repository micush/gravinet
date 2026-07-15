package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// sessionFor logs in and returns the session cookie.
func sessionFor(t *testing.T, ts *httptest.Server) *http.Cookie {
	t.Helper()
	resp := login(t, ts, "admin", "pw")
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestClusterEndpoint(t *testing.T) {
	_, be, ts := newTestServer(t)
	be.managed = true
	be.manager = true
	be.hostname = "alpha-gw"
	be.managedPeers = []mesh.ManagedPeer{
		{
			NodeID: "peer1", Hostname: "node-b", Overlay4: netip.MustParseAddr("10.0.0.2"),
			WebPort: 8443, LastSeen: time.Now(), Connected: true, Manager: true,
		},
		{
			NodeID: "peer2", Hostname: "node-c", Overlay4: netip.MustParseAddr("10.0.0.3"),
			WebPort: 8443, LastSeen: time.Now(), Connected: true, Manager: false,
		},
	}
	c := sessionFor(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/api/cluster", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Managed      bool          `json:"managed"`
		Manager      bool          `json:"manager"`
		SelfHostname string        `json:"self_hostname"`
		Peers        []clusterPeer `json:"peers"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if !body.Managed {
		t.Error("expected managed=true")
	}
	if !body.Manager {
		t.Error("expected manager=true")
	}
	if body.SelfHostname != "alpha-gw" {
		t.Errorf("self_hostname = %q, want alpha-gw", body.SelfHostname)
	}
	if len(body.Peers) != 2 {
		t.Fatalf("unexpected peers: %+v", body.Peers)
	}
	byHost := map[string]clusterPeer{}
	for _, p := range body.Peers {
		byHost[p.Hostname] = p
	}
	// node-b is Managed and Manager: it must appear as manageable AND manager,
	// so the Speedtest "from" picker can offer it as a client.
	if b, ok := byHost["node-b"]; !ok || !b.Manageable || !b.Manager {
		t.Fatalf("node-b should be manageable and manager, got %+v", b)
	}
	// node-c is Managed but not Manager: manageable (a valid speedtest target)
	// but not manager (must not be offered as a speedtest client) — this is
	// exactly the peer that used to 401 when picked as the "from" node.
	if cp, ok := byHost["node-c"]; !ok || !cp.Manageable || cp.Manager {
		t.Fatalf("node-c should be manageable but not manager, got %+v", cp)
	}
}

// TestProxyRejectsManagedManagerPaths locks in that Managed/Manager mode can
// never be changed on a remote peer through the proxy, regardless of what a
// client sends — the server-side trust boundary, not just the frontend's own
// convention of never routing these two paths through /api/proxy in the first
// place (see LOCAL_API in the web UI). Checked with a query string appended
// too, since the frontend's own path-matching strips one before comparing and
// the server-side guard must do the same to not be trivially bypassed by one.
func TestProxyRejectsManagedManagerPaths(t *testing.T) {
	_, be, ts := newTestServer(t)
	be.managed = true
	be.managedPeers = []mesh.ManagedPeer{{NodeID: "p", Overlay4: netip.MustParseAddr("10.0.0.9"), WebPort: 8443, LastSeen: time.Now()}}
	c := sessionFor(t, ts)

	for _, path := range []string{"/api/managed", "/api/manager", "/api/managed?x=1"} {
		req, _ := http.NewRequest("POST", ts.URL+"/api/proxy?node=p&path="+url.QueryEscape(path), nil)
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("proxying %q must be rejected 403, got %d", path, resp.StatusCode)
		}
	}
}

// TestProxyBodyLimit is a regression test for the bug reported after capture
// was made proxyable: /api/capture/pcap must get a response-size cap large
// enough to cover the full rolling capture buffer, or a well-populated
// capture's download would still be silently truncated through the proxy —
// the exact failure mode this endpoint used to sidestep by never being
// proxied at all. Every other path keeps the ordinary, much smaller cap.
func TestProxyBodyLimit(t *testing.T) {
	if got := proxyBodyLimit("/api/status"); got != 8<<20 {
		t.Errorf("proxyBodyLimit(ordinary path) = %d, want %d", got, 8<<20)
	}
	pcapLimit := proxyBodyLimit("/api/capture/pcap")
	if pcapLimit <= capMaxBytes {
		t.Fatalf("capture pcap proxy limit (%d) must exceed capMaxBytes (%d), or a full buffer would still be truncated", pcapLimit, capMaxBytes)
	}
}

// TestProxyRefusesWhenLocalOverlayDown proves the manager-side fail-fast: when
// this node's own overlay data plane is down (the interface missing/down, as in
// the mcfed incident), the proxy refuses with a clear local 503 instead of
// dialing the peer's overlay address — which the OS would leak out the underlay,
// surfacing at the far end as a baffling "connection arrived from <underlay ip>"
// auth failure.
func TestProxyRefusesWhenLocalOverlayDown(t *testing.T) {
	_, be, ts := newTestServer(t)
	be.managed = true
	ov := netip.MustParseAddr("10.0.0.9")
	be.overlayAddr = ov // so OverlayContains(ov) passes and we reach the health guard
	be.managedPeers = []mesh.ManagedPeer{{NodeID: "p", Overlay4: ov, WebPort: 8443, LastSeen: time.Now()}}
	be.overlayPathReason = "overlay interface mesh0 is not present"
	c := sessionFor(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/api/proxy?node=p&path=/api/status", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("overlay-down manager should refuse with 503, got %d", resp.StatusCode)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	msg, _ := m["error"].(string)
	if !bytes.Contains([]byte(msg), []byte("cannot manage peers over the mesh")) || !bytes.Contains([]byte(msg), []byte("not present")) {
		t.Fatalf("expected a clear local overlay-down message, got: %q", msg)
	}
}

func TestProxySSRFGuard(t *testing.T) {
	_, be, ts := newTestServer(t)
	be.managed = true // no managed peers known
	c := sessionFor(t, ts)
	req, _ := http.NewRequest("POST", ts.URL+"/api/proxy?node=ghost&path=/api/config", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unknown peer should be rejected as 502, got %d", resp.StatusCode)
	}
}

func TestProxyOnlyApiPaths(t *testing.T) {
	_, be, ts := newTestServer(t)
	be.managed = true
	be.managedPeers = []mesh.ManagedPeer{{NodeID: "p", Overlay4: netip.MustParseAddr("10.0.0.9"), WebPort: 8443, LastSeen: time.Now()}}
	c := sessionFor(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/api/proxy?node=p&path=/etc/passwd", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-/api path must be refused, got %d", resp.StatusCode)
	}
}

// TestProxyRejectsTraversal ensures the /api/ prefix guard can't be escaped via
// ".." or percent-encoded traversal sequences.
func TestProxyRejectsTraversal(t *testing.T) {
	_, be, ts := newTestServer(t)
	be.managed = true
	be.overlayAddr = netip.MustParseAddr("10.0.0.9")
	be.managedPeers = []mesh.ManagedPeer{{NodeID: "p", Overlay4: netip.MustParseAddr("10.0.0.9"), WebPort: 8443, LastSeen: time.Now()}}
	c := sessionFor(t, ts)

	for _, path := range []string{
		"/api/../admin/secret",
		"/api/%2e%2e/admin",
		"/api/..%2fadmin",
		"/api/foo/../../etc",
		"/api/%2E%2E/x",
	} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/proxy?node=p&path="+path, nil)
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("traversal path %q must be rejected with 403, got %d", path, resp.StatusCode)
		}
	}
}

// TestHandleManagedAppliesLiveNoRestart locks in the fix: toggling managed
// mode goes through mutateConfig the same as any other edit, and
// engine.SetManaged applies to the running daemon inside that same reload —
// there's nothing left to restart, so the response must say restart:false
// (previously hardcoded true, left over from before SetManaged applied live).
// The reload hook here stands in for main.go's reloadFn, which is what
// actually calls engine.SetManaged in the real daemon; asserting b.managed
// flips confirms this test's stand-in reload ran, not just that the config
// file on disk changed.
func TestHandleManagedAppliesLiveNoRestart(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	be := &stubBackend{}
	srv := New(wcfg, be, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error {
		c, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		be.managed = c.Managed // stands in for reloadFn's engine.SetManaged(newCfg.Managed)
		return nil
	})
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(on bool) map[string]any {
		b, _ := json.Marshal(map[string]any{"on": on})
		req, _ := http.NewRequest("POST", ts.URL+"/api/managed", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	res := post(true)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("enabling managed mode was rejected: %+v", res)
	}
	if restart, _ := res["restart"].(bool); restart {
		t.Error("managed mode applies live; restart must be false")
	}
	if !be.managed {
		t.Fatal("reload hook did not run — engine.SetManaged equivalent never applied")
	}

	res = post(false)
	if restart, _ := res["restart"].(bool); restart {
		t.Error("disabling managed mode also applies live; restart must be false")
	}
	if be.managed {
		t.Fatal("managed mode should be off after disabling")
	}
}

// TestHandleManagerAppliesLiveNoRestart mirrors
// TestHandleManagedAppliesLiveNoRestart for the new Manager toggle: it goes
// through the same mutateConfig + reload path, and there's nothing to restart.
func TestHandleManagerAppliesLiveNoRestart(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{
		PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	be := &stubBackend{}
	srv := New(wcfg, be, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error {
		c, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		be.manager = c.Manager // stands in for reloadFn's engine.SetManager(newCfg.Manager)
		return nil
	})
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	post := func(on bool) map[string]any {
		b, _ := json.Marshal(map[string]any{"on": on})
		req, _ := http.NewRequest("POST", ts.URL+"/api/manager", bytes.NewReader(b))
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	res := post(true)
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("enabling manager mode was rejected: %+v", res)
	}
	if restart, _ := res["restart"].(bool); restart {
		t.Error("manager mode applies live; restart must be false")
	}
	if !be.manager {
		t.Fatal("reload hook did not run — engine.SetManager equivalent never applied")
	}

	res = post(false)
	if restart, _ := res["restart"].(bool); restart {
		t.Error("disabling manager mode also applies live; restart must be false")
	}
	if be.manager {
		t.Fatal("manager mode should be off after disabling")
	}
}

func TestOverlaySourceAuth(t *testing.T) {
	_, be, ts := newTestServer(t)
	// httptest clients connect from loopback; treat that as the "overlay" source.
	be.overlayAddr = netip.MustParseAddr("127.0.0.1")
	be.managerAddr = netip.MustParseAddr("127.0.0.1") // caller also counts as a manager

	// Not managed yet: no session => 401.
	resp, _ := http.Get(ts.URL + "/api/status")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unmanaged + no session should be 401, got %d", resp.StatusCode)
	}

	// Managed + overlay source + caller is a manager => allowed without a session.
	be.managed = true
	resp2, _ := http.Get(ts.URL + "/api/status")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("managed + overlay source + manager caller should be allowed, got %d", resp2.StatusCode)
	}

	// Managed + overlay source, but caller is NOT a manager => still 401.
	be.managerAddr = netip.Addr{}
	resp3, _ := http.Get(ts.URL + "/api/status")
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("managed + overlay source but non-manager caller should be 401, got %d", resp3.StatusCode)
	}
}
