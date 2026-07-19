package webadmin

import (
	"net/netip"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// TestDeriveASNFromIPv4 checks the derivation lands in the 4-byte private
// range, is deterministic (same input, same output every time), and gives
// different addresses different ASNs in the common case (no claim of
// perfect collision-freedom — see the function's own doc comment for why
// that's fine for any realistically-sized overlay subnet). This is only
// ever used for this node's own local ASN now — a peer's is gossiped, not
// derived (see desiredAutoBGPNeighbors).
func TestDeriveASNFromIPv4(t *testing.T) {
	a := deriveASNFromIPv4(netip.MustParseAddr("10.42.0.5"))
	b := deriveASNFromIPv4(netip.MustParseAddr("10.42.0.5"))
	if a != b {
		t.Fatalf("not deterministic: %d != %d", a, b)
	}
	if a < autoBGPPrivateASNBase || a > 4294967294 {
		t.Fatalf("ASN %d outside the 4-byte private range [%d, 4294967294]", a, autoBGPPrivateASNBase)
	}
	c := deriveASNFromIPv4(netip.MustParseAddr("10.42.0.6"))
	if a == c {
		t.Errorf("10.42.0.5 and 10.42.0.6 derived the same ASN (%d) — unexpected for two addresses this close", a)
	}
	// An IPv4-in-6 address must derive the same as its plain v4 form —
	// autoBGPSelfTunnel4 always parses via netip.ParseAddr on a dotted-quad
	// string so this shouldn't occur in practice, but the function guards
	// for it explicitly (ip.Is4In6()) rather than silently returning 0 for
	// a value that unwraps to a perfectly good v4 address.
	mapped := netip.MustParseAddr("::ffff:10.42.0.5")
	if got := deriveASNFromIPv4(mapped); got != a {
		t.Errorf("4-in-6 form derived %d, want %d (same as plain v4)", got, a)
	}
	// A genuine v6 address has nothing sensible to derive as a v4 ASN.
	if got := deriveASNFromIPv4(netip.MustParseAddr("fd00::1")); got != 0 {
		t.Errorf("v6 address derived %d, want 0", got)
	}
}

// TestGatherAutoBGPPeersDedupesAcrossNetworks covers a peer that's a member
// of two networks this node shares with it: it must appear exactly once in
// the gathered result (not once per network), carrying the first tunnel
// address it finds for each family — including picking up a v4 address
// from the *second* network when the first one it appears on is v6-only —
// and its gossiped ASN, agreed on both networks here (as it should be:
// it's one fact about the peer itself, not something that legitimately
// differs per network).
func TestGatherAutoBGPPeersDedupesAcrossNetworks(t *testing.T) {
	be := &stubBackend{
		netIDs: []uint64{1, 2},
		peersByNet: map[uint64][]mesh.PeerInfo{
			1: {{NodeID: "peerA", Hostname: "alpha", Overlay6: "fd00:1::5", BGPASN: 4200000009}},
			2: {{NodeID: "peerA", Hostname: "alpha", Overlay4: "10.0.2.5", Overlay6: "fd00:2::5", BGPASN: 4200000009}},
		},
	}
	peers := gatherAutoBGPPeers(be)
	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1 (deduped by node id): %+v", len(peers), peers)
	}
	p := peers[0]
	if p.nodeID != "peerA" || p.hostname != "alpha" {
		t.Errorf("identity = %+v, want nodeID=peerA hostname=alpha", p)
	}
	if p.v4.String() != "10.0.2.5" {
		t.Errorf("v4 = %v, want 10.0.2.5 (from network 2, the first that had one)", p.v4)
	}
	if p.v6.String() != "fd00:1::5" {
		t.Errorf("v6 = %v, want fd00:1::5 (from network 1, the first that had one)", p.v6)
	}
	if p.asn != 4200000009 {
		t.Errorf("asn = %d, want 4200000009", p.asn)
	}
}

