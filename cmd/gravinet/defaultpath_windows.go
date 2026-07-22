//go:build windows

package main

import (
	"os"
	"path/filepath"
)

// platformDefaultConfigPath returns the default -config path when none is
// given, matching where install/install-windows.ps1 puts it
// ($env:ProgramData\gravinet\config.json). ProgramData is set by Windows
// itself on every supported release, but if it's somehow missing (e.g. a
// stripped-down or non-standard environment) fall back to the well-known
// literal path rather than resolving to something nonsensical.
func platformDefaultConfigPath() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "gravinet", "config.json")
}
