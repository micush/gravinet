package tun

import "net/netip"

// Gateway describes a host's current physical default route: which address
// to hand packets to, and which interface to send them out. Shared across
// every platform's implementation (gateway_linux.go today; the others'
// eventual real backends and gateway_unsupported.go's stub in the meantime)
// so callers in internal/mesh have one type to work with regardless of
// platform, without needing their own build tags.
type Gateway struct {
	Addr    netip.Addr
	IfIndex int32
	Metric  int
}
