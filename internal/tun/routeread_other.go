//go:build !linux && !freebsd

package tun

import "net/netip"

// OSRoute mirrors the Linux definition so callers compile everywhere. Reading
// the host routing table is Linux-only for now: the BSDs need a PF_ROUTE
// sysctl walk and Windows GetIpForwardTable2, neither of which is written yet.
// Returning no routes degrades to the explicit-Mesh-Routes behaviour rather
// than failing, which is the correct fallback.
type OSRoute struct {
	Prefix  netip.Prefix
	Gateway netip.Addr
}

func ListRoutesVia(family int, ifIndex int32) ([]OSRoute, error) { return nil, nil }
