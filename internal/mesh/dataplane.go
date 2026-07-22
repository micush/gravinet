package mesh

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Data-plane supervision.
//
// The overlay interface can disappear out from under a running engine without
// the process ever exiting: a driver reset, an `ip link del`, or VM/host
// network churn (libvirt tearing an interface down, a Wi-Fi driver bouncing)
// can destroy the tun while the control plane keeps running happily over the
// underlay socket. Before this file the only thing that ever rebuilt interface
// state was reassertOSState, and it fired solely on suspend/resume detection —
// so any non-resume loss left the node advertising itself in the mesh while its
// data plane was silently dead, and every packet aimed at an overlay address
// fell through to the default route and leaked out the underlay.
//
// Two triggers now cover it:
//
//   - tunLoop's blocking Read returns an error the moment the interface is
//     destroyed; that's the fastest signal we get, and tunLoop drives the
//     actual rebuild from there (recoverDataplane below). Keeping the rebuild
//     inside the one goroutine that owns the read means no second goroutine to
//     track against ns.wg and no race with teardown's wg.Wait.
//
//   - reconcileDataplane runs each maintenance tick as a belt: it asks the
//     kernel (net.InterfaceByName) whether the interface still exists, is up,
//     and still carries our overlay address. A stripped address/route on a
//     still-present interface (the macOS-resume case reassertOSState was
//     originally written for, but not limited to it) is repaired in place; a
//     wholly missing interface is handed back to tunLoop by closing the live
//     device so its Read unblocks.
//
// Both are gated on spec.NewDevice being set (only the real daemon wires a
// factory; tests that inject a fake Device and no factory keep the old
// read-error-exits behaviour untouched).

const (
	dpHealthy    = iota // interface believed good
	dpRebuilding        // a rebuild is in flight (tunLoop)
	dpClosing           // network is being torn down; no more rebuilds
)

const (
	dpRebuildBackoffStart = 500 * time.Millisecond
	dpRebuildBackoffMax   = 30 * time.Second
	// dpRouteReassertEvery bounds how often the healthy-path reconcile re-adds
	// the base subnet route defensively. net.InterfaceByName can confirm the
	// address is present but says nothing about routes, so a route stripped
	// while the address survived is invisible to the check; a low-rate
	// idempotent AddRoute covers it without querying the routing table.
	dpRouteReassertEvery = 2 * time.Minute
)

var (
	errDataplaneClosing = errors.New("overlay network is shutting down")
	errNoDeviceFactory  = errors.New("no overlay device factory configured")
)

// dev returns the current live overlay device (the one to Read/Write and to
// install addresses and routes on). It is swapped atomically by a rebuild, so
// callers on the hot path never see a torn interface value.
func (ns *netState) dev() Device {
	if p := ns.liveDev.Load(); p != nil {
		return *p
	}
	return ns.spec.Dev
}

// setDev installs d as the live overlay device.
func (ns *netState) setDev(d Device) { ns.liveDev.Store(&d) }

// dpStateGet / dpStateSet are tiny helpers so callers don't each hand-roll the
// dpMu dance.
func (ns *netState) dpStateGet() int {
	ns.dpMu.Lock()
	defer ns.dpMu.Unlock()
	return ns.dpState
}

// shuttingDown reports whether this network (or the whole engine) is stopping,
// so recovery loops bail out instead of fighting teardown.
func (e *Engine) shuttingDown(ns *netState) bool {
	select {
	case <-e.stop:
		return true
	case <-ns.done:
		return true
	default:
		return false
	}
}

// recoverDataplane rebuilds the overlay interface, retrying with backoff until
// it succeeds or the network is torn down. Called by tunLoop when its Read
// fails on a device that wasn't closed for shutdown. Returns false if we should
// stop (shutdown), true once a fresh device is live.
func (e *Engine) recoverDataplane(ns *netState) bool {
	backoff := dpRebuildBackoffStart
	for {
		err := e.rebuildOverlayDevice(ns)
		if err == nil {
			return true
		}
		if errors.Is(err, errDataplaneClosing) {
			return false
		}
		// Rate-limited so a persistent failure (e.g. /dev/net/tun missing,
		// CAP_NET_ADMIN dropped) doesn't flood the log every backoff tick.
		if e.dpShouldLog(ns) {
			e.log.Warnf("mesh: overlay interface rebuild failed on net %x: %v — retrying (overlay traffic stays down until it succeeds)", ns.spec.ID, err)
		}
		select {
		case <-e.stop:
			return false
		case <-ns.done:
			return false
		case <-time.After(backoff):
		}
		if backoff < dpRebuildBackoffMax {
			backoff *= 2
			if backoff > dpRebuildBackoffMax {
				backoff = dpRebuildBackoffMax
			}
		}
	}
}

