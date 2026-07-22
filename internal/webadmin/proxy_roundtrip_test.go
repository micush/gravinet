package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// TestProxyRoutesToCorrectPeer is a real, full round trip through
// handleProxy — two actual servers, an actual HTTPS dial between them
// (handleProxy always dials "https://", so B has to be a TLS test server for
// the proxy to reach it at all) — rather than unit-testing pieces of the
// proxy path in isolation. Directly answers "shouldn't the NAT banner (and
// everything else on the page) change when I pick a different peer in the
// management dropdown?": A proxies /api/status to B and the response must be
// B's own backend state, not A's own and not some mix of the two.
func TestProxyRoutesToCorrectPeer(t *testing.T) {
	// B: the managed target, with its own distinct identity and NAT status.
	credB, _ := GenerateCredential("admin", "pw", 10000)
	cfgB := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{credB},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	beB := &stubBackend{hostname: "node-b", natClass: "open", natPublic: "198.51.100.9:65432"}
	// httptest clients (here, A's own proxyClient dialing B) connect from
	// loopback — same convention TestOverlaySourceAuth already uses to stand
	// in for "arrived over the overlay" without a real mesh underneath it.
	beB.overlayAddr = netip.MustParseAddr("127.0.0.1")
	beB.managerAddr = netip.MustParseAddr("127.0.0.1") // B accepts A as a manager, no session needed
	beB.managed = true
	srvB := New(cfgB, beB, logx.Default())
	tsB := httptest.NewTLSServer(srvB.handler())
	defer tsB.Close()

	// A: the manager, with B listed as a managed peer at tsB's real address
	// — not a fake 10.0.0.x the way the guard-only tests above use, since
	// this one needs an actual dial to actually land somewhere.
	_, beA, tsA := newTestServer(t)
	beA.hostname = "node-a"
	beA.natClass = "symmetric"
	beA.natPublic = "203.0.113.5:51820"
	beA.overlayAddr = netip.MustParseAddr("127.0.0.1") // so OverlayContains(B's advertised ip) passes
	bURL, err := url.Parse(tsB.URL)
	if err != nil {
		t.Fatal(err)
	}
	bPort, err := strconv.Atoi(bURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	beA.managedPeers = []mesh.ManagedPeer{{
		NodeID: "node-b-id", Hostname: "node-b",
		Overlay4: netip.MustParseAddr("127.0.0.1"), WebPort: uint16(bPort),
		LastSeen: time.Now(),
	}}
	cA := sessionFor(t, tsA)

	get := func(path string) map[string]any {
		req, _ := http.NewRequest("GET", tsA.URL+path, nil)
		req.AddCookie(cA)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	// A's own status, unproxied: A's own nat class.
	selfStatus := get("/api/status")
	if selfStatus["nat_class"] != "symmetric" {
		t.Fatalf("A's own status: nat_class = %v, want symmetric", selfStatus["nat_class"])
	}

	// Proxied through A to B: must be B's own nat class and public endpoint,
	// not A's, and not a mix — this is the exact call the web UI makes when
	// gn-cush2 (say) is selected in the management dropdown.
	proxied := get("/api/proxy?node=node-b-id&path=" + url.QueryEscape("/api/status"))
	if proxied["nat_class"] != "open" {
		t.Errorf("proxied to B: nat_class = %v, want %q (B's own, not A's %q)", proxied["nat_class"], "open", "symmetric")
	}
	if proxied["public"] != "198.51.100.9:65432" {
		t.Errorf("proxied to B: public = %v, want B's own %q", proxied["public"], "198.51.100.9:65432")
	}
	nets, _ := proxied["nets"].([]any)
	if len(nets) != 1 {
		t.Fatalf("proxied to B: nets = %v, want one network entry", proxied["nets"])
	}
	net0, _ := nets[0].(map[string]any)
	self, _ := net0["self"].(map[string]any)
	if self["hostname"] != "node-b" {
		t.Errorf("proxied to B: self.hostname = %v, want node-b (B's own self, not A's)", self["hostname"])
	}
}
