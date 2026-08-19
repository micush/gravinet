package webadmin

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"gravinet/internal/mesh"
)

// resolveManagedTarget used to read a peer's Overlay4 and consult Overlay6
// only when Overlay4 was absent — never when it was merely unreachable. Since
// it is the single chokepoint for peer management, remote shell, fan-out
// capture, fan-out tshoot and upgrade push, a dual-stack peer whose v4 overlay
// path was broken became unmanageable by all five at once even with a working
// v6 address in the same advertisement. These tests pin the TCP, and pin
// the guards that must survive it.

// liveOnly returns a probe dialer that "connects" only to the given hostports.
// A fake is used rather than real listeners because the v4-dead/v6-live case
// cannot be staged on a host without usable IPv6 — which is most CI containers,
// and precisely where this TCP needs covering.
func liveOnly(live ...string) func(string, time.Duration) (net.Conn, error) {
	set := map[string]bool{}
	for _, h := range live {
		set[h] = true
	}
	return func(hostport string, _ time.Duration) (net.Conn, error) {
		if !set[hostport] {
			return nil, errors.New("connection refused")
		}
		c, _ := net.Pipe()
		return c, nil
	}
}

func hp(ip netip.Addr, port int) string {
	return net.JoinHostPort(ip.String(), strconv.Itoa(port))
}

func dualStackPeer(v4, v6 netip.Addr, port int) mesh.ManagedPeer {
	return mesh.ManagedPeer{
		NodeID: "peer-1", Hostname: "gn-dual",
		Overlay4: v4, Overlay6: v6, WebPort: uint16(port),
		LastSeen: time.Now(), Connected: true,
	}
}

// TestResolveManagedTargetFallsBackToV6 is the bug itself: v4 advertised and
// valid but dead, v6 advertised and live. The old code returned the v4 address
// and every caller burned its full timeout on it.
func TestResolveManagedTargetFallsBackToV6(t *testing.T) {
	v4, v6, port := netip.MustParseAddr("10.99.0.7"), netip.MustParseAddr("fd00::7"), 8443
	s, be, _ := newTestServer(t)
	be.overlayAddrs = []netip.Addr{v4, v6}
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(v4, v6, port)}
	s.dialProbe = liveOnly(hp(v6, port)) // only v6 answers

	got, err := s.resolveManagedTarget("peer-1")
	if err != nil {
		t.Fatalf("resolveManagedTarget: %v", err)
	}
	if got.ip != v6 {
		t.Fatalf("ip = %v, want v6 %v — v4 is advertised but nothing answers on it", got.ip, v6)
	}
	if got.port != port {
		t.Fatalf("port = %d, want %d", got.port, port)
	}
}

// TestResolveManagedTargetPrefersV4WhenBothAnswer keeps the previous choice
// wherever it still works: only peers that were actually broken change path.
func TestResolveManagedTargetPrefersV4WhenBothAnswer(t *testing.T) {
	v4, v6, port := netip.MustParseAddr("10.99.0.7"), netip.MustParseAddr("fd00::7"), 8443
	s, be, _ := newTestServer(t)
	be.overlayAddrs = []netip.Addr{v4, v6}
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(v4, v6, port)}
	s.dialProbe = liveOnly(hp(v4, port), hp(v6, port))

	got, err := s.resolveManagedTarget("peer-1")
	if err != nil {
		t.Fatalf("resolveManagedTarget: %v", err)
	}
	if got.ip != v4 {
		t.Fatalf("ip = %v, want v4 %v when both answer", got.ip, v4)
	}
}

// TestResolveManagedTargetSingleCandidateIsNotProbed: with nothing to choose
// between there is nothing to learn, so a lone address is returned untouched
// even when dead. Behaviour identical to before the TCP existed — the
// caller still produces its own error, and no probe is spent first.
func TestResolveManagedTargetSingleCandidateIsNotProbed(t *testing.T) {
	v4 := netip.MustParseAddr("10.99.0.7")
	s, be, _ := newTestServer(t)
	be.overlayAddrs = []netip.Addr{v4}
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(v4, netip.Addr{}, 8443)}
	probed := 0
	s.dialProbe = func(string, time.Duration) (net.Conn, error) { probed++; return nil, errors.New("refused") }

	got, err := s.resolveManagedTarget("peer-1")
	if err != nil {
		t.Fatalf("resolveManagedTarget: %v", err)
	}
	if got.ip != v4 {
		t.Fatalf("ip = %v, want %v", got.ip, v4)
	}
	if probed != 0 {
		t.Fatalf("probed %d times — a single candidate must not be probed", probed)
	}
}

