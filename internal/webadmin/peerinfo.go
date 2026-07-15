package webadmin

import (
	"context"
	"net/http"
	"time"
)

// handlePeerInfo runs forward DNS, reverse DNS, WHOIS, and (if enabled)
// Geo-IP against a connected peer's observed underlay endpoint, for the info
// (🛈) button next to a ticked peer in the web UI's Peers section — the peer
// analog of handleSeedInfo, reusing the exact same lookupSeedInfo core
// (seedHost already handles a bare "host:port" endpoint fine, the same as it
// does a seed address). Unlike a seed's address, a peer's endpoint isn't
// config — it's this session's observed underlay address (most often a NAT
// mapping) — so it isn't looked up server-side; the caller passes the
// endpoint the UI just rendered for that peer, same trust model
// handleSeedInfo already uses for a seed's address.
func (s *Server) handlePeerInfo(w http.ResponseWriter, r *http.Request) {
	var req struct{ Net, Node, Endpoint string }
	if !decode(w, r, &req) {
		return
	}
	host := seedHost(req.Endpoint)
	if host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no underlay endpoint to look up for this peer yet"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, lookupSeedInfo(ctx, host, s.cfg.GeoIPEnabled()))
}
