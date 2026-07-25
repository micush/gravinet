//go:build !(linux && (amd64 || arm64))

package webadmin

// udpGSOSupported — see udpgso_caps_linux.go. udp_gso is accepted on every
// platform (config.Config.EnableUDPGSO has no build tags) but does nothing
// here: internal/transport has no GSO/GRO code path outside linux/amd64 and
// linux/arm64, so the setting is harmless, just inert.
const udpGSOSupported = false
