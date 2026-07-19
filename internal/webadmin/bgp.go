package webadmin

// BGP peer status, read live from FRR via vtysh. This mirrors how parapet
// surfaces dynamic-routing adjacencies: FRR owns the BGP session, and the
// admin UI is a read-only window onto it, asking vtysh for a JSON summary and
// reshaping it into a flat table. gravinet does not itself speak BGP or manage
// FRR's config — it just reports what FRR already has, and only when FRR's CLI
// (vtysh) is actually installed on this host.
//
// The whole feature is gated on that vtysh presence check: bgpSupported() is
// surfaced in /api/config, and the web UI shows the Traffic > BGP section only
// when it returns true (see ui.go). On a host without FRR — every Windows box,
// and any Unix host that never installed FRR — none of this is reachable and
// the menu entry is hidden, rather than offering a section that could only
// ever say "not installed".

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// bgpVtyshPaths are the locations vtysh is installed to, in priority order.
// The first three are ported verbatim from parapet's FRR integration
// (frr.rs / status.rs), which probes exactly those — Linux FHS conventions.
// The last two are FreeBSD's: its frr port installs under /usr/local (pkg's
// convention for anything outside the base system), specifically to
// /usr/local/bin/vtysh — confirmed against the actual rc.d script FreeBSD
// ships (net/frrN/files/frr.in in the freebsd-ports tree), which invokes
// %%PREFIX%%/bin/vtysh; /usr/local/sbin is checked too as a harmless extra
// in case a given port revision ever places it there instead. A path check
// (rather than a $PATH lookup) keeps detection cheap and side-effect-free:
// no subprocess is spawned just to learn whether the binary exists, and
// checking two extra candidate paths that will never match on a non-FreeBSD
// host costs one failed stat(2) each — negligible, and simpler than
// build-tagging this list apart per OS.
var bgpVtyshPaths = []string{
	"/usr/bin/vtysh", "/usr/sbin/vtysh", "/bin/vtysh", // Linux
	"/usr/local/bin/vtysh", "/usr/local/sbin/vtysh", // FreeBSD
}

// vtyshPath returns the first vtysh binary that exists on this host, and
// whether one was found at all. On Windows (and any Unix host without FRR)
// none of the paths exist, so ok is false and BGP support stays off.
func vtyshPath() (string, bool) {
	for _, p := range bgpVtyshPaths {
		if fi, err := statFile(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// bgpSupported reports whether this host can serve BGP status — i.e. whether
// FRR's vtysh is installed. It's the single source of truth the /api/config
// capability flag and every BGP code path below share, so the menu the user
// sees and the endpoint that backs it can never disagree.
func bgpSupported() bool {
	_, ok := vtyshPath()
	return ok
}

// bgpQueryTimeout bounds a single vtysh call. Right after boot, before FRR's
// daemon sockets are fully up, vtysh can hang; parapet caps the equivalent
// call at 8s via the `timeout` binary. We use a context deadline instead —
// the idiomatic, portable Go equivalent (no dependency on a `timeout` binary,
// which isn't present everywhere) already used elsewhere in this package.
const bgpQueryTimeout = 8 * time.Second

// handleBGP returns the current BGP peer table as reported by FRR. Response
// shape mirrors parapet's neighbors endpoint: {available, reason?, peers[]}.
// When vtysh is absent, or FRR isn't answering, available is false with a
// human reason and an empty peer list, so the UI degrades to an explanatory
// line rather than an error.
func (s *Server) handleBGP(w http.ResponseWriter, r *http.Request) {
	if !bgpSupported() {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    "FRR/vtysh is not installed",
			"peers":     []any{},
		})
		return
	}
	// FRR emits JSON when the command ends in "json". Deliberately "show bgp
	// summary json", not "show ip bgp summary json": the "ip" keyword
	// restricts FRR to the IPv4-unicast address family only, so an IPv6
	// session would never appear in the output no matter how parseBGPSummary
	// below walks it — this is what silently dropped IPv6 neighbors from the
	// table before. Dropping "ip" gets every configured AFI (ipv4Unicast and
	// ipv6Unicast) in one call, matching what parseBGPSummary already expects.
	// runVtysh bounds this hard, so a wedged FRR socket can never hang the
	// request.
	out, ran := runVtysh("show bgp summary json")
	if !ran {
		// vtysh exists but couldn't answer in time: FRR/bgpd isn't running, or
		// the call timed out (e.g. sockets not up yet just after boot).
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    "FRR is not running (no routing daemons active)",
			"peers":     []any{},
		})
		return
	}

	peers, routerID, localAS := parseBGPSummary(out)
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"router_id": routerID,
		"local_as":  localAS,
		"peers":     peers,
	})
}