// dpShouldLog rate-limits the "still down" warnings to once per backoff-max
// window so a wedged interface leaves a periodic trail without spamming.
func (e *Engine) dpShouldLog(ns *netState) bool {
	ns.dpMu.Lock()
	defer ns.dpMu.Unlock()
	now := time.Now()
	if now.Sub(ns.dpLastLog) < dpRebuildBackoffMax {
		return false
	}
	ns.dpLastLog = now
	return true
}

// rebuildOverlayDevice creates a fresh overlay device via the injected factory,
// swaps it in for the dead one, and re-applies the address, base route, and
// every redistributed route (via reassertOSState). The create + swap + close of
// the old handle is done under dpMu so it can't interleave with teardown's own
// dpMu-guarded close — either teardown wins (and this aborts with
// errDataplaneClosing before swapping) or this wins (and teardown then closes
// the interface this installed). Either way exactly one live device exists at
// the end and it's the one teardown will close.
func (e *Engine) rebuildOverlayDevice(ns *netState) error {
	if ns.spec.NewDevice == nil {
		return errNoDeviceFactory
	}

	ns.dpMu.Lock()
	if ns.dpState == dpClosing {
		ns.dpMu.Unlock()
		return errDataplaneClosing
	}
	ns.dpState = dpRebuilding
	newDev, err := ns.spec.NewDevice()
	if err != nil {
		// Leave rebuilding→healthy so the next attempt (or the maint reconcile)
		// can try again; don't clobber a dpClosing that raced in.
		if ns.dpState == dpRebuilding {
			ns.dpState = dpHealthy
		}
		ns.dpMu.Unlock()
		return fmt.Errorf("recreate overlay interface: %w", err)
	}
	old := ns.dev()
	ns.setDev(newDev)
	e.maybeEnableGSO(ns, newDev) // fresh fd: offload isn't negotiated on it yet, even if the old one had it
	ns.dpRebuilds++
	if ns.dpState == dpRebuilding {
		ns.dpState = dpHealthy
	}
	ns.dpMu.Unlock()

	if old != nil {
		_ = old.Close() // free the dead handle; a double close just errors, ignored
	}
	// Re-apply address + base route + tracked redistributed routes onto the new
	// interface. reassertOSState targets ns.dev(), which is now newDev.
	e.reassertOSState(ns)
	e.log.Warnf("mesh: overlay interface rebuilt on net %x (%s) — data plane restored", ns.spec.ID, newDev.Name())
	return nil
}

