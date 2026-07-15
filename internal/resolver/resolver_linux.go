//go:build linux

package resolver

import (
	"fmt"
	"os/exec"
	"strings"
)

// resolvedDown reports whether err is D-Bus telling us systemd-resolved isn't
// there to talk to, rather than a genuine per-link failure.
//
// resolvectl ships in the base systemd package, so exec.LookPath finds it on
// every systemd host — including the many where systemd-resolved itself is not
// running. RHEL, Rocky, Alma and CentOS don't enable it by default (NetworkManager
// writes /etc/resolv.conf itself); Fedora and Debian/Ubuntu do. On those hosts the
// binary runs, fails to reach the service over D-Bus, and reports:
//
//	Failed to set DNS configuration: The name is not activatable
//
// which says nothing about systemd-resolved, names no remedy, and reads like a
// gravinet bug. "The name" is the D-Bus bus name org.freedesktop.resolve1, and
// "not activatable" means nothing is installed to answer it.
func resolvedDown(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, marker := range []string{
		"not activatable",            // no unit installed to answer the bus name
		"org.freedesktop.resolve1",   // named explicitly by some systemd versions
		"Failed to activate service", // installed but the service won't start
		"Could not activate remote peer",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// explainResolved wraps a raw resolvectl failure with the cause and the fix, when
// the cause is that systemd-resolved isn't running at all. Anything else is passed
// through untouched — a real per-link error shouldn't be buried under advice about
// a service that's working fine.
func explainResolved(err error) error {
	if !resolvedDown(err) {
		return err
	}
	return fmt.Errorf("%w\n\n"+
		"systemd-resolved is not running on this host, so there is nothing for resolvectl to\n"+
		"configure (\"the name\" in that error is the D-Bus name org.freedesktop.resolve1).\n"+
		"gravinet's per-network DNS forwarding is implemented on Linux only via\n"+
		"systemd-resolved's per-link routing domains. RHEL, Rocky, Alma and CentOS do not\n"+
		"enable it by default — NetworkManager writes /etc/resolv.conf itself.\n\n"+
		"To enable it (this is exactly what install/install-linux.sh now does):\n"+
		"    dnf install -y systemd-resolved          # RHEL 9+/Fedora ship it as its own package\n"+
		"    systemctl enable --now systemd-resolved\n"+
		"    ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf\n"+
		"    printf '[main]\\ndns=systemd-resolved\\n' > /etc/NetworkManager/conf.d/10-gravinet-resolved.conf\n"+
		"    systemctl reload NetworkManager\n\n"+
		"Or, if you don't want this host's DNS stack changed, turn off DNS forwarding for\n"+
		"this network — nothing else in gravinet depends on systemd-resolved", err)
}

// Sync registers entries' domains as routing domains, and searchDomains as
// search domains, on iface. systemd-resolved's per-link DNS config takes one
// shared server set (see the package doc for why), so unlike Sync on
// darwin/windows this can't keep each routing domain's servers independent;
// if that independence is ever required, a local forwarder in front of a
// single registered server (127.0.0.1) is the extension point, without
// changing this function's signature.
//
// Routing and search domains are always applied in the same `resolvectl
// domain` invocation: that command replaces iface's entire domain list, so
// calling it once for routing domains and again for search domains would
// have the second call silently wipe out the first.
//
// tag is accepted for API symmetry with the other platforms (and so callers
// can pass the same network-scoped identifier everywhere) but isn't needed
// here: systemd-resolved's state is inherently per-link, and each gravinet
// network already owns its own tun interface, so iface alone disambiguates.
func Sync(tag, iface string, entries []Entry, searchDomains []string) error {
	if iface == "" {
		return fmt.Errorf("resolver: sync requires a non-empty interface")
	}
	if _, err := exec.LookPath("resolvectl"); err != nil {
		return fmt.Errorf("resolver: resolvectl not found on PATH (systemd-resolved required): %w", err)
	}
	routing, servers := linuxRoutingArgs(entries)
	search := searchDomainArgs(searchDomains)
	if len(routing) == 0 && len(search) == 0 {
		return Clear(tag, iface)
	}

	// Order matters for a clean apply: set the server set first so it exists
	// by the time the routing domains start directing queries at it. Search
	// domains need no dedicated server of their own — a completed name just
	// resolves through whatever DNS is otherwise in effect — so this step is
	// skipped entirely for a search-only sync.
	if len(routing) > 0 {
		if len(servers) == 0 {
			return fmt.Errorf("resolver: sync on %s: no valid servers among %d entries", iface, len(entries))
		}
		if err := run("resolvectl", append([]string{"dns", iface}, servers...)...); err != nil {
			return explainResolved(fmt.Errorf("resolver: set dns on %s: %w", iface, err))
		}
	}
	domains := append(append([]string{}, routing...), search...)
	if err := run("resolvectl", append([]string{"domain", iface}, domains...)...); err != nil {
		return explainResolved(fmt.Errorf("resolver: set domain on %s: %w", iface, err))
	}
	return nil
}

// Clear reverts iface's per-link DNS config, undoing exactly what Sync applied
// (systemd-resolved has no other owner of per-link state to disturb, so a full
// revert is safe and simpler than tracking a prior domain/server set to diff
// against).
func Clear(tag, iface string) error {
	if iface == "" {
		return fmt.Errorf("resolver: clear requires a non-empty interface")
	}
	if _, err := exec.LookPath("resolvectl"); err != nil {
		// Nothing to revert if systemd-resolved isn't in use.
		return nil
	}
	if err := run("resolvectl", "revert", iface); err != nil {
		// Same reasoning as the LookPath check above, one layer further in: if
		// systemd-resolved isn't running, it cannot be holding any per-link state
		// for us to revert, so there is nothing to fail at. Failing here would make
		// teardown (network disable, config reload, shutdown) noisily report an
		// error about undoing something that was never applied — the Sync that
		// would have applied it failed for this very same reason.
		if resolvedDown(err) {
			return nil
		}
		return fmt.Errorf("resolver: revert %s: %w", iface, err)
	}
	return nil
}

// Dump reports the live systemd-resolved state for iface — the actual current
// registration, read back from resolvectl rather than from anything gravinet
// remembers applying, so it reflects reality even if something else changed it
// or a prior Sync silently failed. tag is unused on Linux (state is inherently
// per-link; see Sync's doc comment) but kept for API symmetry.
func Dump(tag, iface string) (string, error) {
	if iface == "" {
		return "", fmt.Errorf("resolver: dump requires a non-empty interface")
	}
	if _, err := exec.LookPath("resolvectl"); err != nil {
		return "", fmt.Errorf("resolver: resolvectl not found on PATH (systemd-resolved required): %w", err)
	}
	domOut, domErr := exec.Command("resolvectl", "domain", iface).CombinedOutput()
	dnsOut, dnsErr := exec.Command("resolvectl", "dns", iface).CombinedOutput()
	var b strings.Builder
	fmt.Fprintf(&b, "$ resolvectl domain %s\n%s", iface, strings.TrimRight(string(domOut), "\n"))
	if domErr != nil {
		fmt.Fprintf(&b, "  (%v)", domErr)
	}
	fmt.Fprintf(&b, "\n\n$ resolvectl dns %s\n%s", iface, strings.TrimRight(string(dnsOut), "\n"))
	if dnsErr != nil {
		fmt.Fprintf(&b, "  (%v)", dnsErr)
	}
	return b.String(), nil
}

func run(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
