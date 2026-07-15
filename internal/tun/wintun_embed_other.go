//go:build windows && !amd64 && !arm64

package tun

// No embedded Wintun driver for this architecture; the backend falls back to a
// wintun.dll shipped beside the executable.
var wintunDLLBytes []byte
