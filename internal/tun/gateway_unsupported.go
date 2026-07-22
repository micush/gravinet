//go:build !linux && !windows && !freebsd && !darwin && !openbsd

package tun

// Stand-in for any platform gravinet doesn't have a real DefaultGateway/
// AddGatewayRoute/DelGatewayRoute implementation for. As of this writing
// that's every platform gravinet actually ships builds for (linux,
// windows, freebsd, darwin, openbsd — see gateway_linux.go,
// gateway_windows.go, gateway_freebsd.go, gateway_darwin.go,
// gateway_openbsd.go respectively), so this file is a forward-compatibility
// safety net rather than a live gap: if gravinet ever adds a new build
// target, this is what makes full-tunnel refuse cleanly there by default
// instead of either failing to link or — the actually dangerous failure
// mode, see syncFullTunnelRoute's hard GatewaySupported guard in
// routes.go — silently installing the default-route half of full-tunnel
// with no bypass-route safety net behind it. Calling any of these here
// returns a clear, actionable error, the same shape as
// pty_unsupported.go's errShellUnsupported for remote shell before v308
// added real per-platform pty backends.

import (
	"fmt"
	"net/netip"
)

// GatewaySupported is false here; see gateway_linux.go's doc comment for
// why internal/mesh's syncFullTunnelRoute treats this as a hard
// prerequisite for activating full-tunnel at all, not just a capability
// note.
const GatewaySupported = false

// RouteDemotionNeeded is false here — moot, since GatewaySupported above
// already keeps syncFullTunnelRoute from ever activating full-tunnel on a
// platform that lands in this file, so DemoteDefaultRoute below is never
// actually called either. Defined anyway for the same uniform-symbol reason
// as every other function here.
const RouteDemotionNeeded = false

var errGatewayUnsupported = fmt.Errorf("default-gateway detection is not implemented on this platform yet")

func DefaultGateway(family int, excludeIfIndex int32) (Gateway, error) {
	return Gateway{}, errGatewayUnsupported
}

func DemoteDefaultRoute(family int, excludeIfIndex int32, newMetric int) (int, error) {
	return 0, errGatewayUnsupported
}

func AddGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	return errGatewayUnsupported
}

func DelGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	return errGatewayUnsupported
}
