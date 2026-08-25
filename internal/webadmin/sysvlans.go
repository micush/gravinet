package webadmin

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/hostnet"
)

// System > Interfaces, tagged interfaces. The one kind of interface gravinet
// creates on the host rather than merely addressing.
//
// Creation and addressing stay separate on purpose. A VLAN device, once it
// exists, is an interface like any other: it appears in the inventory above,
// it takes an address through the same editor, and it can be served DHCP or
// advertised on. Giving it a second addressing model here would mean two
// places to set an address and two of them to get out of step.
//
// The dangerous operation is deletion, and it is bounded by the configuration
// rather than by the request. Only a device this node has a definition for can
// be removed — an operator cannot reach an arbitrary interface through this
// endpoint by naming it, and neither can anything that reaches this endpoint
// on their behalf.

// handleSystemVLANs lists, creates, toggles and removes tagged interfaces.
func (s *Server) handleSystemVLANs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"vlans":       vlanRows(cfg.HostVLANs),
			"supported":   hostnet.VLANSupported,
			"parents":     s.vlanParents(),
			"mesh_ifaces": s.dhcpMeshIfaces(),
		})
		return
	}

	var req struct {
		Op     string `json:"op"`
		Parent string `json:"parent"`
		ID     int    `json:"id"`
		Name   string `json:"name"`
		Index  int    `json:"index"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !hostnet.VLANSupported && req.Op != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "gravinet can only create tagged interfaces on Linux"})
		return
	}

	// removed carries the devices this request took out of service, so they
	// can be torn down after the configuration has been written rather than
	// before: a delete that succeeded on the host and then failed to save
	// would come back on the next reload, which is the confusing direction to
	// fail in.
	var removed []string
	err := s.mutateConfig(r, func(cfg *config.Config) error {
		switch req.Op {
		case "add":
			v := config.HostVLAN{
				Parent: strings.TrimSpace(req.Parent),
				ID:     req.ID,
				Name:   strings.TrimSpace(req.Name),
			}
			if err := v.Validate(); err != nil {
				return err
			}
			if err := s.refuseVLANParent(v.Parent); err != nil {
				return err
			}
			// A name already on the host that gravinet did not create is a
			// collision, not something to take over. Checked here rather than
			// left to the kernel's EEXIST so the message names the reason.
			if _, err := net.InterfaceByName(v.VLANName()); err == nil {
				return fmt.Errorf("%s already exists on this host", v.VLANName())
			}
			cfg.HostVLANs = append(cfg.HostVLANs, v)
			return cfg.ValidateHostVLANs()
		case "enable", "disable":
			if req.Index < 0 || req.Index >= len(cfg.HostVLANs) {
				return fmt.Errorf("no tagged interface at index %d", req.Index)
			}
			cfg.HostVLANs[req.Index].Disabled = req.Op == "disable"
			if req.Op == "disable" {
				removed = append(removed, cfg.HostVLANs[req.Index].VLANName())
			}
			return nil
		case "delete", "remove":
			if req.Index < 0 || req.Index >= len(cfg.HostVLANs) {
				return fmt.Errorf("no tagged interface at index %d", req.Index)
			}
			v := cfg.HostVLANs[req.Index]
			removed = append(removed, v.VLANName())
			cfg.HostVLANs = append(cfg.HostVLANs[:req.Index], cfg.HostVLANs[req.Index+1:]...)
			// The addressing record goes with the device. Left behind it
			// would describe an interface that no longer exists, and would be
			// reapplied the moment somebody recreated the same VLAN — putting
			// an address the operator had deleted back onto the host.
			cfg.HostInterfaces = dropHostIface(cfg.HostInterfaces, v.VLANName())
			return nil
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	note := s.applyVLANs(removed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": false, "note": note})
}

// applyVLANs makes the host match the configuration that was just written:
// every enabled definition present and up, and every device named in removed
// gone.
//
// Reads the configuration back rather than acting on the request, so what is
// created is what was stored — the same contract handleDHCP applies.
func (s *Server) applyVLANs(removed []string) string {
	for _, name := range removed {
		if err := hostnet.DeleteVLAN(name); err != nil {
			return "saved, but " + err.Error()
		}
	}
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return ""
	}
	var notes []string
	for _, v := range cfg.HostVLANs {
		name := v.VLANName()
		if v.Disabled {
			if err := hostnet.DeleteVLAN(name); err != nil {
				notes = append(notes, err.Error())
			}
			continue
		}
		created, err := hostnet.EnsureVLAN(v.Parent, name, v.ID)
		if err != nil {
			notes = append(notes, err.Error())
			continue
		}
		if created {
			// Worth saying: a new device has appeared on the host, and the
			// next thing the operator has to do is give it an address, which
			// is a different row on the same page.
			notes = append(notes, fmt.Sprintf("created %s — set its address in the table above", name))
		}
	}
	return strings.Join(notes, "; ")
}

// refuseVLANParent rejects the parents a tagged interface cannot ride on.
func (s *Server) refuseVLANParent(name string) error {
	for _, i := range s.be.Interfaces() {
		if i.Iface != "" && strings.EqualFold(i.Iface, name) {
			return fmt.Errorf("%s is this node's mesh device for network %q — a VLAN tag inside the overlay's own encapsulation addresses nothing", name, i.Name)
		}
	}
	if _, err := net.InterfaceByName(name); err != nil {
		return fmt.Errorf("no interface named %s on this host", name)
	}
	return nil
}

// vlanParents is the set of interfaces a tagged interface may be created on:
// everything on the host except gravinet's own devices, the loopback, and
// existing VLANs (which would be stacked tagging, refused in the model).
func (s *Server) vlanParents() []string {
	mesh := map[string]bool{}
	for _, n := range s.dhcpMeshIfaces() {
		mesh[n] = true
	}
	ifis, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifi := range ifis {
		if mesh[ifi.Name] || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if _, _, isVLAN := hostnet.VLANInfo(ifi.Name); isVLAN {
			continue
		}
		out = append(out, ifi.Name)
	}
	return out
}

// vlanRow is one definition plus what the host currently says about it. The
// two can disagree — a parent that has been renamed, a device somebody removed
// by hand — and the page shows the disagreement rather than the definition
// alone, which would read as working.
type vlanRow struct {
	Parent   string `json:"parent"`
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
	Present  bool   `json:"present"`
	Up       bool   `json:"up"`
	Problem  string `json:"problem,omitempty"`
}

func vlanRows(vs []config.HostVLAN) []vlanRow {
	out := make([]vlanRow, 0, len(vs))
	for _, v := range vs {
		name := v.VLANName()
		row := vlanRow{Parent: v.Parent, ID: v.ID, Name: name, Disabled: v.Disabled}
		ifi, err := net.InterfaceByName(name)
		row.Present = err == nil
		if row.Present {
			row.Up = ifi.Flags&net.FlagUp != 0
		}
		switch {
		case v.Disabled:
			// A parked row is not supposed to be there; saying so would be
			// handing the operator their own request back as a fault.
		case !row.Present:
			row.Problem = "not present on this host"
			if _, perr := net.InterfaceByName(v.Parent); perr != nil {
				row.Problem = "parent " + v.Parent + " is not on this host, so " + name + " cannot be created"
			}
		case !row.Up:
			row.Problem = name + " exists but is down, so it carries no traffic"
		default:
			// Present and up, but is it the device this row describes? A name
			// that matches something else entirely is the case worth catching:
			// the row would otherwise look healthy while the traffic went
			// somewhere unrelated.
			if p, id, ok := hostnet.VLANInfo(name); ok {
				if id != v.ID {
					row.Problem = fmt.Sprintf("%s carries vlan %d on this host, not %d", name, id, v.ID)
				} else if p != "" && !strings.EqualFold(p, v.Parent) {
					row.Problem = fmt.Sprintf("%s is on %s on this host, not %s", name, p, v.Parent)
				}
			} else if hostnet.VLANSupported {
				row.Problem = name + " exists but is not a tagged interface — something else on this host owns that name"
			}
		}
		out = append(out, row)
	}
	return out
}

// dropHostIface removes an addressing record by interface name.
//
// Allocates rather than filtering into in[:0]. The in-place form writes
// through the caller's backing array, so the slice that was passed in is left
// holding a duplicated tail — harmless where the result is assigned straight
// back over the original, and a silent corruption anywhere else.
func dropHostIface(in []config.HostIface, name string) []config.HostIface {
	out := make([]config.HostIface, 0, len(in))
	for _, h := range in {
		if !strings.EqualFold(strings.TrimSpace(h.Iface), name) {
			out = append(out, h)
		}
	}
	return out
}
