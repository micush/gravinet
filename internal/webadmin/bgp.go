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
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gravinet/internal/config"
)

// bgpVtyshPaths are the locations vtysh is installed to, in priority order.
// Ported verbatim from parapet's FRR integration (frr.rs / status.rs), which
// probes exactly these three. A path check (rather than a $PATH lookup) keeps
// detection cheap and side-effect-free: no subprocess is spawned just to learn
// whether the binary exists.
var bgpVtyshPaths = []string{"/usr/bin/vtysh", "/usr/sbin/vtysh", "/bin/vtysh"}

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
	// FRR emits JSON when the command ends in "json". runVtysh bounds this hard,
	// so a wedged FRR socket can never hang the request.
	out, ran := runVtysh("show ip bgp summary json")
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
		if s.configPath != "" {
			if cfg, err := config.Load(s.configPath); err == nil {
				bgp = cfg.BGP
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
		})
		return
	}

	var req config.BGPConfig
	if !decode(w, r, &req) {
		return
	}
	// Persist first (mutateConfig validates + saves + fires the daemon's reload
	// hook), so the durable record is updated before we touch FRR.
	if err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.BGP = req
		return nil
	}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// Then reconcile FRR with the freshly-saved config. A no-op (with a note)
	// when FRR isn't installed — never fatal to the save.
	note, err := applyBGP(req, s.log)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": false, "note": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": frrInstalled(), "note": note})
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
	bgp, hasPw, ok := importBGPFromFRR()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"imported": false})
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

// parseBGPSummary reshapes FRR's `show ip bgp summary json` into a flat,
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

// statFile is a thin seam over os.Stat so vtysh detection can be exercised in
// tests without a real FRR install. filepath.Clean guards against a caller
// passing a path with redundant separators.
var statFile = func(p string) (fs.FileInfo, error) { return os.Stat(filepath.Clean(p)) }
