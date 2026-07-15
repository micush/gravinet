//go:build !linux && !darwin && !windows && !freebsd && !openbsd

package resolver

import "fmt"

// Sync reports that conditional-forwarding registration is unsupported on
// this platform, mirroring netfilter's !linux stub.
func Sync(tag, iface string, entries []Entry, searchDomains []string) error {
	if len(entries) == 0 && len(searchDomains) == 0 {
		return nil // nothing requested; nothing to fail
	}
	return fmt.Errorf("resolver: conditional DNS forwarding and search domains are not supported on this platform")
}

// Clear is a no-op: there is nothing this platform could have applied.
func Clear(tag, iface string) error { return nil }

// Dump reports that this platform has no conditional-forwarding state to show.
func Dump(tag, iface string) (string, error) {
	return "", fmt.Errorf("resolver: conditional DNS forwarding is not supported on this platform")
}
