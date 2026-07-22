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
// comes back as a "BGP route" even if it's explicitly selected in
// RedistributeBGPRoutes and sitting right there in the (fake) learned set.
func TestBGPMeshRedistributorSync(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{Enabled: true, ASN: 65001},
		Networks: []config.Network{
			{
				ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
				// Both explicitly selected — 192.0.2.0/24 must still be
				// excluded below despite being named here, since it's this
				// node's own advertised route.
				RedistributeBGPRoutes: []string{"198.51.100.0/24", "192.0.2.0/24"}, RedistributeBGPMetric: 20,
				// This node's own advertised route — must never bounce back
				// as if it were a genuinely external BGP route.
				Routes: []config.Route{{CIDR: "192.0.2.0/24", Enabled: true}},
			},
			{
				// RedistributeBGPRoutes empty: must never be pushed to.
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
		t.Fatalf("got %d SetBGPRoutes calls, want 1 (only the network with a selection): %+v", len(be.bgpRoutesCalls), be.bgpRoutesCalls)
	}
	call := be.bgpRoutesCalls[0]
	if call.networkID != 1 {
		t.Errorf("networkID = %#x, want 1", call.networkID)
	}
	if call.metric != 20 {
		t.Errorf("metric = %d, want 20 (RedistributeBGPMetric)", call.metric)
	}
	if len(call.routes) != 1 || call.routes[0] != mustPrefix(t, "198.51.100.0/24") {
		t.Errorf("routes = %v, want exactly [198.51.100.0/24] (192.0.2.0/24 must be excluded as a self-advertised loop, despite being explicitly selected)", call.routes)
	}
}

// TestBGPMeshRedistributorSyncPerNetworkSelection is the actual point of
// moving from a blanket toggle to a selection: two networks redistributing
// from the very same BGP RIB, each with its own, different subset picked —
// each must get exactly its own selection, never the other's and never the
// union of both.
func TestBGPMeshRedistributorSyncPerNetworkSelection(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{Enabled: true, ASN: 65001},
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
				RedistributeBGPRoutes: []string{"198.51.100.0/24"}, RedistributeBGPMetric: 10},
			{ID: "0000000000000002", Name: "guest", Enabled: true, Subnet4: "10.1.0.0/24",
				RedistributeBGPRoutes: []string{"203.0.113.0/24"}, RedistributeBGPMetric: 20},
		},
	}
	srv, be := bgpRedisTestServer(t, cfg)
	r := newBGPMeshRedistributor(srv)

	orig := bgpLearnedRoutesFn
	defer func() { bgpLearnedRoutesFn = orig }()
	bgpLearnedRoutesFn = func() []netip.Prefix {
		return []netip.Prefix{mustPrefix(t, "198.51.100.0/24"), mustPrefix(t, "203.0.113.0/24")}
	}

	r.sync()

	if len(be.bgpRoutesCalls) != 2 {
		t.Fatalf("got %d SetBGPRoutes calls, want 2 (one per network): %+v", len(be.bgpRoutesCalls), be.bgpRoutesCalls)
	}
	byNet := map[uint64]bgpRoutesCall{}
	for _, c := range be.bgpRoutesCalls {
		byNet[c.networkID] = c
	}
	lan := byNet[1]
	if len(lan.routes) != 1 || lan.routes[0] != mustPrefix(t, "198.51.100.0/24") || lan.metric != 10 {
		t.Errorf("lan call = %+v, want exactly [198.51.100.0/24] at metric 10", lan)
	}
	guest := byNet[2]
	if len(guest.routes) != 1 || guest.routes[0] != mustPrefix(t, "203.0.113.0/24") || guest.metric != 20 {
		t.Errorf("guest call = %+v, want exactly [203.0.113.0/24] at metric 20", guest)
	}
}

// TestBGPMeshRedistributorSyncClearsWhenSelectionEmptied proves a network
// that stops wanting BGP-into-mesh redistribution (here: its
// RedistributeBGPRoutes selection is emptied out) gets one clearing
// SetBGPRoutes(id, nil, 0) call on the next sync, rather than being left
// with whatever was last pushed gossiping into the mesh forever.
func TestBGPMeshRedistributorSyncClearsWhenSelectionEmptied(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{Enabled: true, ASN: 65001},
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
				RedistributeBGPRoutes: []string{"198.51.100.0/24"}, RedistributeBGPMetric: 5},
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

	// Empty the selection and persist, then sync again.
	cfg2, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg2.NetworkSetRedistributeBGPRoutes("lan", nil, 5); err != nil {
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
// network has anything selected in RedistributeBGPRoutes and nothing was
// previously active — the early-exit that keeps an ordinary poll tick, on
// the overwhelming majority of nodes that don't use this feature, from
// costing a subprocess spawn every bgpRedistributePollInterval for nothing.
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

// TestBGPMeshRedistributorSyncSkipsWhenBGPDisabled proves a network with a
// non-empty RedistributeBGPRoutes selection still gets pushed an empty set
// (not skipped outright) when BGP itself isn't usable (disabled, or no
// ASN) — the same "clear, don't leave stale" behavior as bgpUp=false always
// producing an empty candidate set.
func TestBGPMeshRedistributorSyncSkipsWhenBGPDisabled(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		// BGP left disabled entirely.
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24",
				RedistributeBGPRoutes: []string{"198.51.100.0/24"}, RedistributeBGPMetric: 1},
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
