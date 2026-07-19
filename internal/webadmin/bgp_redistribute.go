package webadmin

// bgpMeshRedistributor is the BGP-into-mesh half of route redistribution —
// config.Network.RedistributeBGP's counterpart to BGPConfig.RedistributeMeshRoutes
// (mesh-into-BGP, see bgp.go's meshRouteCIDRs/reconcileMeshRedistribute). It
// periodically pulls FRR's current best-path routes (bgpLearnedRoutes) and
// pushes them into the mesh via Backend.SetBGPRoutes, for every network that
// wants them.
//
// The two directions can't share a mechanism. reconcileMeshRedistribute
// reacts to a config edit because its source — the Mesh Routes page's
// Advertise table — *is* config: gravinet sees every change the moment it
// happens. This direction's source is the live BGP RIB, state gravinet
// doesn't own and gets no edit event for at all: a route can appear or
// disappear because a remote BGP peer changed something on their end,
// which nothing in gravinet's own config ever observes. Polling is the only
// option here, not a stopgap for something event-driven later.

import (
	"net/netip"
	"strconv"
	"sync"
	"time"

	"gravinet/internal/config"
)

// bgpRedistributePollInterval is how often bgpMeshRedistributor re-polls FRR
// and re-syncs the mesh. Well above bgpQueryTimeout (8s) so a slow vtysh call
// can't cause ticks to stack up, but frequent enough that a BGP route change
// shows up in the mesh within a handful of seconds rather than minutes. vtysh
// itself — a subprocess spawn plus marshaling however large the RIB is — is
// the expensive part of a tick, which is why this is nowhere near as tight as
// metricSampleInterval's 2s.
const bgpRedistributePollInterval = 15 * time.Second

// bgpMeshRedistributor's mu guards active: the set of networkIDs it's
// currently redistributing into, so a network that just stopped wanting this
// (RedistributeBGP toggled off, the network disabled/deleted, or BGP itself
// going down) gets one final clearing SetBGPRoutes call instead of being left
// with stale routes gossiped into the mesh forever — sync() has no other way
// to know "this used to be active" without remembering it itself.
type bgpMeshRedistributor struct {
	s    *Server
	stop chan struct{}

	mu     sync.Mutex
	active map[uint64]bool
}

func newBGPMeshRedistributor(s *Server) *bgpMeshRedistributor {
	return &bgpMeshRedistributor{s: s, stop: make(chan struct{}), active: map[uint64]bool{}}
}

func (r *bgpMeshRedistributor) run() {
	t := time.NewTicker(bgpRedistributePollInterval)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.sync()
		}
	}
}

func (r *bgpMeshRedistributor) close() { close(r.stop) }

// bgpLearnedRoutesFn is bgpLearnedRoutes, swappable via this package var
// purely for testability — TestBGPMeshRedistributorSync (bgp_redistribute_
// test.go) substitutes a fake to exercise sync()'s filtering/active-set logic
// without a live FRR install, the same shape metrics.go's readCPUTotalsFn
// and friends already use for the same reason. Never swapped outside tests.
var bgpLearnedRoutesFn = bgpLearnedRoutes

// sync is one poll-and-push cycle: read config, decide which networks want
// BGP-into-mesh redistribution right now, fetch the RIB once — not once per
// network; an FRR box can carry several mesh networks and the RIB doesn't
// change per-network — filter, and push. It's its own method (rather than
// folded into run's select loop) so a config edit that wants an immediate
// resync, instead of waiting up to one poll interval, can just call it
// directly (see the calls alongside reconcileMeshRedistribute in
// bgp.go/edit.go).
//
// There's no separate frrInstalled() gate here before fetching: FRR being
// absent already makes bgpLearnedRoutesFn (via runVtysh) return nothing, on
// its own, without a special case — one fewer place the two could disagree
// about what "FRR isn't there" means, and every network that wanted this
// still correctly gets its redistribution set cleared to empty rather than
// skipped outright, which comes out the same either way.
func (r *bgpMeshRedistributor) sync() {
	if r.s.configPath == "" {
		return
	}
	cfg, err := config.Load(r.s.configPath)
	if err != nil {
		return
	}

	wantsAny := false
	for i := range cfg.Networks {
		if cfg.Networks[i].Enabled && cfg.Networks[i].RedistributeBGP {
			wantsAny = true
			break
		}
	}
	r.mu.Lock()
	hadActive := len(r.active) > 0
	r.mu.Unlock()
	if !wantsAny && !hadActive {
		return // nothing wants this and nothing left to clear — skip the vtysh round-trip entirely
	}

	bgpUp := cfg.BGP.Enabled && cfg.BGP.ASN != 0
	var learned []netip.Prefix
	if bgpUp {
		learned = bgpLearnedRoutesFn()
	}

	// Never redistribute a route this node already originates into the mesh
	// itself — the immediate, single-node case of the mutual-redistribution
	// loop any bidirectional BGP<->IGP redistribution has to guard against:
	// with BGPConfig.RedistributeMeshRoutes also on, this node's own Advertise
	// routes appear right back in its own BGP RIB, and without this check
	// they would bounce straight back into the mesh looking like genuinely
	// external routes. meshRouteCIDRs covers every network's Advertise
	// table, not just whichever one is being redistributed into — BGP is one
	// global speaker shared across every mesh network this node belongs to,
	// so a route advertised on network A has to be excluded from network B's
	// redistribution too, not just A's. This does not (and, short of FRR
	// itself carrying a route tag or community gravinet could check for,
	// cannot) catch every possible loop across a larger multi-node topology
	// — the same caveat any router vendor's mutual-redistribution
	// documentation carries.
	exclude := make(map[string]bool)
	for _, c := range meshRouteCIDRs(cfg) {
		exclude[c] = true
	}
	var candidate []netip.Prefix
	for _, p := range learned {
		if !exclude[p.String()] {
			candidate = append(candidate, p)
		}
	}

	next := make(map[uint64]bool)
	for i := range cfg.Networks {
		n := &cfg.Networks[i]
		if !n.Enabled || !n.RedistributeBGP || !bgpUp {
			continue
		}
		id, err := strconv.ParseUint(n.ID, 16, 64)
		if err != nil {
			continue
		}
		if r.s.be.SetBGPRoutes(id, candidate, n.RedistributeBGPMetric) {
			next[id] = true
		}
	}

	r.mu.Lock()
	prev := r.active
	r.active = next
	r.mu.Unlock()
	for id := range prev {
		if !next[id] {
			// Stopped wanting this since the last cycle — clear it rather
			// than leaving whatever was last pushed gossiping forever.
			r.s.be.SetBGPRoutes(id, nil, 0)
		}
	}
}