// TestResolveManagedTargetNoCandidateAnswers: when neither family connects,
// the preferred one is returned rather than an error. A bare connect failing
// does not prove the peer is down (mid-restart, or a slow first hop on macOS's
// Network-Extension utun), and this function's job is to choose between
// addresses, not to declare the peer unreachable.
func TestResolveManagedTargetNoCandidateAnswers(t *testing.T) {
	v4, v6 := netip.MustParseAddr("10.99.0.7"), netip.MustParseAddr("fd00::7")
	s, be, _ := newTestServer(t)
	be.overlayAddrs = []netip.Addr{v4, v6}
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(v4, v6, 8443)}
	s.dialProbe = liveOnly() // nothing answers

	got, err := s.resolveManagedTarget("peer-1")
	if err != nil {
		t.Fatalf("resolveManagedTarget: %v — a dead peer must still resolve, so the caller reports its own failure", err)
	}
	if got.ip != v4 {
		t.Fatalf("ip = %v, want the preferred v4 %v", got.ip, v4)
	}
}

// TestResolveManagedTargetSSRFGuardIsPerCandidate: the SSRF check runs on each
// address independently, so a peer cannot smuggle a non-overlay address in via
// the family that happens to be tried second — and one bad family does not
// poison a good one. The metadata address must never be dialed at all.
func TestResolveManagedTargetSSRFGuardIsPerCandidate(t *testing.T) {
	evil, v6, port := netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("fd00::7"), 8443
	s, be, _ := newTestServer(t)
	be.overlayAddrs = []netip.Addr{v6} // only v6 is a real overlay address
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(evil, v6, port)}
	var dialed []string
	s.dialProbe = func(hostport string, _ time.Duration) (net.Conn, error) {
		dialed = append(dialed, hostport)
		return nil, errors.New("refused")
	}

	got, err := s.resolveManagedTarget("peer-1")
	if err != nil {
		t.Fatalf("resolveManagedTarget: %v", err)
	}
	if got.ip != v6 {
		t.Fatalf("ip = %v, want %v — the non-overlay v4 must be dropped, not dialed", got.ip, v6)
	}
	for _, d := range dialed {
		if strings.Contains(d, evil.String()) {
			t.Fatalf("dialed the non-overlay address %q", d)
		}
	}
}

// TestResolveManagedTargetAllCandidatesSpoofed: when nothing survives the SSRF
// check the error must still say so specifically. Collapsing it into a generic
// "not reachable" would hide a spoofing attempt behind a routing problem,
// which is the whole reason managedTargetError carries a status.
func TestResolveManagedTargetAllCandidatesSpoofed(t *testing.T) {
	s, be, _ := newTestServer(t)
	be.overlayAddrs = nil
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(
		netip.MustParseAddr("169.254.169.254"),
		netip.MustParseAddr("fe80::1"), 8443)}

	_, err := s.resolveManagedTarget("peer-1")
	mte, ok := err.(*managedTargetError)
	if !ok {
		t.Fatalf("err = %v (%T), want *managedTargetError", err, err)
	}
	if mte.status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (forbidden)", mte.status, http.StatusForbidden)
	}
}

