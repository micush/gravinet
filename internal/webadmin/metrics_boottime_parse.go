package webadmin

import (
	"strconv"
	"strings"
)

// parseBoottimeSysctl parses the textual output of `sysctl -n kern.boottime`
// on the BSD/Darwin family, which prints the kernel's struct timeval boot
// time as "{ sec = 1697000000, usec = 123456 } Wed Oct 11 09:06:40 2023" on
// macOS and FreeBSD. Only the leading "sec = N" is needed; usec and the
// trailing human-readable date are ignored. Split out from the per-platform
// readers (metrics_darwin.go, metrics_freebsd.go, metrics_openbsd.go) so it
// can be unit-tested without one of those OSes to run sysctl on, the same
// way metrics_darwin_parse.go and metrics_openbsd_parse.go already do for
// their own readers.
func parseBoottimeSysctl(out []byte) (bootUnixSec int64, ok bool) {
	s := string(out)
	i := strings.Index(s, "sec")
	if i < 0 {
		return 0, false
	}
	s = s[i+len("sec"):]
	eq := strings.IndexByte(s, '=')
	if eq < 0 {
		return 0, false
	}
	s = strings.TrimSpace(s[eq+1:])
	j := 0
	if j < len(s) && s[j] == '-' {
		j++
	}
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:j], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseBareEpochSysctl parses output that's nothing but a bare integer (Unix
// epoch seconds), for a sysctl(8) that doesn't use the "{ sec = ... }"
// struct-print convention parseBoottimeSysctl handles — see the OpenBSD
// reader for why this exists as a fallback rather than the primary parse.
func parseBareEpochSysctl(out []byte) (bootUnixSec int64, ok bool) {
	s := strings.TrimSpace(string(out))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
