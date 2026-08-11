//go:build windows

package hostnet

// Windows persists by default: netsh's store defaults to "persistent", so the
// commands the apply path already runs are written to the registry as well as
// to the running stack. Saying "no backend" here would be wrong in the
// direction that matters — it would tell an operator their change is temporary
// when it is not.
//
// What is left for this backend is the IPv4 mode, and it lives here rather than
// in applyMode for one reason: both of its commands are destructive. Switching
// to static replaces the interface's addresses, and switching to DHCP restarts
// the lease. Apply runs on every startup and reload from the reconciler, so
// neither belongs there; Persist is called by the reconciler only for a record
// that actually has a non-static family, and otherwise only by an operator's
// edit.
func detect() *Backend {
	return &Backend{
		Name:    "netsh (persistent)",
		Persist: persistNetsh,
		// netsh takes effect as it is run, so there is nothing to reapply.
		Reapply: func(Spec) error { return nil },
	}
}

func persistNetsh(s Spec) error {
	v4, _ := v4v6(s.StaticAddrs(s.Addrs))
	switch s.Mode4.Or(ModeStatic) {
	case ModeDHCP:
		return netshRun("interface", "ipv4", "set", "address",
			"name="+s.Iface, "source=dhcp")
	default:
		// `add address`, which the apply path uses, leaves the interface's
		// source as it found it — so an interface coming back from DHCP would
		// hold a static address and a live lease at the same time, which is
		// how this platform quietly differed from every other one. It is
		// `set address source=static` that ends the lease.
		//
		// Only the first address goes through set: it replaces rather than
		// adds, so putting the whole list through it would leave the last one
		// standing alone. Apply's add path has already put the others on, and
		// re-adds them on the next reconcile if this command removed them.
		if len(v4) == 0 {
			return nil
		}
		args := []string{"interface", "ipv4", "set", "address", "name=" + s.Iface,
			"source=static", "address=" + v4[0].Addr().String(), "mask=" + dottedMask(v4[0])}
		if s.GW4.IsValid() {
			args = append(args, "gateway="+s.GW4.String())
		}
		return netshRun(args...)
	}
}
