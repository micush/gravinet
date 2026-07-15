//go:build darwin

package resolver

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// resolverDir is macOS's built-in per-domain conditional-forwarder directory:
// a file named after a domain, containing "nameserver" lines, routes only
// queries under that domain to those servers. Nothing outside the named
// domain is affected, so this never touches the system's default resolver.
// A var (not const) so tests can redirect it to a temp directory rather than
// touching the real /etc/resolver.
var resolverDir = "/etc/resolver"

// marker is written as the first line of every file gravinet creates, so a
// later Sync (including after a daemon restart, when no in-memory record of
// what was previously written survives) can tell which files under
// resolverDir it owns for tag versus files it must leave alone — an operator's
// own /etc/resolver/foo, or another tag's.
func marker(tag string) string { return "# gravinet " + tag }

// Sync writes one file per entry under /etc/resolver, each listing that
// entry's own Servers, and removes any file previously written for tag whose
// domain is no longer present. Unlike Linux, each domain keeps its own
// independent server set — /etc/resolver has no shared-per-link constraint.
//
// searchDomains is accepted for API symmetry with Linux and Windows, but
// macOS has no interface-scoped equivalent: a real search domain here is a
// property of a network *service*'s configuration (networksetup
// -setsearchdomains), not of gravinet's own tun interface, and reaching into
// that would be a materially more invasive change than anything else this
// file does. Routing domains (entries) are still applied normally; a non-
// empty searchDomains only adds a trailing error naming what wasn't applied,
// so the caller can log it without routing domains failing along with it.
func Sync(tag, iface string, entries []Entry, searchDomains []string) error {
	if err := syncRoutingDomains(tag, entries); err != nil {
		return err
	}
	if len(searchDomains) > 0 {
		return fmt.Errorf("%w: search domains are not supported on macOS (%d domain(s) not applied) — "+
			"no interface-scoped mechanism exists; set them on a network service instead (networksetup -setsearchdomains) if needed",
			ErrSearchDomainsUnsupported, len(searchDomains))
	}
	return nil
}

func syncRoutingDomains(tag string, entries []Entry) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("resolver: sync requires a non-empty tag")
	}
	if err := os.MkdirAll(resolverDir, 0o755); err != nil {
		return fmt.Errorf("resolver: %w", err)
	}

	owned, err := ownedDomains(tag)
	if err != nil {
		return err
	}

	want := make(map[string][]string, len(entries))
	for _, e := range entries {
		d := normalizeDomain(e.Domain)
		if d == "" || len(e.Servers) == 0 {
			continue
		}
		lines := make([]string, 0, len(e.Servers))
		for _, s := range e.Servers {
			if s.IsValid() {
				lines = append(lines, s.String())
			}
		}
		if len(lines) > 0 {
			want[d] = lines
		}
	}

	var errs []string

	// Remove files we own for this tag that are no longer wanted.
	for d := range owned {
		if _, keep := want[d]; !keep {
			if err := os.Remove(filepath.Join(resolverDir, d)); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err.Error())
			}
		}
	}

	// Write (or overwrite) every wanted file.
	for d, servers := range want {
		if err := writeResolverFile(tag, d, servers); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("resolver: sync: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Clear removes every file previously written for tag.
func Clear(tag, iface string) error {
	return Sync(tag, iface, nil, nil)
}

// Dump reports the live /etc/resolver state for tag's domains — reads each
// owned file back from disk rather than from anything gravinet remembers
// writing, so it reflects reality even if a file was edited or removed by
// something else since the last Sync. iface is unused on macOS (registration
// is a global domain namespace, not per-interface) but kept for API symmetry.
func Dump(tag, iface string) (string, error) {
	owned, err := ownedDomains(tag)
	if err != nil {
		return "", err
	}
	if len(owned) == 0 {
		return "(no /etc/resolver entries owned by this network)", nil
	}
	domains := make([]string, 0, len(owned))
	for d := range owned {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	var b strings.Builder
	for i, d := range domains {
		if i > 0 {
			b.WriteString("\n")
		}
		path := filepath.Join(resolverDir, d)
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(&b, "%s: (could not read %s: %v)\n", d, path, err)
			continue
		}
		fmt.Fprintf(&b, "%s (%s):\n", d, path)
		// Skip the marker line (first line) — it's ownership bookkeeping, not
		// useful to a reader trying to see what's actually registered.
		lines := strings.SplitN(string(data), "\n", 2)
		body := ""
		if len(lines) > 1 {
			body = lines[1]
		}
		b.WriteString(strings.TrimRight(body, "\n"))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// ownedDomains scans resolverDir and returns the set of domains whose file's
// first line matches tag's marker.
func ownedDomains(tag string) (map[string]struct{}, error) {
	owned := map[string]struct{}{}
	entries, err := os.ReadDir(resolverDir)
	if err != nil {
		if os.IsNotExist(err) {
			return owned, nil
		}
		return nil, fmt.Errorf("resolver: %w", err)
	}
	want := marker(tag)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(resolverDir, e.Name()))
		if err != nil {
			continue // best-effort; a file we can't read isn't one we can safely claim
		}
		first := strings.SplitN(string(data), "\n", 2)[0]
		if strings.TrimSpace(first) == want {
			owned[e.Name()] = struct{}{}
		}
	}
	return owned, nil
}

func writeResolverFile(tag, domain string, servers []string) error {
	var b strings.Builder
	b.WriteString(marker(tag))
	b.WriteString("\n")
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	path := filepath.Join(resolverDir, domain)
	tmp, err := os.CreateTemp(resolverDir, ".gravinet-resolver-*")
	if err != nil {
		return os.WriteFile(path, []byte(b.String()), 0o644) // fall back to direct write
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return os.WriteFile(path, []byte(b.String()), 0o644)
	}
	return nil
}