// reconcileDataplane is the maintenance-tick belt. It confirms the interface
// against the kernel and repairs drift that a read error alone wouldn't catch.
// Only the address-present-but-stripped case is repaired here directly; a fully
// missing interface is handed to tunLoop (by closing the live device) so a
// single owner drives the rebuild.
func (e *Engine) reconcileDataplane(ns *netState, now time.Time) {
	if ns.spec.NewDevice == nil {
		return // recreation not wired (tests / embedders): leave behaviour unchanged
	}
	if ns.dpStateGet() != dpHealthy {
		return // a rebuild is already in flight; don't second-guess it
	}
	dev := ns.dev()
	if dev == nil {
		return
	}
	name := dev.Name()

	iface, err := net.InterfaceByName(name)
	if err != nil || iface == nil {
		// Interface is gone entirely. Unblock tunLoop's Read so it rebuilds;
		// we deliberately don't rebuild here to keep a single owner.
		e.log.Warnf("mesh: overlay interface %s missing on net %x (%v) — triggering rebuild", name, ns.spec.ID, err)
		_ = dev.Close()
		return
	}

	ns.mu.RLock()
	self4, sub4 := ns.self4, ns.subnet4
	self6, sub6 := ns.self6, ns.subnet6
	lastReassert := ns.dpLastRouteReassert
	ns.mu.RUnlock()

	up := iface.Flags&net.FlagUp != 0
	missingAddr := (self4.IsValid() && !ifaceHasAddr(iface, self4)) ||
		(self6.IsValid() && !ifaceHasAddr(iface, self6))

	if !up || missingAddr {
		// Interface exists but has been brought down or lost its overlay
		// address (a stripped-addr resume, a networkd reconcile, etc.).
		// reassertOSState re-adds address + base route + redistributed routes
		// in place, no device swap needed.
		e.log.Warnf("mesh: overlay interface %s on net %x is degraded (up=%v addr_missing=%v) — reasserting", name, ns.spec.ID, up, missingAddr)
		e.reassertOSState(ns)
		ns.mu.Lock()
		ns.dpLastRouteReassert = now
		ns.mu.Unlock()
		return
	}

	// Healthy interface with our address present. The kernel check can't see
	// routes, so periodically re-add the base subnet route defensively (a cheap
	// idempotent replace) to catch a route stripped while the address survived.
	if now.Sub(lastReassert) >= dpRouteReassertEvery {
		if sub4.IsValid() {
			_ = dev.AddRoute(sub4, 0)
		}
		if sub6.IsValid() {
			_ = dev.AddRoute(sub6, 0)
		}
		ns.mu.Lock()
		ns.dpLastRouteReassert = now
		ns.mu.Unlock()
	}
}

// ifaceHasAddr reports whether iface currently carries addr.
func ifaceHasAddr(iface *net.Interface, addr netip.Addr) bool {
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil {
			continue
		}
		if na, ok := netip.AddrFromSlice(ip); ok && na.Unmap() == addr {
			return true
		}
	}
	return false
}

// dataplaneHealthy reports whether ns's overlay interface is currently usable
// for carrying traffic: present in the kernel, up, and holding our overlay
// address. reason is a short human string when it isn't. This is the same
// kernel-truth check reconcileDataplane uses, exposed so callers can fail fast
// (see OverlayPathHealthy) instead of handing the OS an overlay destination it
// will silently route out the underlay.
func (ns *netState) dataplaneHealthy() (bool, string) {
	if ns.dpStateGet() == dpClosing {
		return false, "overlay network is shutting down"
	}
	dev := ns.dev()
	if dev == nil {
		return false, "no overlay interface"
	}
	name := dev.Name()
	iface, err := net.InterfaceByName(name)
	if err != nil || iface == nil {
		return false, fmt.Sprintf("overlay interface %s is not present", name)
	}
	if iface.Flags&net.FlagUp == 0 {
		return false, fmt.Sprintf("overlay interface %s is down", name)
	}
	ns.mu.RLock()
	self4, self6 := ns.self4, ns.self6
	ns.mu.RUnlock()
	if self4.IsValid() && !ifaceHasAddr(iface, self4) {
		return false, fmt.Sprintf("overlay interface %s is missing its address %s", name, self4)
	}
	if self6.IsValid() && !ifaceHasAddr(iface, self6) {
		return false, fmt.Sprintf("overlay interface %s is missing its address %s", name, self6)
	}
	return true, ""
}

// OverlayPathHealthy reports whether this node can currently carry traffic to
// dst over the mesh: the overlay interface for the network whose subnet
// contains dst must be present, up, and addressed. When it returns false the
// reason explains why. A caller that's about to originate an overlay
// connection — notably the management proxy — should refuse rather than dial,
// because with the tun gone the OS falls back to the default route and leaks
// the connection out a physical NIC, which surfaces at the far end as a
// baffling "connection arrived from <underlay ip>" auth failure rather than a
// clear local error.
func (e *Engine) OverlayPathHealthy(dst netip.Addr) (bool, string) {
	dst = dst.Unmap()
	for _, ns := range e.netSnapshot() {
		ns.mu.RLock()
		owns := (ns.subnet4.IsValid() && ns.subnet4.Contains(dst)) ||
			(ns.subnet6.IsValid() && ns.subnet6.Contains(dst))
		ns.mu.RUnlock()
		if owns {
			return ns.dataplaneHealthy()
		}
	}
	return false, "no overlay network on this node covers that address"
}
