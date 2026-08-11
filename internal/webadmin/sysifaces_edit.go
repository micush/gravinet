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

	spec, err := buildHostSpec(s, req.Iface, req.Op, req.Addrs, req.GW4, req.GW6, req.MTU)
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

	rec := config.HostIface{Iface: spec.Iface, MTU: spec.MTU}
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
	if _, err := hostnet.Persist(spec); err != nil {
		if errors.Is(err, hostnet.ErrNoBackend) {
			problems = append(problems, "this host has no recognised network configuration, so the change will not survive a reboot")
		} else {
			problems = append(problems, "it could not be written to this host's network configuration, so it will not survive a reboot: "+err.Error())
		}
	}
	if len(problems) == 0 {
		return "", nil
	}
	return "The address is applied, but " + strings.Join(problems, "; and ") + ".", nil
}

// buildHostSpec turns one edit into the interface's whole intended state.
//
// An edit names one thing — the addresses, or the gateway — but Apply and the
// config record are both whole-interface. So the half the operator did not
// touch is filled in from what is there now, which is what stops editing a
// gateway from wiping the addresses.
func buildHostSpec(s *Server, iface, op string, addrs []string, gw4, gw6 string, mtu int) (hostnet.Spec, error) {
	// An edit submits the interface's whole intended set, so an address left
	// out of the box is one the operator has removed.
	spec := hostnet.Spec{Iface: strings.TrimSpace(iface), Prune: true}

	switch op {
	case "addrs":
		for _, a := range addrs {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			p, err := netip.ParsePrefix(a)
			if err != nil {
				return spec, fmt.Errorf("%q is not an address with a prefix length (e.g. 10.1.1.1/24): %v", a, err)
			}
			spec.Addrs = append(spec.Addrs, netip.PrefixFrom(p.Addr().Unmap(), p.Bits()))
		}
	case "mtu":
		if mtu < 576 || mtu > 9216 {
			return spec, fmt.Errorf("mtu %d: must be between 576 and 9216", mtu)
		}
		spec.MTU = mtu
		cur, err := hostnet.GlobalAddrs(spec.Iface)
		if err != nil {
			return spec, err
		}
		spec.Addrs = cur
	case "gateway":
		cur, err := hostnet.GlobalAddrs(spec.Iface)
		if err != nil {
			return spec, err
		}
		spec.Addrs = cur
		for _, g := range []struct {
			raw  string
			is4  bool
			dest *netip.Addr
		}{{gw4, true, &spec.GW4}, {gw6, false, &spec.GW6}} {
			raw := strings.TrimSpace(g.raw)
			if raw == "" {
				continue
			}
			a, err := netip.ParseAddr(raw)
			if err != nil {
				return spec, fmt.Errorf("%q is not an IP address: %v", raw, err)
			}
			a = a.Unmap()
			if a.Is4() != g.is4 {
				return spec, fmt.Errorf("%s is not an %s address", raw, map[bool]string{true: "IPv4", false: "IPv6"}[g.is4])
			}
			*g.dest = a
		}
	default:
		return spec, fmt.Errorf("unknown op %q", op)
	}

	// An edit names one field; the rest come from what gravinet already
	// records, so the three edits do not undo each other.
	if op != "gateway" {
		if cfg, err := config.Load(s.configPath); err == nil {
			if h := cfg.HostIfaceFor(spec.Iface); h != nil {
				if a, err := netip.ParseAddr(h.GW4); err == nil {
					spec.GW4 = a
				}
				if a, err := netip.ParseAddr(h.GW6); err == nil {
					spec.GW6 = a
				}
				if op != "mtu" {
					spec.MTU = h.MTU
				}
			}
		}
	}
	return spec, nil
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
