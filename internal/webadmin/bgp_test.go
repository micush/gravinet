package webadmin

import (
	"io/fs"
	"os"
	"testing"
	"time"
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