// TestDesiredAutoBGPNeighborsAddressFamilies covers v4-only, v6-only, and
// dual-stack peers each getting exactly the entries their own tunnel
// addresses support, all sharing that peer's one gossiped ASN — and that a
// peer with no gossiped ASN at all, or with tunnel addresses but ASN==0
// (no BGP there), is skipped entirely rather than half-configured.
func TestDesiredAutoBGPNeighborsAddressFamilies(t *testing.T) {
	v4only := autoBGPPeer{nodeID: "n1", hostname: "v4only", v4: netip.MustParseAddr("10.0.0.1"), asn: 4200000001}
	v6only := autoBGPPeer{nodeID: "n2", hostname: "v6only", v6: netip.MustParseAddr("fd00::2"), asn: 4200000002}
	dual := autoBGPPeer{nodeID: "n3", hostname: "dual", v4: netip.MustParseAddr("10.0.0.3"), v6: netip.MustParseAddr("fd00::3"), asn: 4200000003}
	noASN := autoBGPPeer{nodeID: "n4", hostname: "noasn", v4: netip.MustParseAddr("10.0.0.4")} // asn: 0 — no BGP there
	noAddr := autoBGPPeer{nodeID: "n5", hostname: "noaddr", asn: 4200000005}                   // no tunnel address at all

	got := desiredAutoBGPNeighbors([]autoBGPPeer{v4only, v6only, dual, noASN, noAddr})

	byPeer := map[string]config.BGPNeighbor{}
	for _, n := range got {
		byPeer[n.Peer] = n
	}

	if len(got) != 4 {
		t.Fatalf("got %d neighbors, want 4 (v4only:1 + v6only:1 + dual:2 + noASN:0 + noAddr:0): %+v", len(got), got)
	}
	if n, ok := byPeer["10.0.0.1"]; !ok || n.RemoteAS != 4200000001 {
		t.Errorf("v4-only peer's neighbor = %+v ok=%v, want RemoteAS 4200000001", n, ok)
	}
	v6n, ok := byPeer["fd00::2"]
	if !ok {
		t.Fatal("v6-only peer was skipped — it must be managed via its gossiped ASN, not left out")
	}
	if v6n.RemoteAS != 4200000002 {
		t.Errorf("v6-only peer's remote AS = %d, want 4200000002 (its gossiped ASN)", v6n.RemoteAS)
	}
	dualV4, ok4 := byPeer["10.0.0.3"]
	dualV6, ok6 := byPeer["fd00::3"]
	if !ok4 || !ok6 {
		t.Fatalf("dual-stack peer missing an entry: v4 ok=%v v6 ok=%v", ok4, ok6)
	}
	if dualV4.RemoteAS != 4200000003 || dualV6.RemoteAS != 4200000003 {
		t.Errorf("dual-stack peer's entries = v4:%d v6:%d, want both 4200000003 (one gossiped ASN, shared)", dualV4.RemoteAS, dualV6.RemoteAS)
	}
	if _, ok := byPeer["10.0.0.4"]; ok {
		t.Error("peer with no gossiped ASN (BGPASN=0) should have been skipped, not given a neighbor")
	}
	for _, n := range got {
		if n.Password != autoBGPPassword {
			t.Errorf("neighbor %+v: password = %q, want %q", n, n.Password, autoBGPPassword)
		}
		if !n.BFD {
			t.Errorf("neighbor %+v: BFD off, want on", n)
		}
		if n.Shutdown {
			t.Errorf("neighbor %+v: shut down, want enabled", n)
		}
	}
	if v6n.Description != "v6only" {
		t.Errorf("v6-only peer's description = %q, want its hostname %q", v6n.Description, "v6only")
	}
}

