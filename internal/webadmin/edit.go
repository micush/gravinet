package webadmin

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/service"
)

// The handlers below give the web UI the same config-editing surface as the CLI.
// Each loads the config file, applies a shared config op, validates, saves, and
// triggers a live reload (via mutateConfig). The "restart" flag in the reply
// tells the UI whether the change took effect immediately (NAT/QoS/bandwidth) or
// needs a service restart to apply (networks/routes/addressing).

// editResult writes the standard {ok, restart} / {error} reply.
func (s *Server) editResult(w http.ResponseWriter, err error, restart bool) {
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": restart})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// Admin API requests are small JSON objects; bound the body so a client can't
	// stream an unbounded request.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return false
	}
	return true
}

// handleNetwork: add / delete / enable / disable / rename / notes / subnet /
// join / redistribute-bgp. Most are structural (need a restart to bring
// interfaces up/down or re-home addresses); rename, notes, and
// redistribute-bgp are local changes and apply live.
func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op, Net, Id, NewName, Subnet4, Subnet6, Address4, Address6, Key, Peer, Token, Notes string
		Enabled                                                                             bool
		Metric                                                                              int
		Routes                                                                              []string // redistribute-bgp: selected BGP-learned CIDRs (see NetworkSetRedistributeBGPRoutes)
	}
	if !decode(w, r, &req) {
		return
	}
	// Network lifecycle (add/delete/enable/disable/join/rename) applies live via
	// the reload hook, which diffs the config and brings networks up or down
	// without a restart. Re-addressing changes (subnet, overlay address) still
	// need one — the hot reload does not re-address a running interface.
	restart := false
	// Captured for reconcileMeshRedistribute below: disabling or deleting a
	// network takes its routes off the Mesh Routes page (meshRouteCIDRs skips
	// disabled networks entirely) just as surely as deleting the routes
	// themselves would.
	var prevMesh []string
	err := s.mutateConfig(func(cfg *config.Config) error {
		prevMesh = meshRouteCIDRs(cfg)
		switch req.Op {
		case "add":
			_, e := cfg.NetworkAdd(req.Net, req.Subnet4, req.Subnet6)
			return e
		case "delete", "del", "remove":
			return cfg.NetworkDelete(req.Net)
		case "enable":
			return cfg.NetworkSetEnabled(req.Net, true)
		case "disable":
			return cfg.NetworkSetEnabled(req.Net, false)
		case "rename":
			return cfg.NetworkRename(req.Net, req.NewName)
		case "notes":
			return cfg.NetworkSetNotes(req.Net, req.Notes)
		case "subnet":
			restart = true // re-addressing an interface needs a restart
			return cfg.NetworkSetSubnets(req.Net, req.Subnet4, req.Subnet6)
		case "address":
			restart = true // adopting a changed overlay address needs a restart
			return cfg.NetworkSetAddress(req.Net, req.Address4, req.Address6)
		case "join":
			return cfg.NetworkJoin(req.Id, req.Key, req.Peer, req.Subnet4, req.Subnet6)
		case "join-token":
			_, _, e := cfg.NetworkJoinToken(req.Token)
			return e
		case "redistribute-bgp":
			return cfg.NetworkSetRedistributeBGPRoutes(req.Net, req.Routes, req.Metric)
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	if err == nil {
		s.reconcileMeshRedistribute(prevMesh)
		// Only redistribute-bgp itself needs an immediate resync — it's the
		// only op above that can change which networks want BGP-into-mesh
		// redistribution, or the metric they redistribute at, without also
		// changing something reconcileMeshRedistribute already reacted to.
		// (disable/delete also affect it — meshRouteCIDRs-style eligibility
		// — but bgpMeshRedistributor's own poll interval catches those
		// within bgpRedistributePollInterval regardless, the same
		// eventually-consistent tolerance every other op above already
		// accepts from the plain reload hook for anything that isn't routes
		// or BGP.) Guarded: s.bgpRedis is nil until Start() runs (see
		// reconcileMeshRedistribute's own guards elsewhere for the same
		// "not wired up in this test/context" shape).
		if req.Op == "redistribute-bgp" && s.bgpRedis != nil {
			go s.bgpRedis.sync()
		}
	}
	s.editResult(w, err, restart)
}

// handleNetworkToken mints a join token for a network (read-only — it doesn't
// change config). The token bundles the network id, enabled key(s), subnets,
// and seeds so another node can join by pasting it. addr (optional) advertises
// this node's reachable underlay endpoint as a seed; expires (optional Go
// duration, e.g. "24h") time-boxes the token.
func (s *Server) handleNetworkToken(w http.ResponseWriter, r *http.Request) {
	var req struct{ Net, Addr, Expires string }
	if !decode(w, r, &req) {
		return
	}
	if s.configPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"error": "config not available"})
		return
	}
	cfg, err := config.Load(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		return
	}
	var extra []string
	if a := strings.TrimSpace(req.Addr); a != "" {
		extra = append(extra, a)
	}
	var ttl time.Duration
	if e := strings.TrimSpace(req.Expires); e != "" {
		d, derr := time.ParseDuration(e)
		if derr != nil || d <= 0 {
			writeJSON(w, http.StatusOK, map[string]any{"error": "invalid expiry — use a duration like 24h or 72h"})
			return
		}
		ttl = d
	}
	tok, err := cfg.NetworkToken(req.Net, extra, ttl)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "seeds": cfg.TokenSeedCount(req.Net, extra)})
}