// handleBFD returns the current BFD session table as reported by FRR's
// bfdd, for the Monitor > BGP Peers page's separate "BFD Neighbors" card.
// vtysh is the shared CLI front end for every FRR daemon including bfdd, so
// this is gated on the same bgpSupported() check as handleBGP — a BFD
// session can back a BGP neighbor, an OSPF adjacency, or a monitored static
// route, so it isn't itself gated on BGP being enabled or configured, only
// on FRR/vtysh being present at all. Same degrade-with-a-reason shape as
// handleBGP: available=false with a human reason and an empty list rather
// than an error, when vtysh is absent or bfdd isn't answering.
func (s *Server) handleBFD(w http.ResponseWriter, r *http.Request) {
	if !bgpSupported() {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    "FRR/vtysh is not installed",
			"peers":     []any{},
		})
		return
	}
	out, ran := runVtysh("show bfd peers json")
	if !ran {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    "FRR is not running (no routing daemons active)",
			"peers":     []any{},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"peers":     parseBFDPeers(out),
	})
}

// handleBGPTable returns the raw text of FRR's `show bgp` command — the full
// BGP table (prefixes, next hops, AS paths, and per-route status codes), as
// opposed to handleBGP's per-peer summary. It backs the Monitor > BGP Peers
// "BGP Table" card. Unlike handleBGP/handleBFD this isn't reshaped into a
// struct: `show bgp` has no JSON form, and its fixed-width columns are more
// legible left exactly as FRR renders them than reparsed into a table, so the
// response is the command's own text verbatim. Same availability shape as
// every other BGP/BFD endpoint (gated on bgpSupported, degrading to
// available=false with a human reason rather than an error) so the card
// behaves identically to its neighbors when vtysh is absent or FRR isn't
// answering.
func (s *Server) handleBGPTable(w http.ResponseWriter, r *http.Request) {
	if !bgpSupported() {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    "FRR/vtysh is not installed",
			"text":      "",
		})
		return
	}
	out, ran := runVtysh("show bgp")
	if !ran {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    "FRR is not running (no routing daemons active)",
			"text":      "",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"text":      string(out),
	})
}

