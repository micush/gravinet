package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func isClosed(d *fakeDev) bool {
	select {
	case <-d.closed:
		return true
	default:
		return false
	}
}

// TestRebuildOverlayDeviceSwapsAndReasserts proves the core of the incident
// fix: when the overlay interface is lost, rebuildOverlayDevice creates a fresh
// device via the injected factory, makes it the live device, closes the dead
// one, and re-applies the overlay address and base subnet route onto the new
// interface — so the data plane comes back on its own instead of leaking to the
// underlay until a manual restart.
func TestRebuildOverlayDeviceSwapsAndReasserts(t *testing.T) {
	d0 := newFakeDev("mesh-test0")
	d1 := newFakeDev("mesh-test0") // same name, as a real recreate would use
	var calls int
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID:      1,
		Name:    "n",
		Dev:     d0,
		Subnet4: netip.MustParsePrefix("10.20.0.0/24"),
		Self4:   netip.MustParseAddr("10.20.0.5"),
		NewDevice: func() (Device, []Queue, error) {
			calls++
			return d1, nil, nil
		},
	}}})
	ns := e.netSnapshot()[1]

	if ns.dev() != d0 {
		t.Fatal("precondition: live device should be the seeded one")
	}

	if err := e.rebuildOverlayDevice(ns); err != nil {
		t.Fatalf("rebuildOverlayDevice: %v", err)
	}

	if calls != 1 {
		t.Fatalf("factory called %d times, want 1", calls)
	}
	if ns.dev() != d1 {
		t.Fatal("live device was not swapped to the rebuilt one")
	}
	if !isClosed(d0) {
		t.Fatal("the dead device was not closed")
	}
	if got := d1.addr4(); got != netip.MustParseAddr("10.20.0.5") {
		t.Fatalf("overlay address not re-added to the new interface: got %v", got)
	}
	if !d1.hasRoute(netip.MustParsePrefix("10.20.0.0/24")) {
		t.Fatal("base subnet route not re-installed on the new interface")
	}
	if st := ns.dpStateGet(); st != dpHealthy {
		t.Fatalf("dpState left at %d, want dpHealthy", st)
	}
}

// TestRebuildAbortsWhileClosing proves the teardown race guard: once the
// network is marked closing, a rebuild must not install a fresh device (which
// tunLoop would then block reading forever, hanging wg.Wait). It returns
// errDataplaneClosing and leaves the live device untouched.
func TestRebuildAbortsWhileClosing(t *testing.T) {
	d0 := newFakeDev("mesh-test1")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID:        1,
		Name:      "n",
		Dev:       d0,
		Subnet4:   netip.MustParsePrefix("10.20.0.0/24"),
		Self4:     netip.MustParseAddr("10.20.0.5"),
		NewDevice: func() (Device, []Queue, error) { t.Fatal("factory must not run during teardown"); return nil, nil, nil },
	}}})
	ns := e.netSnapshot()[1]

	ns.dpMu.Lock()
	ns.dpState = dpClosing
	ns.dpMu.Unlock()

	if err := e.rebuildOverlayDevice(ns); err != errDataplaneClosing {
		t.Fatalf("rebuild during teardown: got %v, want errDataplaneClosing", err)
	}
	if ns.dev() != d0 {
		t.Fatal("live device changed despite closing state")
	}
}

// TestReconcileClosesMissingInterface proves the maintenance-tick belt: when
// the kernel no longer has the overlay interface, reconcileDataplane closes the
// live device so tunLoop's blocked Read unblocks and drives the rebuild. The
// fake device is named for an interface that cannot exist, so the InterfaceByName
// probe reliably reports it missing.
func TestReconcileClosesMissingInterface(t *testing.T) {
	dev := newFakeDev("gn-nonexistent-iface-zzz")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID:        1,
		Name:      "n",
		Dev:       dev,
		Subnet4:   netip.MustParsePrefix("10.20.0.0/24"),
		Self4:     netip.MustParseAddr("10.20.0.5"),
		NewDevice: func() (Device, []Queue, error) { return newFakeDev("gn-nonexistent-iface-zzz"), nil, nil },
	}}})
	ns := e.netSnapshot()[1]

	e.reconcileDataplane(ns, time.Now())

	if !isClosed(dev) {
		t.Fatal("reconcile did not close the live device for a missing interface")
	}
}

