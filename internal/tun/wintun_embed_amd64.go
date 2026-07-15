//go:build windows && amd64

package tun

import _ "embed"

// wintunDLLBytes is the Wintun driver DLL bundled into the binary at build time.
// In the source tree this is a placeholder; the release build replaces it with
// the signed wintun.dll for this architecture from wintun.net.
//
//go:embed wintun/amd64/wintun.dll
var wintunDLLBytes []byte