// bgpLearnedRoutes queries FRR for the CIDRs currently installed as this
// node's best BGP path, across both address families — what
// bgpMeshRedistributor actually gossips into the mesh when
// config.Network.RedistributeBGP is on. "show bgp ipv4/ipv6 unicast json"
// (not "show bgp summary json" — that's peers, not routes; not plain "show
// bgp ipv4 unicast" — no JSON form) returns an object keyed by prefix, each
// holding every path FRR knows for it — see parseBGPLearnedRoutes, which
// does the actual selection, for what "best path" means here.
//
// Thin wrapper around runVtysh + parseBGPLearnedRoutes, split apart the same
// way handleBGP/parseBGPSummary already are: this is the only half that
// touches a live FRR (untestable without one installed), the parser is a
// pure function this package's tests exercise directly against captured
// JSON.
func bgpLearnedRoutes() []netip.Prefix {
	seen := make(map[netip.Prefix]bool)
	var out []netip.Prefix
	for _, cmd := range []string{"show bgp ipv4 unicast json", "show bgp ipv6 unicast json"} {
		raw, ran := runVtysh(cmd)
		if !ran {
			continue
		}
		for _, pfx := range parseBGPLearnedRoutes(raw) {
			if !seen[pfx] {
				seen[pfx] = true
				out = append(out, pfx)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// parseBGPLearnedRoutes reshapes one AFI's "show bgp ipv4/ipv6 unicast json"
// into the CIDRs FRR currently has installed — the prefixes with a path that
// is both "valid" and "bestpath": the one FRR actually selected and uses, as
// opposed to every alternate path it's merely holding onto for the prefix,
// which isn't in use and would just be noise to redistribute (and, the
// moment the selected path changes, briefly wrong). The map key is the CIDR
// string itself, so unlike bgpPeerRow this never needs to reconstruct a
// prefix from separate fields — the one part of this shape solid enough to
// lean on without a live FRR install in this repo's test environment to
// verify every other field name against.
//
// Silent best-effort, matching parseBGPSummary/parseBFDPeers: invalid JSON,
// or a prefix string FRR emits that netip can't parse, is just skipped
// rather than erroring the whole call — bgpMeshRedistributor treats "FRR had
// nothing usable to say this tick" as "redistribute nothing this tick", not
// a reason to alarm.
func parseBGPLearnedRoutes(raw []byte) []netip.Prefix {
	var top struct {
		Routes map[string][]struct {
			Valid    bool `json:"valid"`
			Bestpath bool `json:"bestpath"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	var out []netip.Prefix
	for cidr, paths := range top.Routes {
		pfx, err := netip.ParsePrefix(cidr)
		if err != nil {
			continue
		}
		for _, p := range paths {
			if p.Valid && p.Bestpath {
				out = append(out, pfx)
				break
			}
		}
	}
	return out
}

// handleBGPConfig is the read/write side: gravinet owns the BGP/BFD
// configuration and drives the FRR daemon from it. GET returns the stored BGP
// config plus whether FRR is installed. It deliberately does NOT touch vtysh —
// so the editor always loads instantly and can never hang on a slow or wedged
// FRR. Reflecting a pre-existing FRR config (when gravinet isn't managing BGP
// yet) is done separately by handleBGPImport, which the UI calls only after the
// editor is already on screen. PUT/POST persists a new BGP config to gravinet's
// own config.json and then reconciles FRR with it — rendering frr.conf, syncing
// the daemon set, and reloading FRR (see frr.go). The config is saved even when
// FRR isn't installed; it just isn't pushed to a daemon that isn't there.
func (s *Server) handleBGPConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var bgp config.BGPConfig
		var meshRoutes []string
		if s.configPath != "" {
			if cfg, err := config.Load(s.configPath); err == nil {
				bgp = cfg.BGP
				meshRoutes = meshRouteCIDRs(cfg)
			}
		}
		if bgp.Neighbors == nil {
			bgp.Neighbors = []config.BGPNeighbor{}
		}
		if bgp.Networks == nil {
			bgp.Networks = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"bgp":       bgp,
			"installed": frrInstalled(),
			"supported": bgpSupported(),
			// Whether gravinet is actively managing BGP. When false and FRR is
			// installed, the UI follows up with /api/bgp/import to reflect the
			// live config.
			"active": bgp.Enabled && bgp.ASN != 0,
			// mesh_routes is what the "Redistribute mesh routes" toggle would
			// carry into BGP right now (the Mesh Routes page's enabled Advertise
			// entries, on enabled networks) — shown next to the toggle whether or
			// not it's currently on, so the operator can see what turning it on
			// would mean.
			"mesh_routes": meshRoutes,
		})
		return
	}

	var req config.BGPConfig
	if !decode(w, r, &req) {
		return
	}
	// Persist first (mutateConfig validates + saves + fires the daemon's reload
	// hook), so the durable record is updated before we touch FRR. Captured
	// inside the mutation so it's exactly what was persisted before this save
	// overwrote it — applyBGP diffs against it to tell a removal (which needs a
	// real restart) from a pure addition/edit (safe to just reload). meshRoutes
	// is read from the same loaded snapshot; this save doesn't touch the Mesh
	// Routes page, so the same list serves as both applyBGP's "before" and
	// "after" — only RedistributeMesh itself turning on/off can change what's
	// redistributed here, and meshRedistributeRemovesSomething already handles
	// that from prev/next BGPConfig alone.
	var prev config.BGPConfig
	var meshRoutes []string
	if err := s.mutateConfig(func(cfg *config.Config) error {
		prev = cfg.BGP
		meshRoutes = meshRouteCIDRs(cfg)
		cfg.BGP = req
		return nil
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// Then reconcile FRR with the freshly-saved config. A no-op (with a note)
	// when FRR isn't installed — never fatal to the save.
	note, err := applyBGP(req, prev, meshRoutes, meshRoutes, s.log)
	// BGP itself turning on/off (or its ASN changing) changes whether
	// bgpMeshRedistributor's bgpUp check passes, independent of anything on
	// the Mesh Routes page — worth an immediate nudge rather than waiting up
	// to bgpRedistributePollInterval. Backgrounded: sync() can make its own
	// vtysh calls (bgpLearnedRoutes), each bounded by bgpQueryTimeout, and
	// this response shouldn't wait on that.
	if s.bgpRedis != nil {
		go s.bgpRedis.sync()
	}
	// Same reasoning: AutoBGP turning on/off, or this save changing something
	// it would otherwise derive itself (ASN, router-id), is worth reconciling
	// immediately rather than waiting up to autoBGPPollInterval. A no-op call
	// when AutoBGP is off (sync()'s own first check).
	if s.autoBGP != nil {
		go s.autoBGP.sync()
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": false, "note": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": frrInstalled(), "note": note})
}

// meshRouteCIDRs collects the CIDRs currently on the Mesh Routes page (Traffic
// > Mesh Routes' "Advertise" table) across every enabled network: each
// enabled Route's CIDR, from every network that's itself enabled. A disabled
// network isn't actually carrying its routes across the mesh, and a disabled
// route isn't being advertised either, so both are excluded — this must match
// what the mesh engine itself is doing (config.Network.Enabled /
// config.Route.Enabled), not just what the page happens to display. Reject
// entries (RouteRej) never appear here: they filter what's accepted *from*
// other nodes, nothing to do with what this node redistributes into BGP.
//
// Deduplicated (the same CIDR can legitimately be advertised on more than one
// network) and sorted, so renderFRR's `network` statements — and therefore
// frr.conf itself — come out in a stable order from one call to the next
// rather than reshuffling because config.Network iteration order changed.
func meshRouteCIDRs(cfg *config.Config) []string {
	seen := make(map[string]bool)
	var out []string
	for _, n := range cfg.Networks {
		if !n.Enabled {
			continue
		}
		for _, r := range n.Routes {
			if !r.Enabled || r.CIDR == "" || seen[r.CIDR] {
				continue
			}
			seen[r.CIDR] = true
			out = append(out, r.CIDR)
		}
	}
	sort.Strings(out)
	return out
}

// reconcileMeshRedistribute re-renders and reapplies frr.conf's mesh-derived
// `network` statements after something changed that can affect what
// meshRouteCIDRs returns — a route added/removed/enabled/disabled, or a
// network enabled/disabled/deleted — so a route that just disappeared from
// the Mesh Routes page stops being redistributed promptly, rather than only
// the next time the BGP config itself is saved. prevRoutes is the
// meshRouteCIDRs snapshot from immediately before the edit (the caller
// captures it inside the same mutateConfig closure that made the change, so
// the diff applyBGP/meshRedistributeRemovesSomething sees is exact).
//
// A no-op — one extra config.Load, nothing more — unless BGP is enabled with
// RedistributeMesh on, which keeps every ordinary route edit on a node that
// doesn't use this feature cheap. Errors are logged, not returned: this runs
// after the HTTP response for the route edit itself has already been decided
// (that edit succeeded regardless of whether FRR could be reached), the same
// "never let a routing reconcile block or fail an unrelated save" shape
// mutateConfig's own reload hook already uses.
func (s *Server) reconcileMeshRedistribute(prevRoutes []string) {
	if s.configPath == "" {
		return
	}
	cfg, err := config.Load(s.configPath)
	if err != nil || !cfg.BGP.Enabled || cfg.BGP.ASN == 0 || !cfg.BGP.RedistributeMesh {
		return
	}
	nextRoutes := meshRouteCIDRs(cfg)
	if _, err := applyBGP(cfg.BGP, cfg.BGP, nextRoutes, prevRoutes, s.log); err != nil {
		s.log.Warnf("bgp: reconciling mesh-route redistribution: %v", err)
	}
}

// bgpImportTimeout bounds handleBGPImport's total work end to end. Each
// individual runVtysh call inside importBGPFromFRR already bounds itself
// (bgpQueryTimeout + 2s), and the worst case is two sequential calls (the
// running-config fallback, then the summary enrichment) plus a fast local
// file read — call it ~20s. This wraps the whole thing with margin on top of
// that, as a second, independent backstop: even if some future change adds a
// third call, or vtysh behaves in a way an individual call's own timeout
// doesn't fully contain, the HTTP response is still guaranteed within this
// bound rather than trusting every path inside importBGPFromFRR to compose
// correctly. Mirrors the same "abandon a wedged call rather than block the
// caller" shape runVtysh uses for the same reason. A var (not const) so a
// test can shrink it rather than actually waiting out a real deadline.
var bgpImportTimeout = 25 * time.Second

// boundedBGPImport races fn (importBGPFromFRR in production) against timeout,
// returning whichever finishes first. Pulled out of handleBGPImport as its
// own function so the timeout/abandon mechanism can be exercised directly in
// a test with a deliberately slow fn, without needing a real, wedged FRR
// install to prove it actually bounds the wait.
func boundedBGPImport(timeout time.Duration, log *logx.Logger, fn func(*logx.Logger) (config.BGPConfig, bool, bool, string)) (bgp config.BGPConfig, hasPw, ok bool, reason string) {
	type result struct {
		bgp    config.BGPConfig
		hasPw  bool
		ok     bool
		reason string
	}
	ch := make(chan result, 1) // buffered so the goroutine never blocks on send
	go func() {
		b, pw, k, rsn := fn(log)
		ch <- result{b, pw, k, rsn}
	}()
	select {
	case res := <-ch:
		return res.bgp, res.hasPw, res.ok, res.reason
	case <-time.After(timeout):
		// Whatever's inside is taking longer than every individual step should
		// allow for — abandon it (the goroutine is left to finish or leak, same
		// tradeoff runVtysh itself makes) rather than hold the HTTP response
		// open indefinitely.
		if log != nil {
			log.Infof("bgp import: timed out after %s waiting for FRR", timeout)
		}
		return config.BGPConfig{}, false, false, fmt.Sprintf("timed out after %s waiting for FRR", timeout)
	}
}

// handleBGPImport reads the BGP configuration FRR is currently running and
// returns it, so the editor can reflect a setup configured outside gravinet
// (the fix for "the page shows zero config but there are live peers"). This is
// its own endpoint, separate from the config GET, precisely so the editor is
// never blocked on it: the UI renders the stored config immediately, then calls
// this in the background and swaps in the imported values if any come back.
// runVtysh (via importBGPFromFRR) bounds the vtysh call hard, so even a wedged
// FRR can only make this return "nothing to import," never hang. Read-only:
// importing never writes anything; the operator adopts by saving.
func (s *Server) handleBGPImport(w http.ResponseWriter, r *http.Request) {
	bgp, hasPw, ok, reason := boundedBGPImport(bgpImportTimeout, s.log, importBGPFromFRR)
	if !ok {
		// Report why nothing came back (and whether FRR is even here), so the UI
		// can explain an empty editor instead of leaving it a silent mystery.
		writeJSON(w, http.StatusOK, map[string]any{
			"imported":  false,
			"reason":    reason,
			"installed": frrInstalled(),
		})
		return
	}
	if bgp.Neighbors == nil {
		bgp.Neighbors = []config.BGPNeighbor{}
	}
	if bgp.Networks == nil {
		bgp.Networks = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"imported":               true,
		"imported_has_passwords": hasPw,
		"reason":                 reason,
		"bgp":                    bgp,
	})
}

// derivation match parapet's bgp_peer_row so the two projects report BGP the
// same way.
type bgpPeer struct {
	Peer     string `json:"peer"`
	RemoteAS uint64 `json:"remote_as"`
	State    string `json:"state"`
	Uptime   string `json:"uptime"`
	Prefixes uint64 `json:"prefixes_received"`
	AFI      string `json:"afi"`
}

// parseBGPSummary reshapes FRR's `show bgp summary json` into a flat,
// sorted peer list, plus the router id and local AS when present. FRR nests
// per-AFI: { "ipv4Unicast": { "routerId":..., "as":..., "peers": { "<ip>":
// {...} } }, "ipv6Unicast": {...} }. We walk both address families, exactly as
// parapet does, so a dual-stack box shows its v4 and v6 sessions together.
func parseBGPSummary(raw []byte) (peers []bgpPeer, routerID string, localAS uint64) {
	peers = []bgpPeer{}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return peers, "", 0
	}
	for _, afi := range []string{"ipv4Unicast", "ipv6Unicast"} {
		blob, ok := top[afi]
		if !ok {
			continue
		}
		var afiObj struct {
			RouterID string                     `json:"routerId"`
			AS       uint64                     `json:"as"`
			Peers    map[string]json.RawMessage `json:"peers"`
		}
		if err := json.Unmarshal(blob, &afiObj); err != nil {
			continue
		}
		if routerID == "" {
			routerID = afiObj.RouterID
		}
		if localAS == 0 {
			localAS = afiObj.AS
		}
		for ip, pinfo := range afiObj.Peers {
			peers = append(peers, bgpPeerRow(ip, afi, pinfo))
		}
	}
	// Deterministic order: by AFI then peer address, so the table doesn't
	// reshuffle on every refresh (Go map iteration is randomized).
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].AFI != peers[j].AFI {
			return peers[i].AFI < peers[j].AFI
		}
		return peers[i].Peer < peers[j].Peer
	})
	return peers, routerID, localAS
}

// bgpPeerRow builds one peer row from FRR's per-peer object, matching
// parapet's field selection: an established peer FRR reports with a numeric
// uptime and state "Established"; when state is absent we infer it from a
// positive peerUptimeMsec, the same fallback parapet uses.
func bgpPeerRow(ip, afi string, raw json.RawMessage) bgpPeer {
	var info struct {
		RemoteAS       uint64 `json:"remoteAs"`
		State          string `json:"state"`
		PeerUptime     string `json:"peerUptime"`
		PeerUptimeMsec uint64 `json:"peerUptimeMsec"`
		PfxRcd         uint64 `json:"pfxRcd"`
	}
	_ = json.Unmarshal(raw, &info)
	state := info.State
	if state == "" {
		if info.PeerUptimeMsec > 0 {
			state = "Established"
		}
	}
	return bgpPeer{
		Peer:     ip,
		RemoteAS: info.RemoteAS,
		State:    state,
		Uptime:   info.PeerUptime,
		Prefixes: info.PfxRcd,
		AFI:      afi,
	}
}

// bfdPeer is one BFD session row for the BFD Neighbors card. Field selection
// mirrors bgpPeer: enough to tell what's up/down and since when, not every
// counter `show bfd peers json` carries (detect-multiplier, RTT, etc. are
// left out — this is a status glance, not a diagnostics dump).
type bfdPeer struct {
	Peer       string `json:"peer"`
	Local      string `json:"local,omitempty"`
	Interface  string `json:"interface,omitempty"`
	Status     string `json:"status"`
	Uptime     int64  `json:"uptime,omitempty"`   // seconds; present when status is "up"
	Downtime   int64  `json:"downtime,omitempty"` // seconds; present when status is "down"
	Diagnostic string `json:"diagnostic,omitempty"`
}

// parseBFDPeers reshapes FRR's `show bfd peers json` — a flat array of
// session objects, unlike show bgp summary json's per-AFI nesting — into
// the bfdPeer rows the card needs. Field names (peer, local, interface,
// status, uptime, downtime, diagnostic) match bfdd_vty.c's
// __display_peer_json verbatim. Sorted by peer address for a stable display
// order, same reasoning as parseBGPSummary (Go map iteration isn't ordered,
// though here the source is an array — sorting still keeps the table from
// reshuffling if FRR's own array order isn't stable across calls).
func parseBFDPeers(raw []byte) []bfdPeer {
	var arr []struct {
		Peer       string `json:"peer"`
		Local      string `json:"local"`
		Interface  string `json:"interface"`
		Status     string `json:"status"`
		Uptime     int64  `json:"uptime"`
		Downtime   int64  `json:"downtime"`
		Diagnostic string `json:"diagnostic"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return []bfdPeer{}
	}
	peers := make([]bfdPeer, 0, len(arr))
	for _, p := range arr {
		peers = append(peers, bfdPeer{
			Peer: p.Peer, Local: p.Local, Interface: p.Interface,
			Status: p.Status, Uptime: p.Uptime, Downtime: p.Downtime,
			Diagnostic: p.Diagnostic,
		})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Peer < peers[j].Peer })
	return peers
}

// statFile is a thin seam over os.Stat so vtysh detection can be exercised in
// tests without a real FRR install. filepath.Clean guards against a caller
// passing a path with redundant separators.
var statFile = func(p string) (fs.FileInfo, error) { return os.Stat(filepath.Clean(p)) }
