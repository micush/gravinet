//go:build windows

package hostnet

// applyMode sets the IPv6 half here and leaves IPv4 to Persist.
//
// The split is about which commands are safe to run repeatedly. Apply is called
// by the reconciler on every startup and reload, so anything in it has to be
// idempotent and non-destructive. `netsh interface ipv6 set interface` is both:
// it flips two flags and touches no address.
//
// The IPv4 mode is not. Switching to static goes through
// `set address source=static`, which replaces the interface's addresses, and
// switching to DHCP restarts the lease. Neither belongs on a path that runs at
// every reload, so both live in the persist backend, which the reconciler calls
// only for a record that actually has a non-static family.
//
// routerdiscovery is RA handling and managedaddress is whether to take
// addresses from DHCPv6 — the same pair as Linux's accept_ra and autoconf, in
// the other polarity for the second one.
func applyMode(s Spec) error {
	m6 := s.Mode6.Or(ModeStatic)
	rd, managed := "disabled", "disabled"
	if m6.AcceptsRA() {
		rd = "enabled"
	}
	if m6 == ModeDHCP6 {
		managed = "enabled"
	}
	// store=persistent is spelled out for the same reason setMTU spells it
	// out: this is the family of netsh settings whose default store has
	// differed between releases, and a mode that reverts at the next boot is
	// the failure this whole package is built around.
	return netshRun("interface", "ipv6", "set", "interface", s.Iface,
		"routerdiscovery="+rd, "managedaddress="+managed, "store=persistent")
}
