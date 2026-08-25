package webadmin

import (
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/dhcrelay"
	"gravinet/internal/logx"
)

// Applying DHCP. One mode at a time, and the switch between them is the part
// worth being careful about: leaving the previous role running is how a node
// ends up both serving and relaying, which is the failure config.DHCPMode
// exists to make unrepresentable. So every apply stops the role that is not
// selected before starting the one that is, including when neither is.

// relayRunner is how this package starts and stops the in-tree relay. A
// package-level indirection rather than a direct call so the handler can be
// exercised without binding port 67, which a test cannot do and should not.
type relayRunner interface {
	Apply(config.DHCPConfig) error
	Stop()
}

var dhcpRelay relayRunner = &liveRelay{}

// liveRelay drives internal/dhcrelay.
type liveRelay struct{ cur *dhcrelay.Relay }

func (l *liveRelay) Apply(c config.DHCPConfig) error {
	l.Stop()
	if !c.RelayActive() {
		return nil
	}
	// EnabledLinks has already dropped the parked rows, the ones naming no
	// interface, and the ones with nowhere to forward to — so every link built
	// here is one that should be listening.
	var links []dhcrelay.Link
	for _, e := range c.EnabledLinks() {
		var servers []netip.Addr
		for _, s := range e.Servers {
			if a, err := netip.ParseAddr(strings.TrimSpace(s)); err == nil && a.Is4() {
				servers = append(servers, a)
			}
		}
		if len(servers) == 0 {
			continue
		}
		links = append(links, dhcrelay.Link{
			Iface:   strings.TrimSpace(e.Iface),
			Servers: servers,
			MaxHops: e.MaxHops,
		})
	}
	if len(links) == 0 {
		return nil
	}
	r, err := dhcrelay.Start(dhcrelay.Config{Links: links}, logRelay)
	if err != nil {
		return err
	}
	l.cur = r
	return nil
}

func (l *liveRelay) Stop() {
	if l.cur != nil {
		l.cur.Stop()
		l.cur = nil
	}
}

// applyDHCP makes the host match the configuration, and returns an
// operator-facing note describing anything that happened which the page
// cannot show for itself.
//
// The note follows the same contract as every other handler in this package
// and as applyRouterAdvert's: a caveat, a side effect, an outcome that did not
// match what the request implied — and "" when a request simply worked. A
// count of the subnets the operator just typed is not a caveat.
func applyDHCP(c config.DHCPConfig) (note string, err error) {
	// Whichever role is not selected stops first. Doing this before starting
	// the other is what keeps the two from overlapping even for the moment an
	// apply takes: a relay still forwarding while Kea starts answering would
	// give the clients on that link two servers racing.
	if c.Mode != config.DHCPRelay {
		dhcpRelay.Stop()
	}
	if c.Mode != config.DHCPServer {
		keaService("stop")
		// The config file is left on disk rather than deleted. It is a record
		// of what the operator configured, it costs nothing, and a stopped
		// unit is what actually makes the server not serve.
	}

	switch c.Mode {
	case config.DHCPRelay:
		if err := dhcpRelay.Apply(c); err != nil {
			return "", fmt.Errorf("starting the DHCP relay: %w", err)
		}
		// Same treatment the server half gets: a link that cannot do its job
		// is a property of the host rather than of what was just saved, so it
		// is reported here rather than refused above.
		return noteworthy(dhcpProblemNote(c)), nil

	case config.DHCPServer:
		installed := ""
		if !keaInstalled() {
			if err := installKea(); err != nil {
				return "configuration saved, but no DHCP server is available: " + err.Error(), nil
			}
			installed = "installed the Kea DHCPv4 server; "
		}
		backedUp := ""
		if !keaOwned(keaConfPath) {
			to, err := setAsideKeaConf(keaConfPath)
			if err != nil {
				return "", fmt.Errorf("could not set aside the existing %s: %w", keaConfPath, err)
			}
			backedUp = "kept the previous config at " + to + "; "
		}
		// Kea rejects a whole file for one interface it cannot find, so a
		// subnet naming an absent one is left out rather than allowed to stop
		// the LANs that are fine. Reported, never silent.
		served, dropped := servableSubnets(c)
		missing := ""
		if len(dropped) > 0 {
			missing = fmt.Sprintf("left out the subnet(s) on %s: no such interface on this host, and Kea refuses an entire configuration for one it cannot find; ",
				strings.Join(dropped, ", "))
		}
		if len(served.EnabledSubnets()) == 0 {
			// Server mode with nothing to serve. Kea refuses to start with no
			// subnet4, so stopping is both what the operator meant and the
			// only thing that would not crash-loop.
			keaService("stop")
			return noteworthy(installed, backedUp, missing), nil
		}
		conf, err := renderKea(served)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(keaConfPath), 0o755); err != nil {
			return "", fmt.Errorf("creating %s: %w", filepath.Dir(keaConfPath), err)
		}
		if err := os.WriteFile(keaConfPath, conf, 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", keaConfPath, err)
		}
		// Ask Kea's own parser before asking systemd to start it. A config it
		// rejects produces a unit that exits immediately, and the only account
		// of why is in the journal — so the operator is told to go and read a
		// log to find out what the thing they just saved did wrong. The parser
		// will say exactly what and where, so say that instead.
		if why, ok := keaTestConf(keaConfPath); !ok {
			return "", fmt.Errorf("wrote %s but Kea will not accept it: %s", keaConfPath, why)
		}
		if !keaService("restart") {
			return "", fmt.Errorf("wrote %s but the Kea service would not start — check `journalctl -u %s`", keaConfPath, keaUnit())
		}
		keaService("enable")
		return noteworthy(installed, backedUp, missing, dhcpProblemNote(c)), nil
	}
	return "", nil
}

