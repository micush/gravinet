//go:build !linux

package webadmin

// readDefaultGateway has no portable implementation yet. The inventory still
// lists every interface and address; only the gateway columns are blank,
// which is honest about what this platform can currently report rather than
// guessing from the interface list.
func readDefaultGateway(v6 bool) gwInfo { return gwInfo{} }