// TestMergeAutoBGPNeighborsAddRemovePreserveManual covers the three cases
// that matter: a newly-online peer's neighbor gets added (and reported in
// added), a gone-offline peer's autobgp-managed neighbor gets removed (and
// reported in removed, by address only), and a manually configured
// neighbor (no autobgp password) at an address that happens to match a
// mesh peer's tunnel address is left completely untouched either way — and
// never appears in added or removed, since AutoBGP never touched it.
func TestMergeAutoBGPNeighborsAddRemovePreserveManual(t *testing.T) {
	existing := []config.BGPNeighbor{
		// Still online next round — should be refreshed (same shape either way here).
		{Peer: "10.0.0.1", RemoteAS: 4200000001, Description: "alpha", Password: autoBGPPassword, BFD: true},
		// No longer in desired — the peer went offline — must be dropped.
		{Peer: "10.0.0.2", RemoteAS: 4200000002, Description: "beta", Password: autoBGPPassword, BFD: true},
		// A real manually-configured neighbor. Its address happens to be
		// exactly what AutoBGP would derive for a currently-online peer
		// (10.0.0.3) — it must be left alone regardless, never treated as
		// "already satisfies desired".
		{Peer: "10.0.0.3", RemoteAS: 999, Description: "manual-peer", Password: "hunter2", BFD: false, Shutdown: true},
	}
	desired := []config.BGPNeighbor{
		{Peer: "10.0.0.1", RemoteAS: 4200000001, Description: "alpha", Password: autoBGPPassword, BFD: true},
		{Peer: "10.0.0.3", RemoteAS: 4200000003, Description: "gamma", Password: autoBGPPassword, BFD: true}, // collides with the manual entry's address
		{Peer: "10.0.0.4", RemoteAS: 4200000004, Description: "delta", Password: autoBGPPassword, BFD: true}, // newly online
	}

	result, added, removed, changed := mergeAutoBGPNeighbors(existing, desired)
	if !changed {
		t.Fatal("changed = false, want true (an add and a removal both happened)")
	}
	byPeer := map[string]config.BGPNeighbor{}
	for _, n := range result {
		byPeer[n.Peer] = n
	}
	if len(result) != 3 {
		t.Fatalf("got %d neighbors, want 3 (10.0.0.1 kept, 10.0.0.2 dropped, 10.0.0.3 manual kept as-is, 10.0.0.4 added): %+v", len(result), result)
	}
	if _, ok := byPeer["10.0.0.2"]; ok {
		t.Error("10.0.0.2 (offline peer's old neighbor) should have been removed")
	}
	if n, ok := byPeer["10.0.0.4"]; !ok || n.RemoteAS != 4200000004 {
		t.Errorf("10.0.0.4 (newly online peer) should have been added with RemoteAS 4200000004, got %+v ok=%v", n, ok)
	}
	manual, ok := byPeer["10.0.0.3"]
	if !ok {
		t.Fatal("10.0.0.3 (manually configured neighbor) must never be removed")
	}
	if manual.Password != "hunter2" || manual.RemoteAS != 999 || manual.Description != "manual-peer" || !manual.Shutdown || manual.BFD {
		t.Errorf("10.0.0.3 was modified, want it left byte-for-byte alone: %+v", manual)
	}

	// added/removed drive the incremental vtysh apply path (applyBGPIncremental) —
	// exactly the newly-added and newly-removed entries, nothing about the
	// refreshed 10.0.0.1 (same shape either way) or the untouched manual entry.
	if len(removed) != 1 || removed[0] != "10.0.0.2" {
		t.Errorf("removed = %v, want [10.0.0.2]", removed)
	}
	if len(added) != 1 || added[0].Peer != "10.0.0.4" {
		t.Errorf("added = %+v, want just the 10.0.0.4 entry", added)
	}

	// A second merge against the exact same existing/desired must report no
	// change and be a byte-for-byte no-op — the steady-state case that lets
	// sync() skip writing/reloading FRR every poll once the mesh has settled.
	result2, added2, removed2, changed2 := mergeAutoBGPNeighbors(result, desired)
	if changed2 {
		t.Error("second merge against an already-reconciled list reported changed = true, want false")
	}
	if len(result2) != len(result) {
		t.Errorf("second merge changed the neighbor count: %d vs %d", len(result2), len(result))
	}
	if len(added2) != 0 || len(removed2) != 0 {
		t.Errorf("second merge should add/remove nothing, got added=%+v removed=%v", added2, removed2)
	}
}

// autoBGPTestServer mirrors bgpRedisTestServer: a Server + stubBackend
// backed by a real config file on disk, so sync() (which reads/writes via
// config.Load/mutateConfig, same as the redistributor) can be exercised
// directly.
func autoBGPTestServer(t *testing.T, cfg *config.Config, be *stubBackend) *Server {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/config.json"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	srv := New(config.WebAdmin{AuthMode: "local"}, be, logx.Default())
	srv.SetConfigPath(cfgPath)
	return srv
}