// handleDHCP serves the System > DHCP editor.
func (s *Server) handleDHCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// Mesh devices are excluded from the picker for the same reason they
		// are on the RA page: serving DHCP into the overlay would hand mesh
		// peers a lease, and relaying out of it would forward their traffic
		// to a LAN server that knows nothing about them.
		meshIfaces := s.dhcpMeshIfaces()
		skip := make(map[string]bool, len(meshIfaces))
		for _, n := range meshIfaces {
			skip[n] = true
		}
		// The prefill rides along with the configuration rather than being a
		// second request, so the addresses the editor suggests from are read
		// from the same node, at the same moment, as the rows it is filling
		// in. Fetched separately they could come from either side of a switch
		// of the selected peer.
		writeJSON(w, http.StatusOK, map[string]any{
			"dhcp":        cfg.DHCP,
			"installed":   keaInstalled(),
			"owned":       keaOwned(keaConfPath),
			"mesh_ifaces": meshIfaces,
			"problems":    dhcpProblems(cfg.DHCP),
			"suggest":     dhcpSuggestions(skip),
			"system_dns":  systemDNSv4(),
		})
		return
	}

	var req struct {
		Op           string   `json:"op"`
		Mode         string   `json:"mode"`
		Iface        string   `json:"iface"`
		Subnet       string   `json:"subnet"`
		PoolStart    string   `json:"pool_start"`
		PoolEnd      string   `json:"pool_end"`
		Router       string   `json:"router"`
		DNS          []string `json:"dns"`
		Search       []string `json:"search"`
		LeaseSeconds int      `json:"lease_seconds"`
		Servers      []string `json:"servers"`
		MaxHops      int      `json:"max_hops"`
		Index        int      `json:"index"`
	}
	if !decode(w, r, &req) {
		return
	}

	err := s.mutateConfig(r, func(cfg *config.Config) error {
		d := &cfg.DHCP
		switch req.Op {
		case "mode":
			m := config.DHCPMode(strings.ToLower(strings.TrimSpace(req.Mode)))
			if err := config.ValidDHCPMode(m); err != nil {
				return err
			}
			d.Mode = m
		case "relay-add", "relay-update":
			e := config.DHCPRelayLink{
				Iface:   strings.TrimSpace(req.Iface),
				Servers: trimAll(req.Servers),
				MaxHops: req.MaxHops,
			}
			if e.Iface == "" {
				return fmt.Errorf("choose an interface to relay from")
			}
			if err := e.Validate(); err != nil {
				return err
			}
			// The picker hides mesh devices, but a picker is a convenience,
			// not a control. This is what actually prevents it.
			if err := s.refuseMeshIface(e.Iface); err != nil {
				return err
			}
			if req.Op == "relay-add" {
				for _, x := range d.Relay.Links {
					if strings.EqualFold(x.Iface, e.Iface) {
						return fmt.Errorf("interface %s already has a relay entry", e.Iface)
					}
				}
				d.Relay.Links = append(d.Relay.Links, e)
				// Adding the first link selects relay mode, the way adding the
				// first subnet selects server mode. It cannot also leave Kea
				// serving: Mode is one field.
				if d.Mode == config.DHCPOff {
					d.Mode = config.DHCPRelay
				}
			} else {
				if req.Index < 0 || req.Index >= len(d.Relay.Links) {
					return fmt.Errorf("no relay entry at index %d", req.Index)
				}
				// An edit must not collide with a different row's interface.
				for i, x := range d.Relay.Links {
					if i != req.Index && strings.EqualFold(x.Iface, e.Iface) {
						return fmt.Errorf("interface %s already has a relay entry", e.Iface)
					}
				}
				e.Disabled = d.Relay.Links[req.Index].Disabled // preserve state
				d.Relay.Links[req.Index] = e
			}
		case "relay-delete", "relay-remove":
			if req.Index < 0 || req.Index >= len(d.Relay.Links) {
				return fmt.Errorf("no relay entry at index %d", req.Index)
			}
			d.Relay.Links = append(d.Relay.Links[:req.Index], d.Relay.Links[req.Index+1:]...)
		case "relay-enable", "relay-disable":
			if req.Index < 0 || req.Index >= len(d.Relay.Links) {
				return fmt.Errorf("no relay entry at index %d", req.Index)
			}
			d.Relay.Links[req.Index].Disabled = req.Op == "relay-disable"
		case "add", "update":
			e := config.DHCPSubnet{
				Iface: strings.TrimSpace(req.Iface), Subnet: strings.TrimSpace(req.Subnet),
				PoolStart: strings.TrimSpace(req.PoolStart), PoolEnd: strings.TrimSpace(req.PoolEnd),
				Router: strings.TrimSpace(req.Router),
				DNS:    trimAll(req.DNS), Search: trimAll(req.Search),
				LeaseSeconds: req.LeaseSeconds,
			}
			if err := e.Validate(); err != nil {
				return err
			}
			// The picker hides mesh devices, but a picker is a convenience,
			// not a control. This is what actually prevents it.
			if err := s.refuseMeshIface(e.Iface); err != nil {
				return err
			}
			if req.Op == "add" {
				for _, x := range d.Subnets {
					if strings.EqualFold(x.Iface, e.Iface) {
						return fmt.Errorf("interface %s already has a subnet configured", e.Iface)
					}
				}
				d.Subnets = append(d.Subnets, e)
				// Adding the first subnet selects server mode, the way adding
				// the first RA interface turns that feature on. It cannot
				// also leave the relay running: Mode is one field.
				if d.Mode == config.DHCPOff {
					d.Mode = config.DHCPServer
				}
			} else {
				if req.Index < 0 || req.Index >= len(d.Subnets) {
					return fmt.Errorf("no subnet at index %d", req.Index)
				}
				e.Disabled = d.Subnets[req.Index].Disabled // preserve state
				d.Subnets[req.Index] = e
			}
		case "delete", "remove":
			if req.Index < 0 || req.Index >= len(d.Subnets) {
				return fmt.Errorf("no subnet at index %d", req.Index)
			}
			d.Subnets = append(d.Subnets[:req.Index], d.Subnets[req.Index+1:]...)
		case "enable", "disable":
			if req.Index < 0 || req.Index >= len(d.Subnets) {
				return fmt.Errorf("no subnet at index %d", req.Index)
			}
			d.Subnets[req.Index].Disabled = req.Op == "disable"
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// Apply from the saved config, so what runs is what was stored rather
	// than what was requested.
	var note string
	if cfg, lerr := config.Load(s.configPath); lerr == nil {
		if n, aerr := applyDHCP(cfg.DHCP); aerr != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": false, "note": "saved, but: " + aerr.Error()})
			return
		} else {
			note = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": false, "note": note})
}

// logRelay is the relay's log sink. The relay runs inside the daemon rather
// than as a unit of its own, so its lines belong in gravinet's log and not in
// journalctl under some other name — which is also the answer to "where do I
// look" being different for the two halves of this page.
func logRelay(format string, args ...any) { logx.Warnf(format, args...) }

// StartDHCPRelay brings the relay up at daemon startup, so a node configured
// to relay is relaying again after a restart without anyone opening the page.
//
// The relay only, not the whole apply. Kea is a systemd unit that gravinet has
// already enabled, so it comes back on its own; re-rendering its config and
// bouncing it on every gravinet restart would be churn for no gain. The relay
// has no unit of its own — it runs inside this process — so it is the half
// that needs starting here.
//
// Returns an error rather than logging one, so the caller decides how loud a
// failed relay is at boot.
func StartDHCPRelay(c config.DHCPConfig) error {
	if !c.RelayActive() {
		return nil
	}
	return dhcpRelay.Apply(c)
}

// StopDHCPRelay shuts the relay down on clean exit, releasing port 67 so a
// restart does not race its own predecessor for the socket.
func StopDHCPRelay() { dhcpRelay.Stop() }