// TestResolveManagedTargetPathHealthIsPerCandidate: this node's own overlay
// data plane can carry one family and not the other, so the health check runs
// per address too — a v4 path that is down must not veto a working v6 one.
func TestResolveManagedTargetPathHealthIsPerCandidate(t *testing.T) {
	v4, v6, port := netip.MustParseAddr("10.99.0.7"), netip.MustParseAddr("fd00::7"), 8443
	s, be, _ := newTestServer(t)
	be.overlayAddrs = []netip.Addr{v4, v6}
	be.overlayPathReasonFor = map[string]string{v4.String(): "no route to overlay v4"}
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(v4, v6, port)}
	s.dialProbe = liveOnly(hp(v6, port))

	got, err := s.resolveManagedTarget("peer-1")
	if err != nil {
		t.Fatalf("resolveManagedTarget: %v", err)
	}
	if got.ip != v6 {
		t.Fatalf("ip = %v, want %v", got.ip, v6)
	}
}

// TestResolveManagedTargetPathHealthAllDownKeepsReason: when no family has a
// healthy local path, the operator still gets the specific local cause on
// their own UI rather than a generic peer-unreachable.
func TestResolveManagedTargetPathHealthAllDownKeepsReason(t *testing.T) {
	s, be, _ := newTestServer(t)
	v4, v6 := netip.MustParseAddr("10.99.0.7"), netip.MustParseAddr("fd00::7")
	be.overlayAddrs = []netip.Addr{v4, v6}
	be.overlayPathReason = "overlay interface is down"
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(v4, v6, 8443)}

	_, err := s.resolveManagedTarget("peer-1")
	mte, ok := err.(*managedTargetError)
	if !ok {
		t.Fatalf("err = %v (%T), want *managedTargetError", err, err)
	}
	if mte.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", mte.status, http.StatusServiceUnavailable)
	}
	if !strings.Contains(mte.msg, "overlay interface is down") {
		t.Fatalf("msg = %q, want it to carry the local reason", mte.msg)
	}
}

// TestResolveManagedTargetCachesTheWinner: handleProxy runs on essentially
// every peer-UI interaction, so probing per call would put a TCP connect in
// front of each one. The winner is remembered; proven here by counting probes
// across two resolves.
func TestResolveManagedTargetCachesTheWinner(t *testing.T) {
	v4, v6, port := netip.MustParseAddr("10.99.0.7"), netip.MustParseAddr("fd00::7"), 8443
	s, be, _ := newTestServer(t)
	be.overlayAddrs = []netip.Addr{v4, v6}
	be.managedPeers = []mesh.ManagedPeer{dualStackPeer(v4, v6, port)}
	probes := 0
	s.dialProbe = func(hostport string, _ time.Duration) (net.Conn, error) {
		probes++
		if hostport != hp(v6, port) {
			return nil, errors.New("refused")
		}
		c, _ := net.Pipe()
		return c, nil
	}

	if got, err := s.resolveManagedTarget("peer-1"); err != nil || got.ip != v6 {
		t.Fatalf("first resolve = %v, %v; want %v", got, err, v6)
	}
	after := probes
	got, err := s.resolveManagedTarget("peer-1")
	if err != nil || got.ip != v6 {
		t.Fatalf("second resolve = %v, %v; want the cached %v", got, err, v6)
	}
	if probes != after {
		t.Fatalf("second resolve probed %d more times; the choice should be cached", probes-after)
	}
}

// TestResolveManagedTargetCacheExpires: a cached choice must not outlive the
// advertisement it came from, so the TTL has to stay under managedPeerTTL.
func TestResolveManagedTargetCacheExpires(t *testing.T) {
	if managedFamilyTTL >= managedPeerTTL {
		t.Fatalf("managedFamilyTTL (%v) must be shorter than managedPeerTTL (%v)", managedFamilyTTL, managedPeerTTL)
	}
	v6 := netip.MustParseAddr("fd00::7")
	s, _, _ := newTestServer(t)
	s.rememberManagedFamily("peer-1", v6)
	if _, ok := s.managedFamily("peer-1"); !ok {
		t.Fatal("a just-written cache entry should be readable")
	}
	s.managedFamilyMu.Lock()
	s.managedFamilyCache["peer-1"] = managedFamilyChoice{ip: v6, at: time.Now().Add(-managedFamilyTTL - time.Second)}
	s.managedFamilyMu.Unlock()
	if _, ok := s.managedFamily("peer-1"); ok {
		t.Fatal("an expired cache entry should not be returned")
	}
}
