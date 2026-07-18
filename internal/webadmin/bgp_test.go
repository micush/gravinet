package webadmin

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"gravinet/internal/config"
)

// fakeFileInfo is a minimal fs.FileInfo for driving the statFile seam without
// touching the real filesystem.
type fakeFileInfo struct{ dir bool }

func (f fakeFileInfo) Name() string       { return "vtysh" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0755 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

// withStatFile swaps the statFile seam for the duration of a test.
func withStatFile(t *testing.T, fn func(string) (fs.FileInfo, error)) {
	t.Helper()
	orig := statFile
	statFile = fn
	t.Cleanup(func() { statFile = orig })
}

func TestVtyshPathAndSupport(t *testing.T) {
	// None of the candidate paths exist → not supported.
	withStatFile(t, func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist })
	if _, ok := vtyshPath(); ok {
		t.Fatal("vtyshPath: expected not found when no path exists")
	}
	if bgpSupported() {
		t.Fatal("bgpSupported: expected false when vtysh absent")
	}

	// The second candidate exists as a real file → found + supported, and the
	// returned path is the first existing one in priority order.
	withStatFile(t, func(p string) (fs.FileInfo, error) {
		if p == bgpVtyshPaths[1] {
			return fakeFileInfo{}, nil
		}
		return nil, os.ErrNotExist
	})
	got, ok := vtyshPath()
	if !ok || got != bgpVtyshPaths[1] {
		t.Fatalf("vtyshPath: got (%q, %v), want (%q, true)", got, ok, bgpVtyshPaths[1])
	}
	if !bgpSupported() {
		t.Fatal("bgpSupported: expected true when vtysh present")
	}

	// A directory at the path must not count as the binary.
	withStatFile(t, func(string) (fs.FileInfo, error) { return fakeFileInfo{dir: true}, nil })
	if _, ok := vtyshPath(); ok {
		t.Fatal("vtyshPath: a directory must not count as vtysh")
	}
}

func TestParseBGPSummary(t *testing.T) {
	// Realistic FRR `show ip bgp summary json` shape: per-AFI object with a
	// routerId, local as, and an ip-keyed peers map. Covers an established v4
	// peer, an idle v4 peer (state present, no uptime), and a v6 peer whose
	// state is inferred from peerUptimeMsec.
	raw := []byte(`{
	  "ipv4Unicast": {
	    "routerId": "10.0.0.1",
	    "as": 65001,
	    "peers": {
	      "10.0.0.2": {"remoteAs": 65002, "state": "Established", "peerUptime": "01:23:45", "peerUptimeMsec": 5025000, "pfxRcd": 12},
	      "10.0.0.3": {"remoteAs": 65003, "state": "Active", "pfxRcd": 0}
	    }
	  },
	  "ipv6Unicast": {
	    "routerId": "10.0.0.1",
	    "as": 65001,
	    "peers": {
	      "fd00::2": {"remoteAs": 65010, "peerUptime": "00:05:00", "peerUptimeMsec": 300000, "pfxRcd": 3}
	    }
	  }
	}`)

	peers, routerID, localAS := parseBGPSummary(raw)
	if routerID != "10.0.0.1" {
		t.Errorf("routerID = %q, want 10.0.0.1", routerID)
	}
	if localAS != 65001 {
		t.Errorf("localAS = %d, want 65001", localAS)
	}
	if len(peers) != 3 {
		t.Fatalf("got %d peers, want 3", len(peers))
	}

	// Sorted by AFI then peer: ipv4 rows first (10.0.0.2, 10.0.0.3), then ipv6.
	if peers[0].Peer != "10.0.0.2" || peers[0].AFI != "ipv4Unicast" {
		t.Errorf("peers[0] = %+v, want 10.0.0.2/ipv4Unicast", peers[0])
	}
	if peers[0].RemoteAS != 65002 || peers[0].State != "Established" || peers[0].Prefixes != 12 || peers[0].Uptime != "01:23:45" {
		t.Errorf("established peer parsed wrong: %+v", peers[0])
	}
	if peers[1].Peer != "10.0.0.3" || peers[1].State != "Active" {
		t.Errorf("peers[1] = %+v, want 10.0.0.3/Active", peers[1])
	}
	// v6 peer: no explicit state, but positive uptime → inferred Established.
	if peers[2].Peer != "fd00::2" || peers[2].AFI != "ipv6Unicast" || peers[2].State != "Established" {
		t.Errorf("v6 peer state inference wrong: %+v", peers[2])
	}
}

func TestParseBGPSummaryEmptyOrGarbage(t *testing.T) {
	// Garbage or an empty document yields an empty (non-nil) peer slice, never
	// a panic — the endpoint still returns a well-formed available:true body.
	for _, in := range [][]byte{[]byte(""), []byte("not json"), []byte("{}"), []byte(`{"ipv4Unicast":{}}`)} {
		peers, rid, as := parseBGPSummary(in)
		if peers == nil {
			t.Errorf("peers nil for input %q; want empty slice", in)
		}
		if len(peers) != 0 || rid != "" || as != 0 {
			t.Errorf("input %q: got %d peers rid=%q as=%d, want empty", in, len(peers), rid, as)
		}
	}
}

