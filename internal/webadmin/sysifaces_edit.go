package webadmin

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/hostnet"
	"gravinet/internal/mesh"
)

// Editing host addressing. Applies immediately and without confirmation:
// `ip addr` does not ask either, and an admin tool that second-guesses an
// operator editing an address is one they stop using. The page says what the
// consequence is; it does not stand in the way of it.
//
// Implemented on every platform gravinet supports, each through the
// mechanism that platform's overlay device already uses: rtnetlink on Linux,
// ifconfig(8)/route(8) on the BSDs and macOS, netsh on Windows.
//
// What it does not do is persist. Whichever of netplan, NetworkManager,
// systemd-networkd, rc.conf or the Windows registry this host uses owns the
// boot-time configuration, and gravinet writes to none of them, so these
// changes are live-only and are lost on reboot. That is stated on the page
// rather than discovered later.

// handleSystemInterfaceEdit applies an address or gateway change.
//
// Addresses are submitted as the interface's whole intended set rather than
// as individual add/remove calls. Diffing here means an edit that changes one
// address of several does not go through a window where the interface has
// none — and, more to the point, removing the old address and adding the new
// one are then one operation from the operator's side rather than two chances
// to get half way.
func (s *Server) handleSystemInterfaceEdit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op    string   `json:"op"`
		Iface string   `json:"iface"`
		Addrs []string `json:"addrs"`
		GW4   string   `json:"gw4"`
		GW6   string   `json:"gw6"`
		MTU   int      `json:"mtu"`
		Mode4 string   `json:"mode4"`
		Mode6 string   `json:"mode6"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Iface = strings.TrimSpace(req.Iface)
	if req.Iface == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no interface named"})
		return
	}
	// A mesh device's address is not host state: it comes from the network's
	// own address4/address6, and every reload reapplies it. Setting it here
	// with the host tools would hold for a second or two and then be
	// silently reverted by the next reload.
	//
	// So the edit is written to the setting that actually governs it. This
	// page becomes a second view onto Mesh > Networks rather than a dead end
	// that tells the operator to go and find the right page — the address
	// they typed is the address they get, and where it is stored is
	// gravinet's problem.
	for _, i := range s.be.Interfaces() {
		if i.Iface != "" && strings.EqualFold(i.Iface, req.Iface) {
			s.editMeshOverlayAddr(w, r, req.Op, i, req.Addrs, req.MTU)
			return
		}
	}

	spec, err := buildHostSpec(s, hostEdit{
		Iface: req.Iface, Op: req.Op, Addrs: req.Addrs,
		GW4: req.GW4, GW6: req.GW6, MTU: req.MTU,
		Mode4: hostnet.Mode(strings.TrimSpace(req.Mode4)),
		Mode6: hostnet.Mode(strings.TrimSpace(req.Mode6)),
	})
	var warning string
	if err == nil {
		warning, err = s.applyAndRecord(r, spec)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": warning})
}

// applyAndRecord makes the live change, records it in gravinet's config so a
// restored snapshot brings it back, and writes it to the host's own network
// configuration so it survives a reboot.
//
// Returns a warning, not a report. Success is silent: the operator can see
// the new addressing in the table, and which files gravinet wrote it to is
// gravinet's problem, not something to hand back to someone who asked for an
// IP address to change.
//
// A partial failure is different in kind and is surfaced. The address is
// working right now, so reporting an error would be wrong, but it will not
// come back after a reboot or a restore — and that is a fact the operator
// cannot see anywhere and would otherwise discover at the worst moment.
//
// The order matters. Live first because it is what was asked for; the config
// record second so a config describes something real; persistence last
// because it is the step most likely to fail on an unusual host, and failing
// it must not undo the other two.
func (s *Server) applyAndRecord(r *http.Request, spec hostnet.Spec) (warning string, err error) {
	if _, _, err := hostnet.Apply(spec); err != nil {
		return "", err
	}

	rec := config.HostIface{
		Iface: spec.Iface, MTU: spec.MTU,
		Mode4: spec.Mode4, Mode6: spec.Mode6,
	}
	for _, p := range spec.Addrs {
		rec.Addrs = append(rec.Addrs, p.String())
	}
	if spec.GW4.IsValid() {
		rec.GW4 = spec.GW4.String()
	}
	if spec.GW6.IsValid() {
		rec.GW6 = spec.GW6.String()
	}

	var problems []string
	if err := s.mutateConfig(r, func(cfg *config.Config) error { return cfg.SetHostIface(rec) }); err != nil {
		problems = append(problems, "it could not be saved to gravinet's configuration, so a restored backup will not bring it back: "+err.Error())
	}
	persisted := true
	if _, err := hostnet.Persist(spec); err != nil {
		persisted = false
		if errors.Is(err, hostnet.ErrNoBackend) {
			problems = append(problems, "this host has no recognised network configuration, so the change will not survive a reboot")
		} else {
			problems = append(problems, "it could not be written to this host's network configuration, so it will not survive a reboot: "+err.Error())
		}
	}
	// A DHCP family is the one case where writing the configuration is not
	// enough to make the change real. Everything else on this page is a kernel
	// or link setting gravinet has already made live; a lease has to be sought
	// by a client daemon, which belongs to the backend. So the backend is asked
	// to reconfigure the interface — and where it cannot do that for one
	// interface alone, the operator is told the mode is written and waiting
	// rather than having the whole host's networking bounced for them.
	if persisted && (spec.Mode4 == hostnet.ModeDHCP || spec.Mode6 == hostnet.ModeDHCP6) {
		if err := hostnet.Reapply(spec); err != nil {
			if errors.Is(err, hostnet.ErrNoReapply) {
				problems = append(problems, "this host's network configuration has no per-interface reload, so the interface will not ask for a lease until it is brought down and up again or the host reboots")
			} else {
				problems = append(problems, "the interface could not be reconfigured to ask for a lease: "+err.Error())
			}
		}
	}
	if len(problems) == 0 {
		return "", nil
	}
	return "The change is applied, but " + strings.Join(problems, "; and ") + ".", nil
}

// hostEdit is one submitted edit. A struct rather than eight positional
// arguments: the modes made it nine, and two adjacent strings that are both
// modes and two more that are both gateways is a call site nobody can read.
type hostEdit struct {
	Iface, Op    string
	Addrs        []string
	GW4, GW6     string
	MTU          int
	Mode4, Mode6 hostnet.Mode
}

// buildHostSpec turns one edit into the interface's whole intended state.
//
// An edit names one thing — the addresses, the gateway, the MTU, or the mode —
// but Apply, Persist and the config record are all whole-interface. So the parts
// the operator did not touch are filled in from what gravinet already records,
// which is what stops editing a gateway from wiping the addresses.
func buildHostSpec(s *Server, e hostEdit) (hostnet.Spec, error) {
	// An edit submits the interface's whole intended set, so an address left
	// out of the box is one the operator has removed.
	spec := hostnet.Spec{Iface: strings.TrimSpace(e.Iface), Prune: true}

	// The record comes first now, because everything below is interpreted
	// through the modes and an edit that did not mention them inherits them.
	var rec *config.HostIface
	if cfg, err := config.Load(s.configPath); err == nil {
		rec = cfg.HostIfaceFor(spec.Iface)
	}
	spec.Mode4, spec.Mode6 = hostnet.ModeStatic, hostnet.ModeStatic
	if rec != nil {
		spec.Mode4, spec.Mode6 = rec.Mode4.Or(hostnet.ModeStatic), rec.Mode6.Or(hostnet.ModeStatic)
	}

	switch e.Op {
	case "mode":
		if err := hostnet.ValidMode4(e.Mode4); err != nil {
			return spec, err
		}
		if err := hostnet.ValidMode6(e.Mode6); err != nil {
			return spec, err
		}
		was4, was6 := spec.Mode4, spec.Mode6
		spec.Mode4 = e.Mode4.Or(was4)
		spec.Mode6 = e.Mode6.Or(was6)
		// A family that stays static keeps the addresses gravinet has for it;
		// one that has just left static hands them back. Without the second
		// half the old static address stays up on an interface that now
		// reports a different mode, which is an address nothing manages.
		if rec != nil {
			for _, a := range rec.Addrs {
				p, err := netip.ParsePrefix(strings.TrimSpace(a))
				if err != nil {
					continue
				}
				p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits())
				if spec.ModeFor(p.Addr()).IsStatic() {
					spec.Addrs = append(spec.Addrs, p)
				} else {
					spec.Release = append(spec.Release, p)
				}
			}
		}
	case "addrs":
		for _, a := range e.Addrs {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			p, err := netip.ParsePrefix(a)
			if err != nil {
				return spec, fmt.Errorf("%q is not an address with a prefix length (e.g. 10.1.1.1/24): %v", a, err)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits())
			// Typing an address into a family that is not static is not a
			// thing to accept and then not apply. Reported here, where the
			// operator can see which box it was.
			if !spec.ModeFor(p.Addr()).IsStatic() {
				fam, mode := "IPv4", spec.Mode4
				if p.Addr().Is6() {
					fam, mode = "IPv6", spec.Mode6
				}
				return spec, fmt.Errorf("%s is in %s mode, so %s cannot be set here — switch %s to static first", fam, string(mode), a, fam)
			}
			spec.Addrs = append(spec.Addrs, p)
		}
	case "mtu":
		if e.MTU < 576 || e.MTU > 9216 {
			return spec, fmt.Errorf("mtu %d: must be between 576 and 9216", e.MTU)
		}
		spec.MTU = e.MTU
		fillStaticAddrs(&spec, rec)
	case "gateway":
		fillStaticAddrs(&spec, rec)
		for _, g := range []struct {
			raw  string
			is4  bool
			mode hostnet.Mode
			dest *netip.Addr
		}{
			{e.GW4, true, spec.Mode4, &spec.GW4},
			{e.GW6, false, spec.Mode6, &spec.GW6},
		} {
			raw := strings.TrimSpace(g.raw)
			if raw == "" {
				continue
			}
			fam := map[bool]string{true: "IPv4", false: "IPv6"}[g.is4]
			if !g.mode.IsStatic() {
				return spec, fmt.Errorf("%s is in %s mode, so its default route comes from the network and cannot be set here", fam, string(g.mode))
			}
			a, err := netip.ParseAddr(raw)
			if err != nil {
				return spec, fmt.Errorf("%q is not an IP address: %v", raw, err)
			}
			a = a.Unmap()
			if a.Is4() != g.is4 {
				return spec, fmt.Errorf("%s is not an %s address", raw, fam)
			}
			*g.dest = a
		}
	default:
		return spec, fmt.Errorf("unknown op %q", e.Op)
	}

	// An edit names one field; the rest come from what gravinet already
	// records, so the edits do not undo each other. A gateway is only carried
	// forward into a family that is still static — one that is not has just
	// had its recorded gateway made meaningless, and reasserting it would
	// install a default route competing with the one the network supplies.
	if rec != nil {
		if e.Op != "gateway" {
			if a, err := netip.ParseAddr(rec.GW4); err == nil && spec.Mode4.IsStatic() {
				spec.GW4 = a
			}
			if a, err := netip.ParseAddr(rec.GW6); err == nil && spec.Mode6.IsStatic() {
				spec.GW6 = a
			}
		}
		if e.Op != "mtu" {
			spec.MTU = rec.MTU
		}
	}
	return spec, nil
}

// fillStaticAddrs fills in the addresses an edit did not mention, for the
// static families, from what gravinet already records for this interface.
//
// An MTU or gateway edit has to carry the addresses forward or Prune would
// remove them. The record is where they come from, and the distinction from the
// live interface is the whole point: an address on the interface that gravinet
// does not record is not gravinet's, and writing it into the record because
// someone changed the MTU claims it forever.
//
// That is not hypothetical. This used to read the live interface and filter by
// mode, on the reasoning that the mode filter would keep a lease out. It does
// not, in the one case that matters: an interface with no record at all has no
// mode, buildHostSpec reads that as static, and so a DHCP lease on a
// never-managed interface passed the filter and was recorded as a static
// address — reapplied as one at every reload thereafter, and written into the
// host's own boot configuration.
//
// With no record there is no intended set at all, so an MTU or gateway edit
// must not prune either: there is nothing to prune against, and the addresses
// on the interface belong to whatever is managing it.
func fillStaticAddrs(spec *hostnet.Spec, rec *config.HostIface) {
	if rec == nil {
		spec.Prune = false
		return
	}
	var recorded []netip.Prefix
	for _, a := range rec.Addrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(a))
		if err != nil {
			continue
		}
		recorded = append(recorded, netip.PrefixFrom(p.Addr().Unmap(), p.Bits()))
	}
	spec.Addrs = spec.StaticAddrs(recorded)
}

// editMeshOverlayAddr redirects an edit of a mesh device to the network's
// overlay address, then lets the reload apply it — the same path a change
// made on Mesh > Networks takes.
//
// Gateways are refused rather than redirected. A network has no such setting:
// the overlay's routing comes from what peers advertise, so there would be
// nowhere to write it and nothing to apply it.
func (s *Server) editMeshOverlayAddr(w http.ResponseWriter, r *http.Request, op string, info mesh.IfaceInfo, addrs []string, mtu int) {
	if op == "mtu" {
		// A mesh device's MTU is the network's, for the same reason its
		// address is: it is reapplied from the network's settings, so it is
		// written there.
		if mtu < 576 || mtu > 9216 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("mtu %d: must be between 576 and 9216", mtu)})
			return
		}
		err := s.mutateConfig(r, func(cfg *config.Config) error {
			n := cfg.FindNetwork(fmt.Sprintf("%016x", info.NetworkID))
			if n == nil {
				return fmt.Errorf("network %s is no longer configured", info.Name)
			}
			n.MTU = mtu
			return nil
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": ""})
		return
	}
	if op == "mode" {
		// There is no mode to choose. A mesh device's address is the network's
		// address4/address6, reapplied from the configuration on every reload;
		// there is no DHCP server and no router advertising a prefix on an
		// overlay, so the only answer this control could give is the one it
		// already has.
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": info.Iface + " is a mesh interface; its address comes from network " + info.Name + "'s own settings, so there is no DHCP or SLAAC mode to choose",
		})
		return
	}
	if op != "addrs" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": info.Iface + " is a mesh interface; its routing comes from the peers on network " + info.Name + ", so it has no default gateway to set",
		})
		return
	}

	var v4, v6 string
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		p, err := netip.ParsePrefix(a)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("%q is not an address with a prefix length (e.g. 10.42.0.5/16): %v", a, err),
			})
			return
		}
		// One address per family: a mesh device has exactly one overlay
		// address in each, and quietly keeping the first of several would
		// leave the operator looking at a list that is not what is running.
		if p.Addr().Is4() {
			if v4 != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a mesh interface takes one IPv4 overlay address"})
				return
			}
			v4 = p.String()
		} else {
			if v6 != "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a mesh interface takes one IPv6 overlay address"})
				return
			}
			v6 = p.String()
		}
	}

	err := s.mutateConfig(r, func(cfg *config.Config) error {
		n := cfg.FindNetwork(fmt.Sprintf("%016x", info.NetworkID))
		if n == nil {
			return fmt.Errorf("network %s is no longer configured", info.Name)
		}
		// An address outside the network's own subnet cannot be routed by
		// any peer, so it is refused here rather than accepted and then
		// found not to work.
		for _, c := range []struct{ addr, subnet, fam string }{
			{v4, n.Subnet4, "IPv4"}, {v6, n.Subnet6, "IPv6"},
		} {
			if c.addr == "" || c.subnet == "" {
				continue
			}
			p, _ := netip.ParsePrefix(c.addr)
			sub, err := netip.ParsePrefix(c.subnet)
			if err != nil {
				continue
			}
			if !sub.Contains(p.Addr()) {
				return fmt.Errorf("%s is outside network %s's %s subnet %s, so no peer could route to it", c.addr, n.Name, c.fam, c.subnet)
			}
		}
		// Empty means "go back to self-assigning", which is what clearing
		// the field has always meant on Mesh > Networks.
		n.Address4, n.Address6 = v4, v6
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// The reload applies it, so the address is in place by the time the page
	// reloads its inventory.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": ""})
}