// live (the daemon dials the new endpoint); removing takes effect on the next
// restart. The current seed list is served with the network config, so there is
// no GET here.
// handleHost: add / update / remove / enable / disable custom name -> IP records
// advertised mesh-wide, plus reject / reject-remove / reject-enable /
// reject-disable for the local refuse-list of peer-advertised hostnames. Applied
// live by the reload (advertised on add or enable, withdrawn on remove or
// disable; rejected names drop out of the hosts file on the next sync).
func (s *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	var req struct{ Op, Net, Name, NewName, IP string }
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		switch req.Op {
		case "add":
			return cfg.HostAdd(req.Net, req.Name, req.IP)
		case "update":
			return cfg.HostUpdate(req.Net, req.Name, req.NewName, req.IP)
		case "remove", "delete":
			return cfg.HostDelete(req.Net, req.Name)
		case "enable":
			return cfg.HostSetEnabled(req.Net, req.Name, true)
		case "disable":
			return cfg.HostSetEnabled(req.Net, req.Name, false)
		case "reject":
			return cfg.HostRejectAdd(req.Net, req.Name)
		case "reject-remove":
			return cfg.HostRejectDelete(req.Net, req.Name)
		case "reject-enable":
			return cfg.HostRejectSetEnabled(req.Net, req.Name, true)
		case "reject-disable":
			return cfg.HostRejectSetEnabled(req.Net, req.Name, false)
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	s.editResult(w, err, false)
}

// handleDNS: add / update / remove / enable / disable conditional-forwarding
// domains advertised mesh-wide, plus reject / reject-remove / reject-enable /
// reject-disable for the local refuse-list of peer-advertised forwards. Each
// advertised domain also doubles as a plain search-domain suffix for this
// node's own OS resolver (tried automatically for an unqualified query) —
// there's no separate op or list for that, it rides along with the routing
// domain itself. All apply live by the reload. Mirrors handleHost exactly,
// with a comma-separated server list in place of a single IP.
//
// There is deliberately no op to flip DNSSync.Enabled here: like HostsSync, it
// defaults on and has no API/GUI surface at all — control happens through
// reject, not a master switch. Enabled=false is config-file only, for the rare
// case an operator wants a node to advertise but never apply anything locally.
func (s *Server) handleDNS(w http.ResponseWriter, r *http.Request) {
	var req struct{ Op, Net, Domain, NewDomain, Servers string }
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		switch req.Op {
		case "add":
			return cfg.DNSForwardAdd(req.Net, req.Domain, req.Servers)
		case "update":
			return cfg.DNSForwardUpdate(req.Net, req.Domain, req.NewDomain, req.Servers)
		case "remove", "delete":
			return cfg.DNSForwardDelete(req.Net, req.Domain)
		case "enable":
			return cfg.DNSForwardSetEnabled(req.Net, req.Domain, true)
		case "disable":
			return cfg.DNSForwardSetEnabled(req.Net, req.Domain, false)
		case "reject":
			return cfg.DNSRejectAdd(req.Net, req.Domain)
		case "reject-remove":
			return cfg.DNSRejectDelete(req.Net, req.Domain)
		case "reject-enable":
			return cfg.DNSRejectSetEnabled(req.Net, req.Domain, true)
		case "reject-disable":
			return cfg.DNSRejectSetEnabled(req.Net, req.Domain, false)
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	s.editResult(w, err, false)
}
func (s *Server) handleSeed(w http.ResponseWriter, r *http.Request) {
	var req struct{ Op, Net, Addr, Notes, NewAddr string }
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		switch req.Op {
		case "add":
			return cfg.SeedAdd(req.Net, req.Addr)
		case "remove", "delete":
			return cfg.SeedRemove(req.Net, req.Addr)
		case "notes":
			return cfg.SeedSetNotes(req.Net, req.Addr, req.Notes)
		case "update-addr":
			return cfg.SeedUpdateAddr(req.Net, req.Addr, req.NewAddr)
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	s.editResult(w, err, false)
}

// handleRoute: add / redistribute / delete / reject, plus enable / disable (an
// advertised route) and reject-enable / reject-disable (a reject entry), all by
// CIDR. Applied live by the reload — added routes are advertised and removed or
// disabled ones withdrawn without a restart.
func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op, Net, CIDR string
		Metric        int
		Inclusive     bool
	}
	if !decode(w, r, &req) {
		return
	}
	// Captured inside the mutation, before the op below changes anything, so
	// it's an exact "before" snapshot for reconcileMeshRedistribute regardless
	// of which op ran (only add/delete/enable/disable actually move a CIDR in
	// or out of meshRouteCIDRs — reject/reject-enable/reject-disable never do,
	// but capturing it unconditionally is one extra map build, not worth
	// special-casing per op).
	var prevMesh []string
	err := s.mutateConfig(func(cfg *config.Config) error {
		prevMesh = meshRouteCIDRs(cfg)
		switch req.Op {
		case "add", "advertise", "redistribute":
			return cfg.RouteAdd(req.Net, req.CIDR, req.Metric)
		case "delete", "del", "remove":
			return cfg.RouteDelete(req.Net, req.CIDR)
		case "reject":
			return cfg.RouteReject(req.Net, req.CIDR, req.Inclusive)
		case "enable":
			return cfg.RouteSetEnabled(req.Net, req.CIDR, true)
		case "disable":
			return cfg.RouteSetEnabled(req.Net, req.CIDR, false)
		case "reject-enable":
			return cfg.RouteRejectSetEnabled(req.Net, req.CIDR, true)
		case "reject-disable":
			return cfg.RouteRejectSetEnabled(req.Net, req.CIDR, false)
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	if err == nil {
		// If this node redistributes the Mesh Routes page into BGP, keep FRR in
		// sync with whatever this edit just changed — not just at the next BGP
		// config save. See reconcileMeshRedistribute's doc comment.
		s.reconcileMeshRedistribute(prevMesh)
	}
	s.editResult(w, err, false)
}

// handleRouteAdv reports (GET) and sets (POST) the route re-advertisement
// interval in seconds. Applied live on save — no restart.
func (s *Server) handleRouteAdv(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"interval":  cfg.RouteAdvInterval,
			"effective": int(cfg.RouteAdvDuration().Seconds()),
		})
		return
	}
	var req struct {
		Interval int `json:"interval"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Interval < 0 || req.Interval > 86400 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "interval must be between 0 and 86400 seconds"})
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.RouteAdvInterval = req.Interval
		return nil
	})
	s.editResult(w, err, false)
}

// handleKeepalive is the NAT-keepalive cadence setting — same GET/POST shape
// as handleRouteAdv. "effective" also reports PeerTimeoutDuration alongside
// KeepaliveDuration so the UI can show the peer-timeout row's live effective
// value shift immediately when keepalive changes, without a second request —
// relevant since an explicit peer-timeout below the new keepalive interval
// gets silently clamped up to it (see PeerTimeoutDuration).
func (s *Server) handleKeepalive(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"interval":         cfg.KeepaliveInterval,
			"effective":        int(cfg.KeepaliveDuration().Seconds()),
			"peer_timeout_now": int(cfg.PeerTimeoutDuration().Seconds()),
		})
		return
	}
	var req struct {
		Interval int `json:"interval"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Interval < 0 || req.Interval > 86400 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "interval must be between 0 and 86400 seconds"})
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.KeepaliveInterval = req.Interval
		return nil
	})
	s.editResult(w, err, false)
}

