//go:build darwin

package hostnet

// applyMode does nothing on macOS, deliberately.
//
// Addressing here lives in SystemConfiguration rather than on the interface,
// and networksetup(8) writes it and brings it into effect in one step. So the
// mode is applied by the persist backend, and doing anything with ifconfig here
// would set a mode SystemConfiguration does not know about, which it then
// overrides at its leisure — the silent-no-op failure this package exists to
// avoid, with the additional twist of appearing to work first.
//
// The consequence is that on a macOS host with no network service for the
// device, a mode change fails rather than half-applying. That is reported by
// persistNetworksetup, which already refuses that case for addresses.
func applyMode(Spec) error { return nil }
