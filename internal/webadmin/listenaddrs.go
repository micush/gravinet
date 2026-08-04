package webadmin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"gravinet/internal/config"
)

// The admin interface binds a set of addresses rather than one. This file is
// the picker behind that: the options endpoint enumerates what this host could
// bind, and the save endpoint records the choice and asks for a restart.
//
// Only addresses are picked, never ports — the port stays in WebAdmin.Listen.
// That is the question an operator actually has here ("also answer on my LAN
// address", "stop answering on loopback"), and keeping the port out of it means
// the port advertised to peers for cluster management can't drift underneath
// the mesh as a side effect of an address edit.

// listenOption is one bindable address, with enough context to be recognisable
// in the picker — an IP alone is not something most people can place.
type listenOption struct {
	Addr    string `json:"addr"`           // the value: a bare IP literal
	Label   string `json:"label"`          // what the picker shows
	Iface   string `json:"iface"`          // interface it lives on
	Kind    string `json:"kind"`           // loopback | mesh | host | wildcard
	Default bool   `json:"default"`        // part of the default selection
	Note    string `json:"note,omitempty"` // why it's notable, if it is
}

// handleListenOptions enumerates the addresses this host can bind the admin
// interface to. Sourced from the live interface list rather than from config,
// so it reflects what is actually bindable right now.
func (s *Server) handleListenOptions(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	mesh := meshAddrSet(cfg)
	opts := s.enumerateListenOptions(mesh)

	// The selection shown is the configured one, or — when nothing has been
	// configured — the default the daemon is already running, so the picker
	// opens reflecting reality rather than empty.
	selected := cfg.ListenAddrsRaw()
	if len(selected) == 0 {
		for _, o := range opts {
			if o.Default {
				selected = append(selected, o.Addr)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"options":  opts,
		"selected": selected,
		"port":     cfg.WebAdminPort(),
		// Which address this request arrived on, so the UI can warn before
		// someone deselects the one they are using. It is the difference
		// between a restart and a lockout, and the browser cannot work it
		// out for itself when reaching the node through a proxy or a name.
		"current": localAddrOf(r),
	})
}

// meshAddrSet is the set of this node's overlay addresses, which are what make
// the admin interface reachable for cluster management from other peers.
//
// Taken from config rather than from the running engine: these are the
// configured overlay addresses, they are what the picker needs to mark as
// default, and reading them here avoids widening the Backend interface with a
// method that would exist for this one caller. A network still coming up has
// its address in config before the interface carries it, which is the right
// answer for a picker — you want to be able to select it either way.
func meshAddrSet(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	for _, n := range cfg.Networks {
		for _, raw := range []string{n.Address4, n.Address6} {
			if raw == "" {
				continue
			}
			// Stored as CIDR ("192.168.203.157/24"); the address is what binds.
			if p, err := netip.ParsePrefix(raw); err == nil {
				out[p.Addr().String()] = true
				continue
			}
			if a, err := netip.ParseAddr(raw); err == nil {
				out[a.String()] = true
			}
		}
	}
	return out
}

// enumerateListenOptions walks the host's interfaces and classifies each
// address. Ordering is deliberate — loopback, then mesh, then everything else —
// so the two addresses that make up the default sit at the top of the list
// rather than being buried among a laptop's dozen interfaces.
func (s *Server) enumerateListenOptions(mesh map[string]bool) []listenOption {
	ifaces, err := net.Interfaces()
	if err != nil {
		s.log.Debugf("webadmin: listen options: %v", err)
		return nil
	}
	var out []listenOption
	seen := map[string]bool{}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			p, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(p.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			// Link-local v6 needs a zone to be bound and is useless as an
			// admin address; skip rather than offer something that fails.
			if ip.IsLinkLocalUnicast() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			o := listenOption{Addr: ip.String(), Iface: ifc.Name}
			switch {
			case ip.IsLoopback():
				o.Kind, o.Default = "loopback", true
				o.Note = "always reachable from this host"
			case mesh[ip.String()]:
				o.Kind, o.Default = "mesh", true
				o.Note = "overlay address — other peers manage this node here"
			default:
				o.Kind = "host"
			}
			o.Label = ip.String() + " (" + ifc.Name + ")"
			out = append(out, o)
		}
	}
	rank := map[string]int{"loopback": 0, "mesh": 1, "host": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].Addr < out[j].Addr
	})
	// Offered last, never part of the default: a wildcard exposes the admin
	// interface on every address the host has now *and every one it gains
	// later*, which is a different and much broader decision than picking
	// addresses. It stays available because it is occasionally what someone
	// wants, but it should be chosen deliberately.
	out = append(out,
		listenOption{Addr: "0.0.0.0", Label: "0.0.0.0 (all IPv4)", Kind: "wildcard", Note: "every IPv4 address, including ones added later"},
		listenOption{Addr: "::", Label: ":: (all IPv6)", Kind: "wildcard", Note: "every IPv6 address, including ones added later"},
	)
	return out
}

// handleListenAddrsSave records a new pick list and asks the UI to restart.
//
// A listener set cannot be re-bound live in any way worth trusting: the
// primary socket is the one serving the request making the change, so applying
// it in place means answering through a socket that is being torn down. The
// rest of Settings already restarts for changes like this, and the UI does it
// quietly, so this follows that rather than inventing a live-rebind path whose
// failure mode is an unreachable node.
func (s *Server) handleListenAddrsSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Addrs []string `json:"addrs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	clean, err := validateListenAddrs(req.Addrs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	err = s.mutateConfig(r, func(cfg *config.Config) error {
		cfg.WebAdmin.ListenAddrs = clean
		// Keep Listen's host consistent with the pick list so the daemon's
		// primary bind and WebAdminPort agree with what was chosen. The port
		// is preserved untouched — this picker never moves the port, and the
		// mesh advertises that port to peers.
		if len(clean) > 0 {
			if _, ps, e := net.SplitHostPort(cfg.WebAdmin.Listen); e == nil {
				cfg.WebAdmin.Listen = net.JoinHostPort(primaryOf(clean), ps)
			}
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": true})
}

// primaryOf picks which address leads. Loopback if it was chosen: it is the
// one address that cannot stop existing underneath the daemon, so it is the
// safest thing to bind first and hardest to lock yourself out of. Otherwise
// the first pick, the operator having accepted that by deselecting loopback.
func primaryOf(addrs []string) string {
	for _, a := range addrs {
		if ip, err := netip.ParseAddr(a); err == nil && ip.IsLoopback() {
			return a
		}
	}
	return addrs[0]
}

// validateListenAddrs normalizes and checks a pick list.
func validateListenAddrs(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			// Names are refused rather than resolved: this binds a socket, and
			// a name resolving differently later would silently move what the
			// admin interface is exposed on.
			return nil, fmt.Errorf("%q is not an IP address", raw)
		}
		if ip.IsLinkLocalUnicast() {
			return nil, fmt.Errorf("%s is link-local and needs a zone to bind", ip)
		}
		k := ip.Unmap().String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	// Refusing an empty set is the one guard kept even though the operator
	// asked for the picker: saving nothing would leave the admin interface
	// bound to no address at all, which is not a configuration anyone means
	// and cannot be undone from the UI it just removed.
	if len(out) == 0 {
		return nil, fmt.Errorf("pick at least one address — saving none would leave the admin interface unreachable")
	}
	return out, nil
}

// localAddrOf reports the local address a request arrived on, so the UI can
// tell which entry in the picker is the one currently in use.
func localAddrOf(r *http.Request) string {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if h, _, err := net.SplitHostPort(a.String()); err == nil {
			return h
		}
	}
	return ""
}
