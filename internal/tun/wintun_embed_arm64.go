//go:build windows && arm64

package tun

import _ "embed"

//go:embed wintun/arm64/wintun.dll
var wintunDLLBytes []byte
