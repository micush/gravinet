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
		// A static family whose interface is carrying addresses gravinet does
		// not record is the shape of an unfinished switch away from DHCP: the
		// recorded address is on, the lease is still on beside it, and the
		// client that put it there is still running because the host's own
		// configuration was never told otherwise. Naming them is the first
		// half — silently coexisting is how this reads as "why do I have two
		// addresses" — and Persist below is the half that stops the client.
		//
		// They are not deleted here. This runs on every reload, and an address
		// gravinet did not put on an interface may be somebody else's; removing
		// it on the strength of a record is the editor's job, where an operator
		// asked for it and is watching.
		var stray []netip.Prefix
		if live, err := hostnet.GlobalAddrs(h.Iface); err == nil {
			want := map[netip.Prefix]bool{}
			for _, p := range spec.Addrs {
				want[p] = true
			}
			for _, p := range live {
				if !want[p] && spec.ModeFor(p.Addr()).IsStatic() {
					stray = append(stray, p)
				}
			}
		}
		if len(stray) > 0 {
			logx.Warnf("host interface %s: %v %s on this interface but not in gravinet's configuration, "+
				"on a family gravinet records as static — if this was a lease, the client that issued it is still running; "+
				"re-save the addresses on System > Interfaces to remove it",
				h.Iface, stray, map[bool]string{true: "is", false: "are"}[len(stray) == 1])
		}

		// The host's own network configuration is written when a record has a
		// non-static family, and now also when the live interface disagrees
		// with the record — an added address or a stray one both mean the boot
		// configuration says something other than what gravinet was asked for,
		// and a static family whose backend still says dhcp gets its lease
		// back on the next renewal no matter how often Apply removes it.
		//
		// Still not written on an ordinary reload where nothing disagreed.
		// That was the original reason to skip it for static-only records, and
		// it holds: rewriting an interface's boot-time configuration every time
		// a mesh setting changes is churn on the one file a host cannot afford
		// to have go wrong.
		if !h.Mode4.IsStatic() || !h.Mode6.IsStatic() || added > 0 || removed > 0 || len(stray) > 0 {
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

// reconcileHostVLANs makes the host's tagged interfaces match what the
// configuration defines. Run immediately before reconcileHostInterfaces, at
// startup and on every reload, and the order is the point: a HostIface record
// naming eth0.100 has nowhere to put an address until eth0.100 exists.
//
// This is also the whole of gravinet's VLAN persistence. Nothing is written to
// netplan, NetworkManager or systemd-networkd — the devices are recreated here
// on every start instead, which is how the mesh devices have always worked.
// The trade is deliberate: co-owning the file that decides whether a host
// comes back with any networking at all is the failure mode the hostnet
// package exists to avoid, and it is not worth paying to save a device that
// costs one syscall to recreate.
//
// Like its addressing counterpart, a failure here is logged and stepped over.
// One VLAN that cannot be created must not stop a mesh from coming up.
func reconcileHostVLANs(cfg *config.Config) {
	if len(cfg.HostVLANs) == 0 {
		return
	}
	if !hostnet.VLANSupported {
		logx.Errorf("host vlans: %d defined, but gravinet can only create tagged interfaces on Linux", len(cfg.HostVLANs))
		return
	}
	for _, v := range cfg.HostVLANs {
		name := v.VLANName()
		if v.Disabled {
			// Torn down rather than merely skipped. A parked definition whose
			// device stayed up would keep carrying traffic, which is not what
			// disabling a row means anywhere else in this configuration.
			if err := hostnet.DeleteVLAN(name); err != nil {
				logx.Errorf("host vlan %s: %v", name, err)
			}
			continue
		}
		if err := v.Validate(); err != nil {
			logx.Errorf("host vlan %s: %v", name, err)
			continue
		}
		created, err := hostnet.EnsureVLAN(v.Parent, name, v.ID)
		if err != nil {
			logx.Errorf("host vlan %s: %v", name, err)
			continue
		}
		if created {
			logx.Infof("host vlan %s: created as vlan %d on %s", name, v.ID, v.Parent)
		}
	}
}
