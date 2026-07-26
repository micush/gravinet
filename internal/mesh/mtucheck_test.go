package mesh

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"

	"gravinet/internal/logx"
	"gravinet/internal/protocol"
)

// mtuCheckEngine builds the minimum engine state checkOverlayMTUFits reads —
// a network whose device reports the given overlay MTU, and a path-MTU
// discovery ceiling — with the engine's logger redirected into a buffer so the
// test can assert on exactly what an operator would see.
func mtuCheckEngine(t *testing.T, overlayMTU, pmtuCeil int) (*Engine, *netState, *bytes.Buffer) {
	t.Helper()
	dev := newFakeDev("d")
	dev.mtu = overlayMTU
	e := NewEngine(Options{NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: pmtuCeil, Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: dev, Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	var buf bytes.Buffer
	e.log = logx.New(&buf, logx.LevelWarn)
	return e, e.netSnapshot()[1], &buf
}

// warnLines returns the non-empty lines captured from the engine logger.
func warnLines(buf *bytes.Buffer) []string {
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// The v659 defect: an overlay MTU above what the discovery ceiling could ever
// deliver. No peer, however good its path, can carry a full-size packet whole.
// This must warn, and must name a value that actually fixes it.
func TestOverlayMTUWarnsWhenNoPathCouldEverCarryIt(t *testing.T) {
	// 9216 overlay against the historical default ceiling of 9000: the best
	// any path yields is 8915, so every packet splits. This is exactly the
	// configuration that shipped through v658.
	e, ns, buf := mtuCheckEngine(t, 9216, 9000)
	e.checkOverlayMTUFits(ns)

	lines := warnLines(buf)
	if len(lines) != 1 {
		t.Fatalf("want exactly one warning, got %d: %v", len(lines), lines)
	}
	got := lines[0]
	for _, want := range []string{"9216", "8915", "2 fragments"} {
		if !strings.Contains(got, want) {
			t.Fatalf("warning missing %q: %s", want, got)
		}
	}
	// The number it tells the operator to use must genuinely resolve the
	// problem — re-running the check at that MTU has to go quiet.
	e2, ns2, buf2 := mtuCheckEngine(t, computeMaxInnerFrag(9000), 9000)
	e2.checkOverlayMTUFits(ns2)
	if l := warnLines(buf2); len(l) != 0 {
		t.Fatalf("the MTU the warning recommends still warns: %v", l)
	}
}

// The v658 mistake, pinned so it cannot come back: a mesh whose peers sit on
// genuinely different paths is NOT misconfigured. A node with a jumbo-capable
// underlay will fragment to a peer stuck behind a smaller path, and that is
// the entire point of application-layer fragmentation. Nothing here may warn
// about it — and in particular nothing may advise lowering the network MTU to
// suit the worst peer, which would throttle every other peer to match it.
func TestNoWarningForOrdinaryPerPeerFragmentation(t *testing.T) {
	// Overlay sized correctly for a 9000 ceiling. Individual peers may still
	// have 5140- or 1280-byte paths; that must stay silent.
	e, ns, buf := mtuCheckEngine(t, computeMaxInnerFrag(9000), 9000)
	e.checkOverlayMTUFits(ns)
	if l := warnLines(buf); len(l) != 0 {
		t.Fatalf("warned about a correctly-sized overlay: %v", l)
	}
}

// The check is per network start, not per peer or per probe: calling it for a
// second network must not depend on or disturb the first.
func TestOverlayMTUCheckIsIndependentPerNetwork(t *testing.T) {
	e, ns, buf := mtuCheckEngine(t, 9216, 9000)
	e.checkOverlayMTUFits(ns)
	e.checkOverlayMTUFits(ns)
	if l := warnLines(buf); len(l) != 2 {
		t.Fatalf("check should be stateless (one line per call), got %d: %v", len(l), l)
	}
}

// Shipping default must be silent out of the box — that is the whole point of
// v659, and this is the end-to-end proof of it.
func TestShippingDefaultsProduceNoMTUWarning(t *testing.T) {
	e, ns, buf := mtuCheckEngine(t, protocol.DefaultTunnelMTU, 9000)
	e.checkOverlayMTUFits(ns)
	if l := warnLines(buf); len(l) != 0 {
		t.Fatalf("default overlay MTU %d warns against the default ceiling: %v", protocol.DefaultTunnelMTU, l)
	}
}