// TestReconcileNoFactoryIsNoop confirms the gate: without a NewDevice factory
// (tests / embedders that don't wire recreation) the reconcile does nothing,
// preserving the pre-existing behaviour and never touching the device.
func TestReconcileNoFactoryIsNoop(t *testing.T) {
	dev := newFakeDev("gn-nonexistent-iface-yyy")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID:      1,
		Name:    "n",
		Dev:     dev,
		Subnet4: netip.MustParsePrefix("10.20.0.0/24"),
		Self4:   netip.MustParseAddr("10.20.0.5"),
	}}})
	ns := e.netSnapshot()[1]

	e.reconcileDataplane(ns, time.Now())

	if isClosed(dev) {
		t.Fatal("reconcile touched the device even though no factory is configured")
	}
}

// TestEngineStopNeverRebuilds is the regression guard for the shutdown race
// fixed in cmd/gravinet/main.go: main.go used to close every network's TUN
// device itself, before calling engine.Stop(), on the theory that Stop()
// would otherwise deadlock waiting for tunLoop's blocked Read. It doesn't —
// Stop() already closes e.stop *before* it closes the device (see Stop's
// body) — but the pre-close broke shuttingDown(ns)'s ability to tell "we're
// intentionally closing this" from "the interface died," so tunLoopSerial
// took the rebuild branch (recoverDataplane) in the middle of an ordinary
// shutdown: a brand-new TUN device stood up, addressed, and routed, only to
// be torn down again a moment later — slow and OS-call-heavy enough to blow
// through the shutdown watchdog on a loaded host.
//
// This drives tunLoopSerial directly (not the full Start(), which would also
// launch handshake/maintenance goroutines this test has no interest in) and
// proves two things about the *correct* call order (Stop() alone, nothing
// pre-closed): the device-recreation factory is never invoked, and Stop()
// itself returns promptly rather than deadlocking — the exact hazard the
// removed pre-close was (mistakenly) guarding against.
func TestEngineStopNeverRebuilds(t *testing.T) {
	dev := newFakeDev("mesh-test-stop")
	var rebuilt bool
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID:      1,
		Name:    "n",
		Dev:     dev,
		Subnet4: netip.MustParsePrefix("10.20.0.0/24"),
		Self4:   netip.MustParseAddr("10.20.0.5"),
		NewDevice: func() (Device, []Queue, error) {
			rebuilt = true
			return newFakeDev("mesh-test-stop"), nil, nil
		},
	}}})
	ns := e.netSnapshot()[1]

	ns.wg.Add(1)
	go e.tunLoopSerial(ns)

	done := make(chan struct{})
	go func() {
		e.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("engine.Stop() did not return — deadlocked waiting on the TUN read loop")
	}

	if rebuilt {
		t.Fatal("Stop() triggered a device rebuild — the read loop mistook its own intentional close for interface loss")
	}
	if !isClosed(dev) {
		t.Fatal("Stop() did not close the network's device")
	}
}

// TestDeviceCloseBeforeStopSignalRebuilds is the inverse of
// TestEngineStopNeverRebuilds: it documents *why* the order matters by
// reproducing, in isolation, exactly what the removed main.go code used to
// do — close the device directly, without going through Engine.Stop() (so
// e.stop/ns.done are never signaled first). tunLoopSerial has no way to
// distinguish that from a real interface loss and correctly (by its own,
// unchanged contract) takes the rebuild branch. This isn't a bug in this
// package — self-healing from a genuinely lost interface is the point of
// dataplane.go — but it is exactly the trap the caller has to avoid falling
// into, which is what made the main.go ordering bug possible in the first
// place.
func TestDeviceCloseBeforeStopSignalRebuilds(t *testing.T) {
	d0 := newFakeDev("mesh-test-race")
	d1 := newFakeDev("mesh-test-race")
	rebuiltCh := make(chan struct{})
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID:      1,
		Name:    "n",
		Dev:     d0,
		Subnet4: netip.MustParsePrefix("10.20.0.0/24"),
		Self4:   netip.MustParseAddr("10.20.0.5"),
		NewDevice: func() (Device, []Queue, error) {
			close(rebuiltCh)
			return d1, nil, nil
		},
	}}})
	ns := e.netSnapshot()[1]

	ns.wg.Add(1)
	go e.tunLoopSerial(ns)

	// The hazard itself: closing the device with neither e.stop nor ns.done
	// signaled yet, exactly as the removed `for _, d := range devices { d.Close() }`
	// loop in main.go's shutdown() did before it called engine.Stop().
	d0.Close()

	select {
	case <-rebuiltCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a rebuild attempt after an unsignaled device close, got none")
	}

	// Let the loop exit cleanly rather than leaking it. Closing d1 directly
	// here (the way the first Close() above did for d0) would trigger yet
	// another rebuild attempt for the exact same reason this test exists —
	// e.stop/ns.done still wouldn't be signaled. Stop() signals shutdown
	// first, then closes whatever the current live device is (d1, post-swap),
	// so the loop takes the shuttingDown() exit instead.
	e.Stop()
}
