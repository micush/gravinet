package webadmin

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/dhcrelay"
	"gravinet/internal/logx"
)

// Applying DHCP. One card, one switch: the relay is either running on the
// links configured for it or it is not.
//
// gravinet also served leases of its own through Kea until v988, and most of
// what used to live in this file was the care that took — stopping the role
// that was not selected before starting the one that was, so a node was never
// briefly both. With one role left there is no other role to stop, and an
// apply is the ordinary shape every other feature here has: work out what
// should be running, make that be what is running, report anything the page
// cannot show for itself.

// relayRunner is how this package starts and stops the in-tree relay. A
// package-level indirection rather than a direct call so the handler can be
// exercised without binding port 67, which a test cannot do and should not.
type relayRunner interface {
	Apply(config.DHCPConfig) error
	Stop()
	// Listening reports the links actually bound right now, so the page can
	// show what is running rather than what was last selected.
	Listening() []string
}

var dhcpRelay relayRunner = &liveRelay{}

// liveRelay drives internal/dhcrelay.
type liveRelay struct{ cur *dhcrelay.Relay }

func (l *liveRelay) Listening() []string { return l.cur.Listening() }

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
// operator-facing note describing anything that happened which the page cannot
// show for itself.
//
// The note follows the same contract as every other handler in this package
// and as applyRouterAdvert's: a caveat, a side effect, an outcome that did not
// match what the request implied — and "" when a request simply worked. A
// count of the links the operator just typed is not a caveat.
func applyDHCP(c config.DHCPConfig) (note string, err error) {
	// Stopping first, unconditionally, is what makes this idempotent: Apply
	// stops before it starts, and the mode being off is the case where
	// stopping is the whole of the work.
	if err := dhcpRelay.Apply(c); err != nil {
		return "", fmt.Errorf("starting the DHCP relay: %w", err)
	}
	// A link that cannot do its job is a property of the host rather than of
	// what was just saved — an interface with no address, or none by that
	// name — so it is reported here rather than refused on the way in.
	return noteworthy(dhcpProblemNote(c)), nil
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
		// are on the RA page: relaying out of the overlay would forward mesh
		// peers' requests to a LAN server that knows nothing about them.
		writeJSON(w, http.StatusOK, map[string]any{
			"dhcp":        cfg.DHCP,
			"mesh_ifaces": s.dhcpMeshIfaces(),
			"problems":    dhcpProblems(cfg.DHCP),
			"running":     dhcpRuntime(cfg.DHCP),
		})
		return
	}

	var req struct {
		Op      string   `json:"op"`
		Mode    string   `json:"mode"`
		Iface   string   `json:"iface"`
		Servers []string `json:"servers"`
		MaxHops int      `json:"max_hops"`
		Index   int      `json:"index"`
	}
	if !decode(w, r, &req) {
		return
	}

	err := s.mutateConfig(r, func(cfg *config.Config) error {
		d := &cfg.DHCP
		switch req.Op {
		case "mode":
			m := config.DHCPMode(strings.ToLower(strings.TrimSpace(req.Mode)))
			// Checked before it is stored, not after. Config.Validate would
			// quietly fold the retired server mode to off (see
			// migrateServerMode), so validating only on the way out would
			// answer somebody asking for a role that no longer exists with a
			// silent success.
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
				// Adding a link does not turn the relay on. The pill by the
				// page title does that, the way it does on every other page in
				// the console — a firewall rule added to a disabled firewall
				// does not enable it either.
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
// journalctl under some other name.
func logRelay(format string, args ...any) { logx.Warnf(format, args...) }

// StartDHCPRelay brings the relay back up at daemon startup, so a node
// configured to relay is relaying again after a restart without anyone opening
// the page.
//
// Also the one place that says anything about the DHCP server this release
// removed. A node that was serving comes back from this upgrade with its Kea
// still installed, still enabled, and still handing out the leases in the file
// gravinet last wrote for it — nothing here stops that, deliberately, because
// a release that reached out to stop a daemon during an upgrade would take a
// working LAN down for people who never asked. What it must not do is leave
// that unsaid: the page that configured it is gone, so without a line in the
// log the only evidence left is a server nothing in the console admits to.
//
// Said once per start, and only while the stored mode still says "server" —
// which is until the configuration is next written, since migrateServerMode
// clears it on the way through. That is the right lifetime for it: the warning
// is about an upgrade, not a standing condition, and an operator who has since
// edited anything on this node has been past the page and seen the server card
// missing.
//
// Returns an error rather than logging one, so the caller decides how loud a
// failed relay is at boot.
func StartDHCPRelay(c config.DHCPConfig) error {
	if c.RetiredServerMode() {
		logx.Warnf("dhcp: this node's stored configuration has it serving DHCP through Kea, a role gravinet no longer has (removed in v988). " +
			"gravinet has not touched the Kea service: if it was running it is still running, still enabled at boot, and still serving " +
			"/etc/kea/kea-dhcp4.conf, which nothing manages now. Manage it directly, or `systemctl disable --now` it. " +
			"The subnets it was given are in this node's config history if they are worth recreating elsewhere.")
	}
	if !c.RelayActive() {
		return nil
	}
	return dhcpRelay.Apply(c)
}

// StopDHCPRelay shuts the relay down on clean exit, releasing port 67 so a
// restart does not race its own predecessor for the socket.
func StopDHCPRelay() { dhcpRelay.Stop() }
