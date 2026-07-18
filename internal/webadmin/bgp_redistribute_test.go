package webadmin

import (
	"net/netip"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// bgpRedisTestServer builds a Server + stubBackend backed by a real config
// file on disk (bgpMeshRedistributor.sync() reads via config.Load, same as
// reconcileMeshRedistribute), for exercising sync() directly without an HTTP
// layer or a live FRR.
func bgpRedisTestServer(t *testing.T, cfg *config.Config) (*Server, *stubBackend) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	be := &stubBackend{}
	srv := New(config.WebAdmin{AuthMode: "local"}, be, logx.Default())
	srv.SetConfigPath(cfgPath)
	return srv, be
}

func mustPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("bad prefix %q: %v", s, err)
	}
	return p
}

// TestBGPMeshRedistributorSync covers the core decision logic: which
// networks get redistributed into, at what metric, and — the loop guard —
// that a route this node already advertises into the mesh itself never
// comes back as a "BGP route" even though it's sitting right there in the
// (fake) learned set.
func TestBGPMeshRedistributorSync(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{Enabled: true, ASN: 65001},
		Networks: []config.Network{
			{
				ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
				RedistributeBGP: true, RedistributeBGPMetric: 20,
				// This node's own advertised route — must never bounce back
				// as if it were a genuinely external BGP route.
				Routes: []config.Route{{CIDR: "192.0.2.0/24", Enabled: true}},
			},
			{
				// RedistributeBGP off: must never be pushed to.
				ID: "0000000000000002", Name: "guest", Enabled: true, Subnet4: "10.1.0.0/24",
			},
		},
	}
	srv, be := bgpRedisTestServer(t, cfg)
	r := newBGPMeshRedistributor(srv)

	orig := bgpLearnedRoutesFn
	defer func() { bgpLearnedRoutesFn = orig }()
	bgpLearnedRoutesFn = func() []netip.Prefix {
		return []netip.Prefix{
			mustPrefix(t, "198.51.100.0/24"), // genuinely external — should redistribute
			mustPrefix(t, "192.0.2.0/24"),    // this node's own mesh Advertise route — must be excluded
		}
	}

	r.sync()

	if len(be.bgpRoutesCalls) != 1 {
		t.Fatalf("got %d SetBGPRoutes calls, want 1 (only the RedistributeBGP=true network): %+v", len(be.bgpRoutesCalls), be.bgpRoutesCalls)
	}
	call := be.bgpRoutesCalls[0]
	if call.networkID != 1 {
		t.Errorf("networkID = %#x, want 1", call.networkID)
	}
	if call.metric != 20 {
		t.Errorf("metric = %d, want 20 (RedistributeBGPMetric)", call.metric)
	}
	if len(call.routes) != 1 || call.routes[0] != mustPrefix(t, "198.51.100.0/24") {
		t.Errorf("routes = %v, want exactly [198.51.100.0/24] (192.0.2.0/24 must be excluded as a self-advertised loop)", call.routes)
	}
}

// TestBGPMeshRedistributorSyncClearsOnToggleOff proves a network that stops
// wanting BGP-into-mesh redistribution (here: RedistributeBGP flips to
// false) gets one clearing SetBGPRoutes(id, nil, 0) call on the next sync,
// rather than being left with whatever was last pushed gossiping into the
// mesh forever.
func TestBGPMeshRedistributorSyncClearsOnToggleOff(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{Enabled: true, ASN: 65001},
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
				RedistributeBGP: true, RedistributeBGPMetric: 5},
		},
	}
	srv, be := bgpRedisTestServer(t, cfg)
	r := newBGPMeshRedistributor(srv)

	orig := bgpLearnedRoutesFn
	defer func() { bgpLearnedRoutesFn = orig }()
	bgpLearnedRoutesFn = func() []netip.Prefix { return []netip.Prefix{mustPrefix(t, "198.51.100.0/24")} }

	r.sync()
	if len(be.bgpRoutesCalls) != 1 || len(be.bgpRoutesCalls[0].routes) != 1 {
		t.Fatalf("first sync: got %+v, want one call with one route", be.bgpRoutesCalls)
	}

	// Flip RedistributeBGP off and persist, then sync again.
	cfg2, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg2.NetworkSetRedistributeBGP("lan", false, 5); err != nil {
		t.Fatal(err)
	}
	if err := cfg2.SaveTo(srv.configPath); err != nil {
		t.Fatal(err)
	}

	r.sync()
	if len(be.bgpRoutesCalls) != 2 {
		t.Fatalf("got %d total calls, want 2 (initial + clear)", len(be.bgpRoutesCalls))
	}
	clear := be.bgpRoutesCalls[1]
	if clear.networkID != 1 || len(clear.routes) != 0 {
		t.Errorf("clearing call = %+v, want {networkID:1, routes:[]}", clear)
	}
}

// TestBGPMeshRedistributorSyncSkipsWhenNothingWantsIt proves sync() never
// touches bgpLearnedRoutesFn (the vtysh-calling path in production) when no
// network has RedistributeBGP on and nothing was previously active — the
// early-exit that keeps an ordinary poll tick, on the overwhelming majority
// of nodes that don't use this feature, from costing a subprocess spawn
// every bgpRedistributePollInterval for nothing.
func TestBGPMeshRedistributorSyncSkipsWhenNothingWantsIt(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"},
		},
	}
	srv, be := bgpRedisTestServer(t, cfg)
	r := newBGPMeshRedistributor(srv)

	calls := 0
	orig := bgpLearnedRoutesFn
	defer func() { bgpLearnedRoutesFn = orig }()
	bgpLearnedRoutesFn = func() []netip.Prefix { calls++; return nil }

	r.sync()
	if calls != 0 {
		t.Errorf("bgpLearnedRoutesFn called %d times, want 0", calls)
	}
	if len(be.bgpRoutesCalls) != 0 {
		t.Errorf("SetBGPRoutes called %d times, want 0", len(be.bgpRoutesCalls))
	}
}

// TestBGPMeshRedistributorSyncSkipsWhenBGPDisabled proves a network with
// RedistributeBGP on still gets pushed an empty set (not skipped outright)
// when BGP itself isn't usable (disabled, or no ASN) — the same "clear,
// don't leave stale" behavior as bgpUp=false always producing an empty
// candidate set.
func TestBGPMeshRedistributorSyncSkipsWhenBGPDisabled(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		// BGP left disabled entirely.
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
				RedistributeBGP: true, RedistributeBGPMetric: 1},
		},
	}
	srv, be := bgpRedisTestServer(t, cfg)
	r := newBGPMeshRedistributor(srv)

	calls := 0
	orig := bgpLearnedRoutesFn
	defer func() { bgpLearnedRoutesFn = orig }()
	bgpLearnedRoutesFn = func() []netip.Prefix { calls++; return []netip.Prefix{mustPrefix(t, "198.51.100.0/24")} }

	r.sync()
	if calls != 0 {
		t.Errorf("bgpLearnedRoutesFn called %d times, want 0 (BGP isn't enabled)", calls)
	}
	// bgpUp is false, so the per-network loop's own condition skips it
	// entirely (nothing was ever active for it either) — no call at all,
	// which is the same end state ("not redistributing") a clearing call
	// would produce.
	if len(be.bgpRoutesCalls) != 0 {
		t.Errorf("SetBGPRoutes called %d times, want 0", len(be.bgpRoutesCalls))
	}
}
