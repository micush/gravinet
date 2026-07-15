//go:build windows

package config

import "os"

// DefaultControlSocket is the local IPC endpoint used by the CLI.
//
// "/run/gravinet.sock" (the old, Linux-only default) doesn't exist as a path
// on Windows at all — there's no drive-letter-less root — so this failed the
// same way it did on macOS/FreeBSD, just with a different missing directory.
//
// ProgramData is the same well-known, always-writable-by-the-service location
// already used for the config file (see defaultpath_windows.go); reuse it
// here too, one level down in a gravinet\ subfolder. Built with forward
// slashes rather than filepath.Join's native backslashes: internal/control's
// netAndAddr treats a path containing ":" with no "/" as a "host:port" TCP
// address, and a bare "C:\ProgramData\..." would trip that — the forward
// slash keeps it correctly classified as a unix (named-pipe-backed) socket.
var DefaultControlSocket = func() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = "C:/ProgramData"
	}
	base = replaceBackslashes(base)
	return base + "/gravinet/gravinet.sock"
}()

func replaceBackslashes(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			out[i] = '/'
		} else {
			out[i] = s[i]
		}
	}
	return string(out)
}