// handlePeerTimeout is the dead-session timeout setting — same GET/POST
// shape as handleRouteAdv/handleKeepalive.
func (s *Server) handlePeerTimeout(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"interval":  cfg.PeerTimeout,
			"effective": int(cfg.PeerTimeoutDuration().Seconds()),
		})
		return
	}
	var req struct {
		Interval int `json:"interval"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Interval < 0 || req.Interval > 86400 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "interval must be between 0 and 86400 seconds"})
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.PeerTimeout = req.Interval
		return nil
	})
	s.editResult(w, err, false)
}

// exemptView is one always-allowed entry as shown in the UI. Mgmt entries report
// the resolved web-admin port number (not a placeholder) so the operator sees an
// actual port; the mgmt flag is still returned so an unedited entry keeps
// following the admin port when saved back.
type exemptView struct {
	Name     string `json:"name"`
	Proto    string `json:"proto"`
	Port     int    `json:"port"`
	Mgmt     bool   `json:"mgmt,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// serveDocFile returns a doc file (README/LICENSE) from disk as JSON, read fresh
// each request so an updated file shows without a daemon restart. A 1 MiB ceiling
// guards against an unexpectedly huge file.
func serveDocFile(w http.ResponseWriter, path string) {
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]any{"text": "", "path": "", "available": false})
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"text": "", "path": path, "available": false})
		return
	}
	const maxBytes = 1 << 20
	if len(b) > maxBytes {
		b = b[:maxBytes]
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": string(b), "path": path, "available": true})
}

// handleReadme returns the project README from disk (installed alongside the
// binary), read fresh each request so edits show without a restart.
func (s *Server) handleReadme(w http.ResponseWriter, r *http.Request) {
	serveDocFile(w, s.readmePath)
}

// handleLicense returns the project LICENSE from disk (installed alongside the
// binary), read fresh each request.
func (s *Server) handleLicense(w http.ResponseWriter, r *http.Request) {
	serveDocFile(w, s.licensePath)
}