// TestAutoBGPReconcilerSyncDerivesAndCreates is the end-to-end happy path:
// AutoBGP on, nothing configured yet, one peer online with a gossiped ASN.
// sync() must derive this node's ASN/router-id from its own first tunnel
// IPv4, turn Enabled on, gossip that derived ASN back out via SetBGPASN,
// and create that peer's neighbor (v4 and v6) using its gossiped remote AS
// directly — no derivation involved for the peer's side at all.
func TestAutoBGPReconcilerSyncDerivesAndCreates(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{AutoBGP: true},
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"},
		},
	}
	be := &stubBackend{
		netIDs:    []uint64{1},
		selfByNet: map[uint64]mesh.PeerInfo{1: {NodeID: "self-node-id", Hostname: "self", Overlay4: "10.0.0.1"}},
		peersByNet: map[uint64][]mesh.PeerInfo{1: {{
			NodeID: "peerA", Hostname: "alpha", Overlay4: "10.0.0.9", Overlay6: "fd00::9", BGPASN: 4200005555,
		}}},
	}
	srv := autoBGPTestServer(t, cfg, be)
	r := newAutoBGPReconciler(srv)
	r.sync()

	got, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !got.BGP.Enabled {
		t.Error("BGP.Enabled = false, want true (AutoBGP must turn it on)")
	}
	wantASN := deriveASNFromIPv4(netip.MustParseAddr("10.0.0.1"))
	if got.BGP.ASN != wantASN {
		t.Errorf("ASN = %d, want %d (derived from this node's own tunnel IPv4 10.0.0.1)", got.BGP.ASN, wantASN)
	}
	if got.BGP.RouterID != "10.0.0.1" {
		t.Errorf("RouterID = %q, want %q", got.BGP.RouterID, "10.0.0.1")
	}
	if len(got.BGP.Neighbors) != 2 {
		t.Fatalf("got %d neighbors, want 2 (peerA's v4 and v6): %+v", len(got.BGP.Neighbors), got.BGP.Neighbors)
	}
	for _, n := range got.BGP.Neighbors {
		if n.Password != "autobgp" || !n.BFD || n.Description != "alpha" || n.RemoteAS != 4200005555 {
			t.Errorf("neighbor %+v doesn't match expected shape (password=autobgp, bfd=true, description=alpha, remote_as=4200005555, its gossiped ASN)", n)
		}
	}
	// This node's own derived ASN must have been gossiped back out — the
	// point of SetBGPASN existing at all — even though this is the very
	// first pass and ASN was 0 a moment ago.
	if len(be.bgpASNCalls) == 0 || be.bgpASNCalls[len(be.bgpASNCalls)-1] != wantASN {
		t.Errorf("SetBGPASN calls = %v, want the last one to be %d", be.bgpASNCalls, wantASN)
	}
}

// TestAutoBGPReconcilerSyncRemovesOnDisconnect proves a peer's neighbor
// disappears within one sync() once it's no longer in ListPeers — the
// "goes offline" half of the spec — while a manually-configured neighbor
// coexisting in the same list survives untouched.
func TestAutoBGPReconcilerSyncRemovesOnDisconnect(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{
			AutoBGP: true, Enabled: true, ASN: 4200000001, RouterID: "10.0.0.1",
			Neighbors: []config.BGPNeighbor{
				{Peer: "192.0.2.1", RemoteAS: 65099, Description: "real-external-peer", Password: "s3cr3t", BFD: false, Shutdown: false},
			},
		},
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"},
		},
	}
	be := &stubBackend{
		netIDs:    []uint64{1},
		selfByNet: map[uint64]mesh.PeerInfo{1: {NodeID: "self-node-id", Hostname: "self", Overlay4: "10.0.0.1"}},
		peersByNet: map[uint64][]mesh.PeerInfo{
			1: {{NodeID: "peerA", Hostname: "alpha", Overlay4: "10.0.0.9", BGPASN: 4200009999}},
		},
	}
	srv := autoBGPTestServer(t, cfg, be)
	r := newAutoBGPReconciler(srv)
	r.sync()

	after1, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(after1.BGP.Neighbors) != 2 {
		t.Fatalf("after peer online: got %d neighbors, want 2 (the manual one + peerA's v4): %+v", len(after1.BGP.Neighbors), after1.BGP.Neighbors)
	}

	// Peer disconnects: no longer in ListPeers at all.
	be.peersByNet[1] = nil
	r.sync()

	after2, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(after2.BGP.Neighbors) != 1 {
		t.Fatalf("after peer offline: got %d neighbors, want 1 (peerA's removed, manual one remains): %+v", len(after2.BGP.Neighbors), after2.BGP.Neighbors)
	}
	remaining := after2.BGP.Neighbors[0]
	if remaining.Peer != "192.0.2.1" || remaining.Password != "s3cr3t" {
		t.Errorf("remaining neighbor = %+v, want the untouched manual one (192.0.2.1 / s3cr3t)", remaining)
	}
}

