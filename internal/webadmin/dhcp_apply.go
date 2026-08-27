package webadmin

import (
	"bytes"
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
		keaStopAndDisable()
		// The config file is left on disk rather than deleted. It is a record
		// of what the operator configured, and it costs nothing — but it is
		// emphatically not what makes the server not serve. Only the unit
		// being stopped *and* disabled does that; see keaStopAndDisable.
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
		// What Kea would actually be given is worked out first, before
		// anything is installed or moved aside.
		//
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
			//
			// Disabled as well as stopped, and for a sharper reason than the
			// mode switch above: this branch returns *before* renderKea, so
			// whatever is on disk is the previous apply's file. An enabled
			// unit would come back at the next boot serving subnets the
			// operator has since removed.
			//
			// Returning here before the install is deliberate. Selecting
			// "server" in the role dropdown is an apply like any other, and
			// through v951 it installed the Kea package immediately — so
			// switching the role back from off just to look at a subnet table
			// pulled a DHCP server down from the distribution, then stopped
			// and disabled it a moment later on discovering there was nothing
			// to serve. The page's own hint has always said "saving a subnet
			// will install it"; this is that sentence being true.
			keaStopAndDisable()
			return noteworthy(missing), nil
		}
		installed := ""
		if !keaInstalled() {
			if err := installKea(); err != nil {
				return "configuration saved, but no DHCP server is available: " + err.Error(), nil
			}
			installed = "installed the Kea DHCPv4 server; "
		}
		backedUp := ""
		if !keaOwned(keaConfPath) {
			// Also after the check: setting an operator's hand-written
			// kea-dhcp4.conf aside is only justified by being about to write
			// one of our own.
			to, err := setAsideKeaConf(keaConfPath)
			if err != nil {
				return "", fmt.Errorf("could not set aside the existing %s: %w", keaConfPath, err)
			}
			backedUp = "kept the previous config at " + to + "; "
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
		return noteworthy(installed, backedUp, missing, relayIfaceNote(served), dhcpProblemNote(c)), nil
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
			"running":     dhcpRuntime(cfg.DHCP),
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
		Relays       []string `json:"relays"`
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
				// As with subnets above: adding a link does not turn the relay
				// on. The pill on the card does that.
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
				Router: strings.TrimSpace(req.Router), Relays: trimAll(req.Relays),
				DNS:          trimAll(req.DNS),
				Search:       trimAll(req.Search),
				LeaseSeconds: req.LeaseSeconds,
			}
			if err := e.Validate(); err != nil {
				return err
			}
			// The picker hides mesh devices, but a picker is a convenience,
			// not a control. This is what actually prevents it.
			//
			// Except on a relayed row, where the iface column is not the link
			// the clients are on — they are on the far side of a relay — but
			// the link this node reaches that relay over, and answers it
			// across. On the lab that is the mesh, and refusing it here left
			// the scope selectable and unanswerable (v978). A mesh device is
			// still refused for an attached subnet, which is the case the rule
			// was written for: that would put a DHCP server on the overlay.
			//
			// A relayed row cannot do that even naming one. The scope carries
			// no "interface" key, so it is selected by giaddr alone, and no
			// giaddr is an overlay address; a request arriving on the overlay
			// socket matches no scope and is dropped. The socket is a way out,
			// not a way in.
			if !e.Relayed() {
				if err := s.refuseMeshIface(e.Iface); err != nil {
					return err
				}
			}
			if req.Op == "add" {
				// Attached subnets only. Two of those on one link are two
				// answers to the same question — but a relayed subnet is
				// selected by giaddr, so any number of them share the link
				// their relays arrive over, and that sharing is the feature.
				// See DHCPConfig.Validate, which holds the same line for a
				// config arriving by any other route than this handler.
				//
				// The wording matters as much as the rule. Until v969 this
				// said only that the interface was taken, which is what an
				// operator hit while trying to add the second of several
				// remote LANs behind one uplink — a true sentence about a
				// restriction that no longer applies to what they were doing,
				// and no hint that the relay column was the answer.
				for _, x := range d.Subnets {
					if !e.Relayed() && !x.Relayed() && strings.EqualFold(x.Iface, e.Iface) {
						return fmt.Errorf("interface %s already has a directly attached subnet (%s) — a second subnet on one interface is only served if it is behind a relay, in which case fill in the relay address it forwards under",
							e.Iface, strings.TrimSpace(x.Subnet))
					}
				}
				d.Subnets = append(d.Subnets, e)
				// Adding a subnet does not turn the server on. The card's own
				// pill is the control, the way it is on every other card in
				// the console — a firewall rule added to a disabled firewall
				// does not enable it either. Auto-enabling here would flip a
				// switch sitting visibly on the same card, and on a node with
				// the relay running it would have to either silently stop the
				// relay or silently do nothing, both of which are worse than
				// leaving the operator to click the pill.
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

// keaBootAction is what reconcileKeaAtBoot has decided to do. Split out from
// the doing so the decision table can be exercised directly: the alternative
// is a test that writes to /etc/kea and drives systemd, which is not a test.
type keaBootAction int

const (
	// keaBootNothing: the file agrees with the configuration, or this node
	// does not serve. Write nothing, run nothing.
	keaBootNothing keaBootAction = iota
	// keaBootStopDisable: server mode with nothing servable.
	keaBootStopDisable
	// keaBootNotInstalled: configured to serve, no Kea to serve with.
	keaBootNotInstalled
	// keaBootNotOurs: the file on disk is somebody else's and disagrees.
	keaBootNotOurs
	// keaBootRewrite: the file is gravinet's, and stale.
	keaBootRewrite
)

// keaBootState is everything the decision depends on, gathered by the caller.
type keaBootState struct {
	Mode      config.DHCPMode
	Servable  int  // enabled subnets that name an interface this host has
	Installed bool // kea-dhcp4 present
	Owned     bool // keaOwned: absent, or carrying gravinet's marker
	Matches   bool // on-disk bytes equal what the stored config renders to
}

// keaBootDecision is reconcileKeaAtBoot's whole decision, as a function of
// what the host looks like. Ordering carries the meaning:
//
//   - Not serving is decided before anything is read. Kea is not this node's
//     business in relay or off mode, and the exclusion has already been
//     re-asserted by the caller.
//   - Nothing servable outranks everything below it, including "not
//     installed": there is no configuration to render either way, and an
//     enabled unit left from an earlier apply is the thing that needs
//     handling. applyDHCP orders these two the same way and for the same
//     reason.
//   - Matching outranks ownership. A file that already says what the
//     configuration says needs no decision about who wrote it, and asking
//     would only produce a warning about a file that is correct. (In practice
//     an unowned file cannot match, since renderKea always writes the marker
//     — but that is renderKea's property to keep, not something this
//     ordering should depend on.)
//   - Ownership outranks rewriting, which is the one that matters: a
//     hand-maintained kea-dhcp4.conf is never taken over during a boot.
func keaBootDecision(st keaBootState) keaBootAction {
	if st.Mode != config.DHCPServer {
		return keaBootNothing
	}
	if st.Servable == 0 {
		return keaBootStopDisable
	}
	if !st.Installed {
		return keaBootNotInstalled
	}
	if st.Matches {
		return keaBootNothing
	}
	if !st.Owned {
		return keaBootNotOurs
	}
	return keaBootRewrite
}

// reconcileKeaAtBoot brings kea-dhcp4.conf back into agreement with the stored
// configuration, and does nothing at all when the two already agree.
//
// applyDHCP is reached from the DHCP page and from nowhere else, so any path
// that changes the stored configuration without going through that page leaves
// the file behind. A history restore is the case that prompted this: the
// restore validates, saves and snapshots like any other edit, asks for a
// restart, and never re-renders Kea — so a restored server-mode configuration
// reached Kea when somebody next happened to re-save the page, and not before.
// On the same host that is worse than nothing running, because the unit is
// still enabled from the earlier apply: systemd starts Kea, Kea serves the
// pre-restore subnets, and the page shows the restored ones with nothing
// anywhere to say they are not what is in service.
//
// The no-churn property is the whole reason this is a comparison and not an
// apply. renderKea is pure, so what the stored configuration *would* produce
// is cheap to work out and compare against the bytes on disk; when they match
// — which is every ordinary restart of a node nobody has touched — this
// function writes nothing, starts nothing, and runs no systemctl at all. That
// was the objection to re-applying at boot, and it is answered by measuring
// rather than by not looking.
//
// Three things it deliberately does not do, all encoded in keaBootDecision:
//
//   - Install Kea. applyDHCP installs when an operator saves a subnet, which
//     is v951's rule and the page's own hint ("saving a subnet will install
//     it"). Pulling a package down from the distribution during daemon
//     startup is not that.
//   - Take over a file gravinet did not write. applyDHCP sets an operator's
//     hand-maintained kea-dhcp4.conf aside, justified by an operator having
//     just asked for it. Nothing at boot justifies that, so an unowned file is
//     left exactly where it is and the divergence is logged instead.
//   - Touch the unit when the file already agrees. If the config is right and
//     Kea is stopped, that is systemd's state and the runtime report says so
//     on the page; re-enabling it at every boot would fight an operator who
//     stopped it on purpose. This function reconciles the file.
//
// Failures are logged rather than returned. The caller's error channel belongs
// to the relay, and a Kea problem reported under "dhcp relay:" would send a
// reader to the wrong half of the page — while dhcpRuntime already reports
// what is actually serving, which is the same reasoning keaStopAndDisable
// gives for reporting neither of its own results.
func reconcileKeaAtBoot(c config.DHCPConfig) {
	if c.Mode != config.DHCPServer {
		return
	}
	served, _ := servableSubnets(c)
	st := keaBootState{
		Mode:      c.Mode,
		Servable:  len(served.EnabledSubnets()),
		Installed: keaInstalled(),
		Owned:     keaOwned(keaConfPath),
	}

	// Rendered before the decision, because Matches needs it — and a render
	// that fails is reported here rather than encoded as a fourth outcome, so
	// the decision table stays about the host rather than about our own bugs.
	var want []byte
	if st.Mode == config.DHCPServer && st.Servable > 0 {
		var err error
		if want, err = renderKea(served); err != nil {
			logx.Warnf("dhcp: could not render the stored Kea configuration: %v", err)
			return
		}
		if have, rerr := os.ReadFile(keaConfPath); rerr == nil {
			st.Matches = bytes.Equal(bytes.TrimSpace(have), bytes.TrimSpace(want))
		}
	}

	switch keaBootDecision(st) {
	case keaBootNothing:
		return

	case keaBootStopDisable:
		// Server mode with nothing servable. applyDHCP stops and disables here
		// for a reason that names this exact moment — an enabled unit would
		// come back at the next boot serving subnets the operator has since
		// removed — and this is that boot. Reached when a restore brings back
		// a configuration whose subnets are all parked, or all name
		// interfaces this host does not have.
		keaStopAndDisable()

	case keaBootNotInstalled:
		logx.Warnf("dhcp: this node is configured to serve DHCP but the Kea DHCPv4 server is not installed; save the DHCP page to install it")

	case keaBootNotOurs:
		logx.Warnf("dhcp: %s was not written by gravinet and does not match this node's stored DHCP configuration; leaving it alone. Save the DHCP page to take it over (the existing file is kept).", keaConfPath)

	case keaBootRewrite:
		if err := os.MkdirAll(filepath.Dir(keaConfPath), 0o755); err != nil {
			logx.Warnf("dhcp: creating %s: %v", filepath.Dir(keaConfPath), err)
			return
		}
		if err := os.WriteFile(keaConfPath, want, 0o644); err != nil {
			logx.Warnf("dhcp: write %s: %v", keaConfPath, err)
			return
		}
		// Kea's own parser before systemd, for the reason applyDHCP gives: a
		// file it rejects produces a unit that exits immediately with the
		// explanation in the journal. Here there is no operator watching a
		// save, so the reason goes in gravinet's log with the rest of the boot.
		if why, ok := keaTestConf(keaConfPath); !ok {
			logx.Warnf("dhcp: rewrote %s from the stored configuration but Kea will not accept it: %s", keaConfPath, why)
			return
		}
		if !keaService("restart") {
			logx.Warnf("dhcp: rewrote %s from the stored configuration but the Kea service would not start — check `journalctl -u %s`", keaConfPath, keaUnit())
			return
		}
		keaService("enable")
		logx.Infof("dhcp: %s did not match this node's stored DHCP configuration and was rewritten from it; Kea restarted", keaConfPath)
	}
}

// StartDHCPRelay brings this node's DHCP role back up at daemon startup, so a
// node configured to relay is relaying again after a restart without anyone
// opening the page.
//
// Not the whole apply: Kea is not installed here, an operator's own config is
// never taken over here, and a server whose file already matches the stored
// configuration is not bounced — that last one was the objection to doing
// anything at all at boot, and reconcileKeaAtBoot answers it by comparing
// rather than by re-applying. What is re-asserted is the *exclusion*, and the
// agreement between the stored configuration and the file Kea parses. Neither
// is churn and neither is optional.
//
// A node that ever served and then switched away still has the Kea unit
// enabled from that earlier apply — see keaStopAndDisable for how, and why
// stopping alone never made that stick. Left alone, such a node comes back
// from a reboot serving leases while this function starts the relay beside it.
// Disabling here is what heals a node that was already in that state before
// this code existed, since the alternative is waiting for somebody to happen
// to re-save the DHCP page.
//
// The same "waiting for somebody to re-save the page" is what reconcileKeaAtBoot
// removes on the server side, where the stored configuration can be changed by
// a path that never renders Kea at all — a history restore being the one that
// prompted it. See that function for what it will and will not do.
//
// Idempotent and cheap: disabling an already-disabled unit is a no-op, the
// reconcile writes nothing when there is nothing to reconcile, and the whole
// thing runs once per daemon start.
//
// Returns an error rather than logging one, so the caller decides how loud a
// failed relay is at boot. The reconcile logs its own failures instead of
// returning them, so a Kea problem is not reported to the caller as a relay
// one.
func StartDHCPRelay(c config.DHCPConfig) error {
	if c.Mode != config.DHCPServer {
		keaStopAndDisable()
	}
	reconcileKeaAtBoot(c)
	if !c.RelayActive() {
		return nil
	}
	return dhcpRelay.Apply(c)
}

// StopDHCPRelay shuts the relay down on clean exit, releasing port 67 so a
// restart does not race its own predecessor for the socket.
func StopDHCPRelay() { dhcpRelay.Stop() }

// keaStopAndDisable takes the Kea unit out of service in a way that survives a
// reboot.
//
// Stopping alone does not. The server branch runs `systemctl enable` so a node
// configured to serve is serving again after a restart without anyone opening
// the page — and through v950 nothing ever ran `disable`, so that enablement
// outlived every switch away from server mode. The sequence is ordinary:
//
//	role = server, add a subnet   -> Kea started, and enabled for boot
//	role = relay                  -> Kea stopped now; still enabled
//	reboot                        -> systemd starts Kea; gravinet starts the
//	                                 relay from the stored mode
//
// and the node comes back serving leases *and* relaying, on the same links.
// That is the exact failure config.DHCPMode exists to make unrepresentable —
// unrepresentable in the configuration, and entirely representable on the host,
// because the mutual exclusion was only ever enforced at apply time against a
// service whose boot behaviour gravinet had set and never unset. A relay
// forwarding to a central server while a local Kea answers the same broadcasts
// gives every client on that link two servers racing, and which one wins is
// whichever reply arrives first.
//
// Both are attempted regardless of the other's result: a unit that is already
// stopped still needs disabling, and a `disable` that fails should not stop the
// `stop` from having happened. Neither result is reported, matching the
// existing calls — the runtime report (see dhcp_runtime.go) is what tells an
// operator whether the server is actually running, rather than a note about
// one systemctl invocation.
func keaStopAndDisable() {
	keaService("stop")
	keaService("disable")
}
