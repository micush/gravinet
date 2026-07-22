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
		NewDevice: func() (Device, error) {
			calls++
			return d1, nil
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
		NewDevice: func() (Device, error) { t.Fatal("factory must not run during teardown"); return nil, nil },
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
		NewDevice: func() (Device, error) { return newFakeDev("gn-nonexistent-iface-zzz"), nil },
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
