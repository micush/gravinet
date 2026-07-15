//go:build !windows && !freebsd

package main

// platformDefaultConfigPath returns the default -config path when none is
// given, matching where install/install-linux.sh and install-macos.sh both
// put it. FreeBSD has its own default in defaultpath_freebsd.go.
func platformDefaultConfigPath() string { return "/etc/gravinet/config.json" }
