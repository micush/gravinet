package transport

import (
	"errors"
	"net/netip"
	"testing"
)

// TestDualSendNilUDP is the regression guard for the fix that makes a
// disabled UDP underlay (Dual.UDP == nil, the primary_port=0 sentinel) safe:
// before this, Send unconditionally called d.UDP.Send(...), which would
// nil-pointer-panic on literally the first send attempt to any peer once UDP
// was ever turned off. It must now return errNoUDP instead, the same way a
// nil TLS with no fallback available already returns errNoFallback from
// DialFallback — an ordinary, non-fatal "try something else" signal, not a
// crash.
func TestDualSendNilUDP(t *testing.T) {
	to := netip.MustParseAddrPort("203.0.113.5:65432")

	// UDP nil, TLS nil (both off — Config.Validate wouldn't normally allow
	// this in production, but Send itself must still not panic).
	d := Dual{}
	if err := d.Send(to, []byte("x")); !errors.Is(err, errNoUDP) {
		t.Fatalf("Send with nil UDP and nil TLS = %v, want errNoUDP", err)
	}

	// UDP nil, TLS present but with no live connection to `to` yet: still no
	// panic, still errNoUDP — a caller (mesh.Engine.ensureFallback) is
	// responsible for dialing TLS before there's anything to route a send
	// through.
	d = Dual{TLS: &TLSTransport{}}
	if err := d.Send(to, []byte("x")); !errors.Is(err, errNoUDP) {
		t.Fatalf("Send with nil UDP, disconnected TLS = %v, want errNoUDP", err)
	}
}

// TestDualCloseNilUDP guards Close's existing nil-safety (already correct
// before this change, but worth pinning down now that a nil UDP is a real,
// reachable production state rather than a theoretical one).
func TestDualCloseNilUDP(t *testing.T) {
	d := Dual{}
	if err := d.Close(); err != nil {
		t.Fatalf("Close on an empty Dual = %v, want nil", err)
	}
}