// handleGettingStarted returns getting-started.md — the markdown source —
// as {text, path, available}, exactly the shape README/LICENSE use, so the
// Info → Getting Started page renders it the same way secReadme renders
// README: through mdRender, styled with the app's own CSS variables. (A
// separate getting-started.html once existed, shown in an iframe; removed
// once native styling was what was actually wanted, so there's one file to
// keep current, not two.)
func (s *Server) handleGettingStarted(w http.ResponseWriter, r *http.Request) {
	serveDocFile(w, s.gettingStartedPath)
}

// handleLogs returns the tail of the daemon log file as individual lines for the
// web Logs view. Reads only the last chunk of the file so a large log doesn't
// blow up memory, then returns at most the last `n` lines (default 1000).
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.logPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "path": "", "enabled": false})
		return
	}
	// Download mode: return the whole file (capped) as text for a client-side save.
	if r.URL.Query().Get("download") == "1" {
		const maxDownload = 64 << 20 // safety cap
		f, err := os.Open(s.logPath)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"text": "", "path": s.logPath, "enabled": true})
			return
		}
		defer f.Close()
		buf, _ := io.ReadAll(io.LimitReader(f, maxDownload))
		writeJSON(w, http.StatusOK, map[string]any{"text": string(buf), "path": s.logPath, "enabled": true})
		return
	}
	n := 1000
	if v := r.URL.Query().Get("n"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 && k <= 10000 {
			n = k
		}
	}
	f, err := os.Open(s.logPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "path": s.logPath, "enabled": true})
		return
	}
	defer f.Close()
	const maxRead = 1 << 20 // read at most the last 1 MiB
	var buf []byte
	if fi, err := f.Stat(); err == nil {
		size := fi.Size()
		start := int64(0)
		if size > maxRead {
			start = size - maxRead
		}
		if _, err := f.Seek(start, io.SeekStart); err == nil {
			buf, _ = io.ReadAll(f)
		}
	}
	var lines []string
	if len(buf) > 0 {
		lines = strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	}
	// Drop a partial first line when we seeked into the middle of the file.
	if len(buf) == maxRead && len(lines) > 1 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "path": s.logPath, "enabled": true})
}

