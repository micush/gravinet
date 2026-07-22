package main

import (
	"errors"
	"net"
	"strconv"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// seedNotesForEndpoint finds the notes on a configured seed whose address
// matches a peer's live observed endpoint. Mirrors the same ambiguity-safe
// matching the web UI's hover-tooltip uses (internal/webadmin/ui.go's
// seedNotesForAddr): an exact host:port match wins outright — it pins down
// one specific seed row by construction — but a host-only match (a seed
// entered without a port, or the peer currently on a port that isn't the one
// in its own seed) is only trusted when every seed sharing that host agrees
// on the same note. Two different boxes behind one NAT'd IP, told apart only
// by which port gets forwarded to which, is exactly the case a seed list can
// express precisely but a bare host can't disambiguate — so an ambiguous
// host-only match returns "" rather than guessing.
func seedNotesForEndpoint(seeds []config.Seed, endpoint string) string {
	if endpoint == "" || len(seeds) == 0 {
		return ""
	}
	epHost, _, epErr := net.SplitHostPort(endpoint)
	hostNotes := map[string]bool{}
	for _, s := range seeds {
		if s.Notes == "" {
			continue
		}
		_, addr := config.SeedParts(s.Address)
		if strings.EqualFold(addr, endpoint) {
			return s.Notes // exact host:port
		}
		if epErr != nil {
			continue // endpoint isn't host:port shaped; nothing left to compare
		}
		sHost, _, sErr := net.SplitHostPort(addr)
		if sErr != nil {
			sHost = addr // bare host, no port
		}
		if sHost != "" && strings.EqualFold(sHost, epHost) {
			hostNotes[s.Notes] = true
		}
	}
	if len(hostNotes) == 1 {
		for k := range hostNotes {
			return k
		}
	}
	return ""
}

// errNoSeedNoteChange signals "nothing to persist" from inheritSeedNotes'
// config.WithLock callback without that being treated as a real failure.
var errNoSeedNoteChange = errors.New("no seed-note changes")

// applySeedNoteInheritance mutates cfg in place, copying a matching seed's
// notes onto any connected peer (from peersByNetwork, keyed by the same
// uint64 network id used everywhere else) that doesn't have its own note
// yet. Returns whether anything actually changed, so the caller can skip
// Validate/Save when there's nothing to persist.
//
// Split out from inheritSeedNotes so the actual decision logic — which peer
// gets which note, and when — can be tested directly against constructed
// mesh.PeerInfo values, without needing a live *mesh.Engine or an actual
// mesh session to produce one.
func applySeedNoteInheritance(cfg *config.Config, peersByNetwork map[uint64][]mesh.PeerInfo) bool {
	changed := false
	for i := range cfg.Networks {
		n := &cfg.Networks[i]
		if len(n.Seeds) == 0 {
			continue
		}
		netID, err := strconv.ParseUint(n.ID, 16, 64)
		if err != nil {
			continue
		}
		for _, pi := range peersByNetwork[netID] {
			if pi.NodeID == "" || n.PeerNotes[pi.NodeID] != "" {
				continue // no id to key on, or already has its own note
			}
			notes := seedNotesForEndpoint(n.Seeds, pi.Endpoint)
			if notes == "" {
				continue
			}
			if n.PeerNotes == nil {
				n.PeerNotes = map[string]string{}
			}
			n.PeerNotes[pi.NodeID] = notes
			changed = true
			logx.Infof("mesh: peer %s on %s inherited its note from a matching seed", pi.NodeID, n.Name)
		}
	}
	return changed
}

// inheritSeedNotes gives a connected peer a permanent note the first time
// its live endpoint matches a configured seed's (see seedNotesForEndpoint
// and applySeedNoteInheritance) — run periodically (see its ticker in
// run()), not on the connection event itself, since this is operator-facing
// metadata, not anything latency-sensitive, and a periodic disk-backed pass
// is far simpler than threading a callback through the handshake path for
// it.
//
// Once copied, the note lives on the peer's own node id (Config.PeerNotes) —
// the same field the Mesh -> Peers/Bans notes columns already read and
// write — so it survives whatever the peer's address does next: a
// LAN-discovered direct path, a NAT rebind, a stretch spent relayed, none of
// it matters once the id has its own note. Never overwrites an existing
// peer note (an operator's own edit always wins) and never touches the
// seed's own note either — the seed stays as the description for whoever
// hasn't connected yet.
func inheritSeedNotes(cfgPath string, eng *mesh.Engine, reload func() error) {
	err := config.WithLock(cfgPath, func() error {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}
		peersByNetwork := map[uint64][]mesh.PeerInfo{}
		for i := range cfg.Networks {
			netID, err := strconv.ParseUint(cfg.Networks[i].ID, 16, 64)
			if err != nil {
				continue
			}
			peersByNetwork[netID] = eng.ListPeers(netID)
		}
		if !applySeedNoteInheritance(cfg, peersByNetwork) {
			return errNoSeedNoteChange
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		return cfg.SaveTo(cfgPath)
	})
	if err == errNoSeedNoteChange {
		return
	}
	if err != nil {
		logx.Warnf("seed-note inheritance: %v", err)
		return
	}
	// Applied on disk; push it into the live engine too (same as any other
	// config edit) so the change shows up immediately rather than waiting on
	// the next unrelated reload.
	if reload != nil {
		if e := reload(); e != nil {
			logx.Warnf("seed-note inheritance: reload after saving: %v", e)
		}
	}
}
