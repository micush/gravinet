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
		// One reading of the host for both halves of the response, so the
		// parent picker and the rows below it cannot disagree about what is
		// on this machine.
		host := readHostView()
		writeJSON(w, http.StatusOK, map[string]any{
			"vlans":       vlanRows(cfg.HostVLANs, host),
			"supported":   hostnet.VLANSupported,
			"parents":     s.vlanParents(host),
			"mesh_ifaces": s.dhcpMeshIfaces(),
		})
		return
	}

	var req struct {
		Op     string `json:"op"`
		Parent string `json:"parent"`
		ID     int    `json:"id"`
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
			// No Name. The device name is derived from the parent and the tag
			// — see HostVLAN.Name for why the field still exists and what
			// still honours it.
			v := config.HostVLAN{
				Parent: strings.TrimSpace(req.Parent),
				ID:     req.ID,
			}
			if err := v.Validate(); err != nil {
				return err
			}
			if err := s.refuseVLANParent(v.Parent); err != nil {
				return err
			}
			if err := refuseVLANNameInUse(v, readHostView()); err != nil {
				return err
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

// refuseVLANNameInUse rejects an add whose device name is already taken by
// something that is not the tagged interface being asked for.
//
// Checked here rather than left to the kernel's EEXIST so the message names
// the reason. The distinction matters more since names became derived: an
// operator can no longer sidestep a clash by typing a different name, so a
// bare "already exists" would be a dead end rather than a hint.
//
// A device that is already exactly this VLAN is not a clash. gravinet's own
// devices outlive the process that made them — the kernel keeps a link until
// something removes it — so a node whose configuration was restored from a
// backup, or whose entry was deleted and re-added, meets its own handiwork
// here. EnsureVLAN already treats an existing device as success; refusing the
// definition that describes it would leave the operator with a device on the
// host and no row for it.
func refuseVLANNameInUse(v config.HostVLAN, host hostView) error {
	name := v.VLANName()
	if !host.exists(name) {
		return nil
	}
	dev, isVLAN := host.vlans[name]
	switch {
	case !hostnet.VLANSupported || !isVLAN:
		return fmt.Errorf("%s already exists on this host and is not a tagged interface — something else owns that name", name)
	case dev.ID != v.ID:
		return fmt.Errorf("%s already exists on this host carrying vlan %d, not %d", name, dev.ID, v.ID)
	case dev.Parent != "" && !strings.EqualFold(dev.Parent, v.Parent):
		return fmt.Errorf("%s already exists on this host on %s, not %s", name, dev.Parent, v.Parent)
	case dev.QinQ:
		return fmt.Errorf("%s already exists on this host carrying tag %d as 802.1ad (QinQ), not 802.1Q", name, dev.ID)
	}
	// Already the device this definition describes.
	return nil
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
//
// The VLAN exclusion only started working when VLANDevices did. While the
// lookup behind it answered "not a VLAN" about everything, every tagged
// interface on the host was offered here as a parent — so the picker invited
// exactly the stacked configuration ValidateHostVLANs then refuses on save.
func (s *Server) vlanParents(host hostView) []string {
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
		if _, isVLAN := host.vlans[ifi.Name]; isVLAN {
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

// hostView is everything vlanRows needs to know about the host, gathered once.
//
// Passed in rather than read inside the loop for two reasons. Every row is
// then answered from one snapshot, so a device appearing or going away
// half-way down the table cannot produce a listing that contradicts itself;
// and the interesting cases here are all about what the host says, which makes
// them untestable while the host is the only thing that can say it. The bug
// this structure was introduced for — every healthy tagged interface reported
// as a name collision — could not be written down as a test before.
type hostView struct {
	// links is every interface on the host: name to whether it is up.
	links map[string]bool
	// vlans is the subset of those that are tagged interfaces.
	vlans map[string]hostnet.VLANDevice
}

func (h hostView) exists(name string) bool { _, ok := h.links[name]; return ok }

// readHostView asks the host once for both halves.
func readHostView() hostView {
	v := hostView{links: map[string]bool{}, vlans: hostnet.VLANDevices()}
	ifis, err := net.Interfaces()
	if err != nil {
		return v
	}
	for _, ifi := range ifis {
		v.links[ifi.Name] = ifi.Flags&net.FlagUp != 0
	}
	return v
}

func vlanRows(vs []config.HostVLAN, host hostView) []vlanRow {
	out := make([]vlanRow, 0, len(vs))
	for _, v := range vs {
		name := v.VLANName()
		row := vlanRow{Parent: v.Parent, ID: v.ID, Name: name, Disabled: v.Disabled}
		row.Up, row.Present = host.links[name]
		dev, isVLAN := host.vlans[name]
		switch {
		case v.Disabled:
			// A parked row is not supposed to be there; saying so would be
			// handing the operator their own request back as a fault.
		case !row.Present:
			row.Problem = "not present on this host"
			if !host.exists(v.Parent) {
				row.Problem = "parent " + v.Parent + " is not on this host, so " + name + " cannot be created"
			}
		case !row.Up:
			row.Problem = name + " exists but is down, so it carries no traffic"
		case !hostnet.VLANSupported:
			// Nothing below this line can be answered on a host that has no
			// way to tell a tagged interface from anything else. The page
			// says so once at the top; repeating it per row, or worse
			// comparing against a lookup that always misses, would turn a
			// platform limit into a table full of faults.
		case !isVLAN:
			// Present and up, but not a tagged interface at all: the name
			// belongs to something else. This is the case worth catching — the
			// row would otherwise look healthy while the traffic went
			// somewhere unrelated.
			//
			// Reachable only because the host was asked a question it can
			// answer. The sysfs read this replaced could not, so it said "not
			// a VLAN" about every device including real ones, and this branch
			// fired on every working row.
			row.Problem = name + " exists but is not a tagged interface — something else on this host owns that name"
		case dev.ID != v.ID:
			row.Problem = fmt.Sprintf("%s carries vlan %d on this host, not %d", name, dev.ID, v.ID)
		case dev.Parent != "" && !strings.EqualFold(dev.Parent, v.Parent):
			// An empty parent is not a mismatch. A VLAN moved into another
			// namespace away from its lower link keeps an ifindex that
			// resolves to nothing here, and reporting that as "on the wrong
			// parent" would name a device the operator would then go looking
			// for.
			row.Problem = fmt.Sprintf("%s is on %s on this host, not %s", name, dev.Parent, v.Parent)
		case dev.QinQ:
			// Same tag, same parent, different ethertype. gravinet sends no
			// IFLA_VLAN_PROTOCOL, so it never creates one of these — a device
			// carrying 802.1ad under a gravinet definition was made by
			// something else and will not carry the traffic the row implies.
			row.Problem = fmt.Sprintf("%s carries tag %d as 802.1ad (QinQ), not 802.1Q — gravinet did not create this device", name, dev.ID)
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