// Field names (peer, local, interface, status, uptime, downtime, diagnostic)
// match bfdd_vty.c's __display_peer_json verbatim, straight from FRR's own
// source rather than guessed at — `show bfd peers json` is a flat array, one
// object per session, unlike show ip bgp summary json's per-AFI nesting.
func TestParseBFDPeers(t *testing.T) {
	raw := []byte(`[
	  {"multihop": false, "peer": "10.0.0.2", "local": "10.0.0.1", "id": 1, "remote-id": 2,
	   "passive-mode": false, "status": "up", "uptime": 125, "diagnostic": "ok",
	   "remote-diagnostic": "ok", "type": "configured", "receive-interval": 300, "transmit-interval": 300},
	  {"multihop": false, "peer": "10.0.0.3", "interface": "eth0", "id": 3, "remote-id": 0,
	   "status": "down", "downtime": 40, "diagnostic": "control-detect-time-expired",
	   "remote-diagnostic": "ok", "type": "dynamic"}
	]`)
	peers := parseBFDPeers(raw)
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(peers))
	}
	// Sorted by peer address: 10.0.0.2 before 10.0.0.3.
	if peers[0].Peer != "10.0.0.2" || peers[0].Local != "10.0.0.1" || peers[0].Status != "up" ||
		peers[0].Uptime != 125 || peers[0].Diagnostic != "ok" {
		t.Errorf("peers[0] parsed wrong: %+v", peers[0])
	}
	if peers[1].Peer != "10.0.0.3" || peers[1].Interface != "eth0" || peers[1].Status != "down" ||
		peers[1].Downtime != 40 || peers[1].Diagnostic != "control-detect-time-expired" {
		t.Errorf("peers[1] parsed wrong: %+v", peers[1])
	}
}

// TestHandleBGPTableNoVtysh covers the degrade path for the Monitor > BGP
// Peers "BGP Table" card's endpoint: with vtysh absent, it must report
// available=false with a human reason and empty text — same shape as
// handleBGP/handleBFD — rather than an error or a hang.
func TestHandleBGPTableNoVtysh(t *testing.T) {
	withStatFile(t, func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist })

	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleBGPTable(rr, httptest.NewRequest(http.MethodGet, "/api/bgp/table", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Available {
		t.Error("available should be false when vtysh is absent")
	}
	if out.Reason == "" {
		t.Error("reason should explain why the table is unavailable")
	}
	if out.Text != "" {
		t.Errorf("text = %q, want empty", out.Text)
	}
}

// meshRouteCIDRs is the source list behind the "Redistribute mesh routes" BGP
// toggle: exactly the CIDRs on the Mesh Routes page's Advertise table, for
// networks that are themselves enabled — disabled networks and disabled
// individual routes are excluded, duplicates across networks collapse to one
// entry, and the result is sorted for a stable frr.conf.
func TestMeshRouteCIDRs(t *testing.T) {
	cfg := &config.Config{
		Networks: []config.Network{
			{
				ID: "1", Name: "lan", Enabled: true,
				Routes: []config.Route{
					{CIDR: "10.2.0.0/24", Enabled: true},
					{CIDR: "10.1.0.0/24", Enabled: true},
					{CIDR: "10.9.0.0/24", Enabled: false}, // disabled route: excluded
				},
			},
			{
				ID: "2", Name: "other", Enabled: true,
				Routes: []config.Route{
					{CIDR: "10.1.0.0/24", Enabled: true}, // duplicate across networks: collapses
					{CIDR: "10.3.0.0/24", Enabled: true},
				},
			},
			{
				ID: "3", Name: "disabled-net", Enabled: false,
				Routes: []config.Route{
					{CIDR: "10.99.0.0/24", Enabled: true}, // whole network disabled: excluded
				},
			},
		},
	}
	got := meshRouteCIDRs(cfg)
	want := []string{"10.1.0.0/24", "10.2.0.0/24", "10.3.0.0/24"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}

func TestMeshRouteCIDRsEmpty(t *testing.T) {
	if got := meshRouteCIDRs(&config.Config{}); len(got) != 0 {
		t.Errorf("expected no routes from an empty config, got %v", got)
	}
}

// TestParseBGPLearnedRoutes covers the selection rule bgpMeshRedistributor
// depends on for loop-safety and correctness: only a bestpath+valid path
// counts, a prefix with no such path is skipped entirely (not "picked
// arbitrarily"), and an unparseable map key is dropped rather than panicking
// or propagating a zero-value prefix into the redistribution set.
func TestParseBGPLearnedRoutes(t *testing.T) {
	raw := []byte(`{
	  "routes": {
	    "10.0.1.0/24": [
	      {"valid": true, "bestpath": true},
	      {"valid": true, "bestpath": false}
	    ],
	    "10.0.2.0/24": [
	      {"valid": true, "bestpath": false},
	      {"valid": false, "bestpath": false}
	    ],
	    "10.0.3.0/24": [
	      {"valid": true, "bestpath": true}
	    ],
	    "not-a-prefix": [
	      {"valid": true, "bestpath": true}
	    ]
	  }
	}`)
	got := parseBGPLearnedRoutes(raw)
	want := map[string]bool{"10.0.1.0/24": true, "10.0.3.0/24": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v", got, want)
	}
	for _, p := range got {
		if !want[p.String()] {
			t.Errorf("unexpected prefix %s in result", p)
		}
	}
}

func TestParseBGPLearnedRoutesEmptyOrGarbage(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("not json"), []byte(`{"routes": {}}`)} {
		if got := parseBGPLearnedRoutes(raw); len(got) != 0 {
			t.Errorf("parseBGPLearnedRoutes(%q) = %v, want empty", raw, got)
		}
	}
}

func TestParseBFDPeersEmptyOrGarbage(t *testing.T) {
	for _, in := range [][]byte{[]byte(""), []byte("not json"), []byte("{}"), []byte("[]")} {
		peers := parseBFDPeers(in)
		if peers == nil {
			t.Errorf("peers nil for input %q; want empty slice", in)
		}
		if len(peers) != 0 {
			t.Errorf("input %q: got %d peers, want 0", in, len(peers))
		}
	}
}
