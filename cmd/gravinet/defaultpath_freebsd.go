//go:build freebsd

package main

// platformDefaultConfigPath returns the default -config path when none is
// given, matching where install/install-freebsd.sh puts it. FreeBSD
// reserves /etc for the base system; third-party software (ports/pkg, and
// this install script) uses /usr/local/etc instead.
func platformDefaultConfigPath() string { return "/usr/local/etc/gravinet/config.json" }
