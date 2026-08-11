package main

import (
	"errors"
	"net/netip"

	"gravinet/internal/config"
	"gravinet/internal/hostnet"
	"gravinet/internal/logx"
)

// reconcileHostInterfaces makes the host's addressing match what the
// configuration says gravinet owns. Run at startup and on every reload.
//
// This is what makes a restored snapshot bring addressing back with it. The
// restore writes the configuration and reloads; the reload gets here; the
// addresses named in that configuration are put back on the host. Without it
// the config would describe addressing that nothing ever applied.
//
// Only interfaces listed in HostInterfaces are touched, and that list is only
// ever populated by an operator editing an interface through gravinet. An
// unlisted interface is not managed and not reconciled — DHCP, another
// configuration tool, or a hand-set address on any other NIC is left entirely
// alone.
//
// Failures are logged and stepped over rather than aborting the reload. A
// reload carries far more than this, and one interface that cannot be
// configured — a NIC renamed, a cable pulled, an address already taken — must
// not stop a mesh from coming up.
func reconcileHostInterfaces(cfg *config.Config) {
	for _, h := range cfg.HostInterfaces {
		// Prune is deliberately left false. This runs at every startup and
		// reload, against a record that may be older than the interface:
		// an address put there by DHCP, by another tool, or by an edit
		// gravinet never saw is not gravinet's to delete. Ensuring the
		// recorded addresses are present is the job; removing everything
		// else is not, and doing it here stripped addressing on every
		// reload — which then also cost FRR the connected routes derived
		// from it.
		spec := hostnet.Spec{Iface: h.Iface, Mode4: h.Mode4, Mode6: h.Mode6}
		bad := false
		for _, a := range h.Addrs {
			p, err := netip.ParsePrefix(a)
			if err != nil {
				logx.Errorf("host interface %s: bad address %q in config: %v", h.Iface, a, err)
				bad = true
				break
			}
			spec.Addrs = append(spec.Addrs, netip.PrefixFrom(p.Addr().Unmap(), p.Bits()))
		}
		if bad {
			continue
		}
		spec.MTU = h.MTU
		if a, err := netip.ParseAddr(h.GW4); err == nil {
			spec.GW4 = a.Unmap()
		}
		if a, err := netip.ParseAddr(h.GW6); err == nil {
			spec.GW6 = a.Unmap()
		}

		added, removed, err := hostnet.Apply(spec)
		_ = removed // never non-zero here: the reconciler does not prune
		if err != nil {
			logx.Errorf("host interface %s: %v", h.Iface, err)
			continue
		}
		// A record with a non-static family is also written to the host's own
		// network configuration here, which the reconciler does for nothing
		// else. The reason is narrow: a restore writes gravinet's config and
		// reloads, and everything else in a record is something Apply above has
		// just made true on the running interface. A DHCP lease is not — it
		// needs the backend's client — so without this a restored node would
		// report dhcp on an interface that had never asked for an address.
		//
		// Static-only records take the path they always have, untouched. That
		// is deliberate: this runs on every reload, and rewriting an
		// interface's boot-time configuration each time for a change that has
		// already been applied is churn on the one file a host cannot afford
		// to have go wrong.
		if !h.Mode4.IsStatic() || !h.Mode6.IsStatic() {
			if _, err := hostnet.Persist(spec); err != nil {
				logx.Errorf("host interface %s: addressing mode applied but not written to this host's network configuration: %v", h.Iface, err)
			} else if err := hostnet.Reapply(spec); err != nil && !errors.Is(err, hostnet.ErrNoReapply) {
				logx.Errorf("host interface %s: could not ask the network configuration to reconfigure it: %v", h.Iface, err)
			}
		}
		// Quiet when there is nothing to do, which is the usual case on an
		// ordinary reload — this runs on every one of them, and logging a
		// line each time would bury the reloads where it did something.
		if added > 0 || removed > 0 {
			logx.Infof("host interface %s: restored %d address(es) from gravinet's configuration", h.Iface, added)
		}
	}
}