// handleLogsClear empties the active log file. It only acts when file logging is
// enabled and a clear hook was wired (so the rotating writer's size counter is
// reset in step with the truncation).
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	if s.logPath == "" || s.logClear == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "file logging is disabled"})
		return
	}
	if err := s.logClear(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleExempt reports (GET) and replaces (POST) the node-global always-allowed
// list. Applied live on save — no restart.
func (s *Server) handleExempt(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		port := cfg.WebAdminPort()
		list, isDefault := cfg.FirewallExemptList()
		out := make([]exemptView, 0, len(list))
		for _, e := range list {
			v := exemptView{Name: e.Name, Proto: e.Proto, Port: e.Port, Mgmt: e.Mgmt, Disabled: e.Disabled}
			if v.Proto == "" {
				v.Proto = "any"
			}
			if e.Mgmt {
				v.Port = port // show the actual management port number
			}
			out = append(out, v)
		}
		writeJSON(w, http.StatusOK, map[string]any{"exempt": out, "default": isDefault, "mgmt_port": port})
		return
	}
	var req struct {
		Op     string       `json:"op"`
		Index  int          `json:"index"`
		Exempt []exemptView `json:"exempt"`
		Reset  bool         `json:"reset"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Reset {
		err := s.mutateConfig(func(cfg *config.Config) error { cfg.FirewallExemptReset(); return nil })
		s.editResult(w, err, false)
		return
	}
	// A per-entry toggle by index, mirroring the firewall per-rule enable/disable.
	if req.Op == "enable" || req.Op == "disable" {
		err := s.mutateConfig(func(cfg *config.Config) error {
			return cfg.FirewallExemptSetEnabled(req.Index, req.Op == "enable")
		})
		s.editResult(w, err, false)
		return
	}
	list := make([]config.FirewallExempt, 0, len(req.Exempt))
	for _, v := range req.Exempt {
		e := config.FirewallExempt{Name: v.Name, Proto: v.Proto, Mgmt: v.Mgmt, Disabled: v.Disabled}
		if !v.Mgmt {
			e.Port = v.Port // a fixed port; mgmt entries follow the admin port instead
		}
		list = append(list, e)
	}
	err := s.mutateConfig(func(cfg *config.Config) error { return cfg.FirewallExemptSet(list) })
	s.editResult(w, err, false)
}

// handleNAT: add / update / delete / enable / disable (whole NAT) and
// rule-enable / rule-disable (a single rule by index). Applied live by the
// reload.
// handlePort sets the underlay primary UDP port. Applied live by the reload:
// the daemon binds the new port, swaps it in for outbound, and connected peers
// migrate to it automatically (the old port keeps serving inbound briefly).
// handleNATState sets the global NAT connection state timeout (seconds). Applied
// live by the reload; 0 restores the default (120s).
func (s *Server) handleNATState(w http.ResponseWriter, r *http.Request) {
	var req struct{ Timeout int }
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		return cfg.NATStateTimeoutSet(req.Timeout)
	})
	s.editResult(w, err, false) // applied live; no restart
}

// handleGeoIPSetting toggles the peer/seed info panel's Geo-IP lookup (on by
// default — see config.WebAdmin.GeoIPLookup's doc comment). Unlike
// handleNATState/handlePort just below — mesh-level settings the running
// Engine picks up live via s.reload() — this is a webadmin.Server-scoped
// setting read from s.cfg, which (like AuthMode and the local admin user
// list) is captured once at startup and never refreshed by mutateConfig's
// reload, so the new value only takes effect after a restart.
func (s *Server) handleGeoIPSetting(w http.ResponseWriter, r *http.Request) {
	var req struct{ On bool }
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		on := req.On
		cfg.WebAdmin.GeoIPLookup = &on
		return nil
	})
	s.editResult(w, err, true) // needs a restart — see doc comment above
}

// handleUPnPSetting toggles gravinet's own best-effort UPnP IGD port-
// mapping helper (config.Config.EnableUPnP's doc comment has the full
// picture) — off by default. This needs a restart to take effect for a
// different reason than handleGeoIPSetting just above: it's not that the
// value is read from a startup-frozen copy, it's that the upnp.Manager
// mapping this node's listen ports is itself only ever built and started
// once, alongside those ports' own transports, at daemon startup (see
// cmd/gravinet/main.go) — there's no live "start/stop the manager, or
// change what it maps" hook wired into reloadFn.
func (s *Server) handleUPnPSetting(w http.ResponseWriter, r *http.Request) {
	var req struct{ On bool }
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.EnableUPnP = req.On
		return nil
	})
	s.editResult(w, err, true) // needs a restart — see doc comment above
}

// handlePort sets the UDP underlay port(s): the first in the list becomes
// the primary (used for outbound and advertised to peers), any rest become
// extra listen-only ports (config extra_listen_ports) — one field for both,
// not a separate one per concept, so "listen on 65432, 443, and 80" is just
// three comma-separated numbers here rather than a primary field plus a
// second list elsewhere. Applied live: reloadFn rebinds on a change to
// either the primary or the extra list (see prevExtraUDP in main.go).
// Each port is range-checked; a port that's actually unbindable (privileged,
// already in use) is instead caught and logged, not rejected, at bind time
// — see transport.Options.ExtraPorts.
//
// disabled:true (the web UI's "-" sentinel) turns UDP off entirely instead
// of setting a port list — sets primary_port to 0 and clears the extra list.
// Refused if the TCP/TLS fallback is also off, since the node would then
// have no way to be reached at all; Config.Validate enforces this as a
// backstop too. Applied live the same way (main.go's reload closes the live
// UDP socket — see the newCfg.PrimaryPort == 0 branch there).
func (s *Server) handlePort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ports    []int
		Disabled bool
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		if req.Disabled {
			if !cfg.TCPFallbackEnabled() {
				return fmt.Errorf("can't turn off the UDP port while the TCP fallback is also off — at least one must stay on")
			}
			cfg.PrimaryPort = 0
			cfg.ExtraListenPorts = nil
			return nil
		}
		if len(req.Ports) == 0 {
			return fmt.Errorf("at least one port is required (or \"-\" to turn UDP off)")
		}
		for _, p := range req.Ports {
			if p < 1 || p > 65535 {
				return fmt.Errorf("port %d must be between 1 and 65535", p)
			}
		}
		cfg.PrimaryPort = req.Ports[0]
		cfg.ExtraListenPorts = req.Ports[1:]
		return nil
	})
	s.editResult(w, err, false) // applied live; no restart
}

// handleTCPPort is handlePort's TCP/TLS-fallback counterpart: the first port
// in the list is the fallback listener itself (config tcp_fallback_port),
// any rest are extra TCP/TLS ports (extra_tcp_listen_ports). Applied live
// the same way — the daemon rebinds the fallback listener (and any extra
// ones) to the new set; the old ones keep serving briefly.
//
// disabled:true (the web UI's "-" sentinel) sets disable_tcp_fallback
// instead of a port list — the TCPFallbackPort/ExtraTCPListenPorts values
// are left as-is (not cleared) so they're remembered if re-enabled later.
// Refused if the UDP port is also off, for the same reason handlePort
// refuses the opposite; Config.Validate enforces this as a backstop too.
func (s *Server) handleTCPPort(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ports    []int
		Disabled bool
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		if req.Disabled {
			if cfg.PrimaryPort == 0 {
				return fmt.Errorf("can't turn off the TCP fallback while the UDP port is also off — at least one must stay on")
			}
			cfg.DisableTCPFallback = true
			return nil
		}
		if len(req.Ports) == 0 {
			return fmt.Errorf("at least one port is required (or \"-\" to turn the TCP fallback off)")
		}
		for _, p := range req.Ports {
			if p < 1 || p > 65535 {
				return fmt.Errorf("port %d must be between 1 and 65535", p)
			}
		}
		cfg.DisableTCPFallback = false
		cfg.TCPFallbackPort = req.Ports[0]
		cfg.ExtraTCPListenPorts = req.Ports[1:]
		return nil
	})
	s.editResult(w, err, false) // applied live; no restart
}

func (s *Server) handleNAT(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op, Net                        string
		Iface, Source, Dest, Translate string
		Index, Timeout                 int
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		switch req.Op {
		case "add":
			// Full rule when any rule field is set; otherwise the masquerade
			// shorthand (interface only).
			if req.Source != "" || req.Dest != "" || req.Translate != "" {
				return cfg.NATRuleAdd(req.Net, req.Source, req.Dest, req.Translate, req.Iface)
			}
			return cfg.NATAdd(req.Net, req.Iface)
		case "update":
			return cfg.NATRuleUpdateAt(req.Net, req.Index, req.Source, req.Dest, req.Translate, req.Iface)
		case "delete", "del", "remove":
			if req.Iface != "" {
				return cfg.NATDelete(req.Net, req.Iface)
			}
			return cfg.NATRuleDeleteAt(req.Net, req.Index)
		case "enable":
			return cfg.NATSetEnabled(req.Net, true)
		case "disable":
			return cfg.NATSetEnabled(req.Net, false)
		case "rule-enable":
			return cfg.NATRuleSetEnabled(req.Net, req.Index, true)
		case "rule-disable":
			return cfg.NATRuleSetEnabled(req.Net, req.Index, false)
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	s.editResult(w, err, false)
}

// handleQoS: add / delete / enable / disable (whole classifier),
// rule-enable / rule-disable (a single rule by proto/port/services), and
// mark / unmark (a class's outbound DSCP override). Applied live by the
// reload. Services names entries in the node-global firewall service
// catalog (Config.FirewallServices) — same catalog, same union-with-proto/
// port convention as FirewallRule.Services.
func (s *Server) handleQoS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op, Net, Proto string
		Port, Class    int
		Services       []string
		DSCP           int
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		switch req.Op {
		case "add":
			return cfg.QoSAdd(req.Net, strings.ToLower(req.Proto), req.Port, req.Services, req.Class)
		case "delete", "del", "remove":
			return cfg.QoSDelete(req.Net, strings.ToLower(req.Proto), req.Port, req.Services)
		case "enable":
			return cfg.QoSSetEnabled(req.Net, true)
		case "disable":
			return cfg.QoSSetEnabled(req.Net, false)
		case "rule-enable":
			return cfg.QoSRuleSetEnabled(req.Net, strings.ToLower(req.Proto), req.Port, req.Services, true)
		case "rule-disable":
			return cfg.QoSRuleSetEnabled(req.Net, strings.ToLower(req.Proto), req.Port, req.Services, false)
		case "mark":
			return cfg.QoSSetClassDSCP(req.Net, req.Class, req.DSCP)
		case "unmark":
			return cfg.QoSClearClassDSCP(req.Net, req.Class)
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	s.editResult(w, err, false)
}

// handleBandwidth: set up/down/both throttle (bytes/sec). Applied live.
func (s *Server) handleBandwidth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op, Net, Dir string
		Bps          int
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		switch req.Op {
		case "enable":
			return cfg.ThrottleSetEnabled(req.Net, true)
		case "disable":
			return cfg.ThrottleSetEnabled(req.Net, false)
		default:
			return cfg.ThrottleSet(req.Net, req.Dir, req.Bps)
		}
	})
	s.editResult(w, err, false)
}

// handleRestart restarts the service so structural changes take effect. It
// replies first, then restarts shortly after (this process is terminated).
// Delegates to service.CanRestart/service.Restart — the same primitives the
// CLI's own restart path uses — instead of a webadmin-local reimplementation,
// so every platform they support (Linux via systemctl, macOS via launchctl
// kickstart, Windows via Restart-Service, FreeBSD via service(8)) gets a
// working restart here, not just Linux with everything else told to restart
// manually.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if ok, hint := service.CanRestart(); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": hint})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restarting": true})
	go func() {
		time.Sleep(700 * time.Millisecond) // let the reply flush first
		s.log.Infof("webadmin: restarting service (requested from admin UI)")
		if ok, hint := service.Restart(); !ok {
			// The reply already promised a restart; this process is very
			// likely gone momentarily regardless (systemctl/launchctl often
			// send the stop signal as part of the same request even when the
			// command that issued it reports an error), so there's no
			// response left to correct — just make the failure visible in
			// the log for whoever's looking.
			s.log.Warnf("webadmin: automatic restart failed: %s", hint)
		}
	}()
}

// handleSystemPower reboots or shuts down the *host* (not just the gravinet
// service — that's handleRestart), optionally on a delay, via the OS's own
// shutdown facility. Backend lives in the service package (HostPower /
// HostPowerCancel / HostPowerSupported), cross-platform.
//
// Request: {action, when, minutes, time}.
//
//	action: "restart" | "shutdown" | "cancel"
//	when:   "now" | "in" | "at"     (ignored for "cancel")
//	minutes: N>0                    (required for when=="in")
//	time:   "HH:MM" 24h             (required for when=="at")
//
// "when" is resolved to whole minutes-from-now here — "now"→0, "in"→minutes,
// "at HH:MM"→minutes until the next occurrence of that clock time in the host's
// local zone — so the platform layer only ever deals in a minute count and
// never has to reason about clock formats that differ per OS. The reply echoes
// a human-readable "when" so the UI can confirm what it scheduled.
func (s *Server) handleSystemPower(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action  string `json:"action"`
		When    string `json:"when"`
		Minutes int    `json:"minutes"`
		Time    string `json:"time"`
	}
	if !decode(w, r, &req) {
		return
	}

	if req.Action == "cancel" {
		s.log.Infof("webadmin: cancelling pending host power action (requested from admin UI)")
		if ok, hint := service.HostPowerCancel(); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": hint})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "cancel"})
		return
	}

	if req.Action != "restart" && req.Action != "shutdown" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action must be 'restart', 'shutdown', or 'cancel'"})
		return
	}
	if ok, hint := service.HostPowerSupported(); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": hint})
		return
	}

	// Resolve when → whole minutes from now, plus a human string for the reply.
	delayMin := 0
	human := "now"
	switch req.When {
	case "", "now":
		// immediate
	case "in":
		if req.Minutes < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "'in' needs a positive number of minutes"})
			return
		}
		if req.Minutes > 60*24*7 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "minutes must be at most 10080 (7 days)"})
			return
		}
		delayMin = req.Minutes
		human = fmt.Sprintf("in %d minute(s)", req.Minutes)
	case "at":
		mins, err := minutesUntilClock(req.Time)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		delayMin = mins
		human = "at " + req.Time
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "'when' must be 'now', 'in', or 'at'"})
		return
	}

	s.log.Infof("webadmin: host %s scheduled %s (requested from admin UI)", req.Action, human)
	if ok, hint := service.HostPower(req.Action, delayMin); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": hint})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": req.Action, "when": human})
}

// minutesUntilClock returns whole minutes from now until the next occurrence of
// a 24-hour "HH:MM" wall-clock time in the host's local zone. A time earlier
// today (or exactly now) rolls to tomorrow, so "at 02:00" at 09:00 means 17h
// out, never a negative delay. Rounds up so the resulting minute never lands
// before the requested clock time. Returns an error for a malformed time.
func minutesUntilClock(hhmm string) (int, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("'at' needs a time in 24-hour HH:MM format")
	}
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	mins := int((target.Sub(now) + time.Minute - time.Nanosecond) / time.Minute) // ceil
	if mins < 1 {
		mins = 1
	}
	return mins, nil
}

// handleKey: manage a network's join/rotation key slots. Key changes apply on
// restart (keys bind into the engine at startup), so these return restart:true.
// "reveal" is read-only and returns the full key for distribution.
func (s *Server) handleKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op, Net, Key, Label, Expires, Notes string
		Slot                                int
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Op == "reveal" {
		if s.configPath == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "config path not set"})
			return
		}
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		key, err := cfg.KeyReveal(req.Net, req.Slot)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
		return
	}
	// "distribute" pushes an already-configured, enabled key out to every peer
	// currently connected on the network (over the encrypted mesh itself — see
	// Engine.FloodKey), passing this node's own slot along as a hint so a peer
	// with that exact slot free uses it too. It marks the slot Distributed so
	// the checkbox stays ticked and reflects reality across a page reload, and
	// is safe to click again later, e.g. once more peers have joined.
	if req.Op == "distribute" {
		if s.configPath == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "config path not set"})
			return
		}
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		n, err := cfg.PickNetwork(req.Net)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		key, err := cfg.KeyReveal(req.Net, req.Slot)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		slot := n.Keys[req.Slot]
		if !slot.Enabled {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "enable this key before distributing it"})
			return
		}
		netID, err := strconv.ParseUint(n.ID, 16, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad network id"})
			return
		}
		var expNano int64
		if slot.Expires != "" {
			if t, e := time.Parse(time.RFC3339, slot.Expires); e == nil {
				expNano = t.UnixNano()
			}
		}
		if err := s.be.FloodKey(netID, key, slot.Label, expNano, req.Slot); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.mutateConfig(func(cfg *config.Config) error {
			return cfg.KeySetDistributed(req.Net, req.Slot, true)
		}); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peers": len(s.be.ListPeers(netID))})
		return
	}
	// "undistribute" — the "Distributed" checkbox being unticked — detaches
	// this key from mesh-wide management: it stops being something this node
	// pushes label/expiry changes for (see the "label"/"expiry" ops' flood
	// logic below, both gated on slot.Distributed), and becomes an ordinary,
	// independent key on every node that already has a copy. It deliberately
	// does NOT retract the key (see RetractKey, used instead by "delete" —
	// unticking is "stop managing this together," not "revoke it"; a peer
	// keeps trusting its copy exactly as it was at the moment of unticking,
	// and each node's copy can now drift independently (relabeled, given a
	// different expiry, etc.) without any of that propagating anywhere.
	if req.Op == "undistribute" {
		if err := s.mutateConfig(func(cfg *config.Config) error {
			return cfg.KeySetDistributed(req.Net, req.Slot, false)
		}); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var genSlot int
	var genKey string
	var floodDistributed bool
	var floodKeyB64, floodLabel, floodExpires string
	var floodNetID uint64
	var retractDistributed bool
	var retractKeyB64 string
	var retractNetID uint64
	err := s.mutateConfig(func(cfg *config.Config) error {
		switch req.Op {
		case "generate":
			key, e := cfg.KeyGenerateInto(req.Net, req.Slot, req.Label)
			genSlot, genKey = req.Slot, key
			return e
		case "set", "import":
			return cfg.KeySet(req.Net, req.Slot, req.Key, req.Label)
		case "label":
			// A relabel of an already-distributed key is pushed out to every
			// peer holding a copy, over the same flood that distributed it in
			// the first place — see FloodKey/addPropagatedKey's relabel path.
			if err := cfg.KeySetLabel(req.Net, req.Slot, req.Label); err != nil {
				return err
			}
			n, err := cfg.PickNetwork(req.Net)
			if err != nil {
				return err
			}
			slot := n.Keys[req.Slot]
			if slot.Distributed {
				floodDistributed = true
				floodKeyB64 = slot.Key
				floodLabel = slot.Label
				floodExpires = slot.Expires
				if id, e := strconv.ParseUint(n.ID, 16, 64); e == nil {
					floodNetID = id
				}
			}
			return nil
		case "expiry", "expires":
			// Same reasoning as "label" just above: a distributed key's expiry
			// is part of the same encodeKeyAdd payload FloodKey sends (see
			// keyrotate.go), so a peer already holding this key only learns
			// about a new expiry if it's re-flooded — otherwise every other
			// node keeps trusting the key on whatever expiry (or lack of one)
			// it had at distribution time, silently outliving what the
			// operator just set here on the origin node.
			if err := cfg.KeySetExpiry(req.Net, req.Slot, req.Expires); err != nil {
				return err
			}
			n, err := cfg.PickNetwork(req.Net)
			if err != nil {
				return err
			}
			slot := n.Keys[req.Slot]
			if slot.Distributed {
				floodDistributed = true
				floodKeyB64 = slot.Key
				floodLabel = slot.Label
				floodExpires = slot.Expires
				if id, e := strconv.ParseUint(n.ID, 16, 64); e == nil {
					floodNetID = id
				}
			}
			return nil
		case "notes":
			// Unlike label/expiry, notes are never part of the distributed-key
			// flood payload — they stay local to this node even for a
			// Distributed slot, so no flood bookkeeping is needed here.
			return cfg.KeySetNotes(req.Net, req.Slot, req.Notes)
		case "enable":
			return cfg.KeySetEnabled(req.Net, req.Slot, true)
		case "disable":
			return cfg.KeySetEnabled(req.Net, req.Slot, false)
		case "delete", "del", "remove":
			// Deleting a distributed key is the one operation that *does*
			// retract it from every peer holding a copy — unlike unticking
			// "Distributed" (see the "undistribute" op above), which
			// deliberately leaves peers' copies alone. Deleting means "this
			// key should no longer exist, anywhere," so capture what's needed
			// to retract it before it's gone from config, and let the caller
			// flood the retraction once the delete itself has succeeded.
			n, err := cfg.PickNetwork(req.Net)
			if err != nil {
				return err
			}
			if req.Slot >= 0 && req.Slot < len(n.Keys) {
				slot := n.Keys[req.Slot]
				if slot.Distributed && slot.Key != "" {
					retractDistributed = true
					retractKeyB64 = slot.Key
					if id, e := strconv.ParseUint(n.ID, 16, 64); e == nil {
						retractNetID = id
					}
				}
			}
			return cfg.KeyDelete(req.Net, req.Slot)
		default:
			return fmt.Errorf("unknown op %q", req.Op)
		}
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if floodDistributed {
		var expNano int64
		if floodExpires != "" {
			if t, e := time.Parse(time.RFC3339, floodExpires); e == nil {
				expNano = t.UnixNano()
			}
		}
		if err := s.be.FloodKey(floodNetID, floodKeyB64, floodLabel, expNano, req.Slot); err != nil {
			s.log.Warnf("webadmin: could not propagate %s change for net %016x slot %d: %v", req.Op, floodNetID, req.Slot, err)
		}
	}
	if retractDistributed {
		if err := s.be.RetractKey(retractNetID, retractKeyB64); err != nil {
			s.log.Warnf("webadmin: could not retract deleted key for net %016x slot %d: %v", retractNetID, req.Slot, err)
		}
	}
	// Key changes (generate/import/enable/disable/delete/label) apply live via
	// the reload hook — no restart needed.
	resp := map[string]any{"ok": true, "restart": false}
	if req.Op == "generate" {
		resp["slot"] = genSlot
		resp["key"] = genKey
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleInterfaces lists the host's non-loopback network interface names, for the
// NAT masquerade dropdown (so operators pick an interface instead of typing one).
func (s *Server) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	ifaces, err := net.Interfaces()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"interfaces": []string{}})
		return
	}
	names := make([]string, 0, len(ifaces))
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		names = append(names, ifi.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"interfaces": names})
}