// TestAutoBGPReconcilerSyncOffWhenDisabled proves sync() never touches the
// BGP config at all when AutoBGP itself is off — a manually-configured
// BGP setup must be completely unaffected by this reconciler running in
// the background — but that it still gossips the manually-configured ASN
// via SetBGPASN, since that's owed to any AutoBGP peer regardless of
// whether AutoBGP is what produced it.
func TestAutoBGPReconcilerSyncOffWhenDisabled(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{
			AutoBGP: false, Enabled: true, ASN: 65001, RouterID: "203.0.113.1",
			Neighbors: []config.BGPNeighbor{{Peer: "192.0.2.1", RemoteAS: 65099, Password: "s3cr3t"}},
		},
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"},
		},
	}
	be := &stubBackend{
		netIDs:     []uint64{1},
		selfByNet:  map[uint64]mesh.PeerInfo{1: {NodeID: "self-node-id", Overlay4: "10.0.0.1"}},
		peersByNet: map[uint64][]mesh.PeerInfo{1: {{NodeID: "peerA", Overlay4: "10.0.0.9"}}},
	}
	srv := autoBGPTestServer(t, cfg, be)
	r := newAutoBGPReconciler(srv)
	r.sync()

	got, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.BGP.ASN != 65001 || got.BGP.RouterID != "203.0.113.1" {
		t.Errorf("BGP config changed while AutoBGP is off: asn=%d router_id=%q", got.BGP.ASN, got.BGP.RouterID)
	}
	if len(got.BGP.Neighbors) != 1 || got.BGP.Neighbors[0].Peer != "192.0.2.1" {
		t.Errorf("Neighbors changed while AutoBGP is off: %+v", got.BGP.Neighbors)
	}
	if len(be.bgpASNCalls) != 1 || be.bgpASNCalls[0] != 65001 {
		t.Errorf("SetBGPASN calls = %v, want exactly one call with the manually-configured ASN 65001", be.bgpASNCalls)
	}
}

// TestAutoBGPReconcilerSyncGossipsZeroWhenBGPDisabled proves a node with no
// BGP configured at all (or explicitly disabled) gossips ASN 0 — the
// "nothing to peer with here" signal a peer's desiredAutoBGPNeighbors
// treats as "skip this peer".
func TestAutoBGPReconcilerSyncGossipsZeroWhenBGPDisabled(t *testing.T) {
	cfg := &config.Config{
		PrimaryPort: 51820, EnableIPv4: true,
		BGP: config.BGPConfig{AutoBGP: false, Enabled: false, ASN: 65001}, // Enabled=false: the ASN, if any, doesn't count
		Networks: []config.Network{
			{ID: "0000000000000001", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"},
		},
	}
	be := &stubBackend{netIDs: []uint64{1}, selfByNet: map[uint64]mesh.PeerInfo{1: {NodeID: "self-node-id", Overlay4: "10.0.0.1"}}}
	srv := autoBGPTestServer(t, cfg, be)
	r := newAutoBGPReconciler(srv)
	r.sync()

	if len(be.bgpASNCalls) != 1 || be.bgpASNCalls[0] != 0 {
		t.Errorf("SetBGPASN calls = %v, want exactly one call with 0 (BGP not enabled here)", be.bgpASNCalls)
	}
}
