package mesh

import (
	"net/netip"
	"testing"
)

func TestHostAdvDecodeAndLearn(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	ps := &peerSession{net: ns, nodeID: "peer1"}

	// codec round-trip
	o, n, ip, ok := decodeHostAdd(encodeHostAdd("peer1", "web.local", netip.MustParseAddr("192.168.5.5"))[1:])
	if !ok || o != "peer1" || n != "web.local" || ip != netip.MustParseAddr("192.168.5.5") {
		t.Fatalf("codec round-trip wrong: %q %q %v %v", o, n, ip, ok)
	}

	// learn via the wire path
	e.onHostAdd(ps, encodeHostAdd("peer1", "web.local", netip.MustParseAddr("192.168.5.5"))[1:])
	ns.mu.RLock()
	h := ns.learnedHosts[hostKey("peer1", "web.local")]
	ns.mu.RUnlock()
	if h == nil || h.ip != netip.MustParseAddr("192.168.5.5") {
		t.Fatalf("record not learned: %+v", h)
	}

	// our own origin is ignored (we already have it from config)
	e.onHostAdd(ps, encodeHostAdd("self", "x.local", netip.MustParseAddr("1.2.3.4"))[1:])
	ns.mu.RLock()
	_, hasSelf := ns.learnedHosts[hostKey("self", "x.local")]
	ns.mu.RUnlock()
	if hasSelf {
		t.Error("self-origin record should be ignored")
	}

	// withdrawal removes it
	e.onHostDel(ps, encodeHostDel("peer1", "web.local")[1:])
	ns.mu.RLock()
	_, still := ns.learnedHosts[hostKey("peer1", "web.local")]
	ns.mu.RUnlock()
	if still {
		t.Error("record not withdrawn")
	}
}
