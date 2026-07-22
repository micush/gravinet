//go:build windows

package resolver

import (
	"fmt"
	"os/exec"
	"strings"
)

// NRPT (Name Resolution Policy Table) rules route queries under a namespace
// to specific servers without touching the machine's default DNS config —
// Windows's equivalent of macOS's /etc/resolver, one rule per domain, each
// with its own independent server set.
//
// Rules are tagged via -Comment so a later Sync (including after a daemon
// restart) can identify and reconcile exactly the rules it previously
// created, the same discipline as the marker line in resolver_darwin.go.

func comment(tag string) string { return "gravinet " + tag }

// Sync creates or replaces one NRPT rule per entry (namespace ".<domain>",
// that entry's own Servers) and removes any tag-owned rule whose domain is no
// longer present. It also sets iface's connection-specific DNS suffix to the
// first of searchDomains — the per-adapter equivalent of a search domain,
// scoped to gravinet's own adapter the same way NRPT rules are scoped by
// namespace. Windows only supports one such suffix per adapter (unlike
// Linux's full per-link domain list), so a second or later search domain is
// reported as unapplied via a trailing error rather than silently dropped.
func Sync(tag, iface string, entries []Entry, searchDomains []string) error {
	if err := syncRoutingDomains(tag, entries); err != nil {
		return err
	}
	return syncSearchDomain(iface, searchDomains)
}

func syncRoutingDomains(tag string, entries []Entry) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("resolver: sync requires a non-empty tag")
	}

	owned, err := ownedRules(tag)
	if err != nil {
		return err
	}

	want := make(map[string][]string, len(entries))
	for _, e := range entries {
		d := normalizeDomain(e.Domain)
		if d == "" || len(e.Servers) == 0 {
			continue
		}
		servers := make([]string, 0, len(e.Servers))
		for _, s := range e.Servers {
			if s.IsValid() {
				servers = append(servers, s.String())
			}
		}
		if len(servers) > 0 {
			want[d] = servers
		}
	}

	var errs []string

	for domain := range owned {
		if _, keep := want[domain]; !keep {
			if err := removeRule(tag, domain); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}

	for domain, servers := range want {
		// Replace unconditionally (remove-then-add) rather than diffing
		// server lists, since NRPT rules are cheap to recreate and this
		// avoids needing to compare the existing rule's server set.
		if _, isOwned := owned[domain]; isOwned {
			if err := removeRule(tag, domain); err != nil {
				errs = append(errs, err.Error())
				continue
			}
		}
		if err := addRule(tag, domain, servers); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("resolver: sync: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Clear removes every NRPT rule previously created for tag, and clears iface's
// connection-specific DNS suffix.
func Clear(tag, iface string) error {
	return Sync(tag, iface, nil, nil)
}

// syncSearchDomain sets or clears iface's connection-specific DNS suffix —
// the one per-adapter slot Windows offers for "try appending this suffix to
// an unqualified query," scoped to gravinet's own adapter without touching
// any other adapter's configuration.
func syncSearchDomain(iface string, searchDomains []string) error {
	domains := searchDomainArgs(searchDomains)
	if iface == "" {
		if len(domains) == 0 {
			return nil
		}
		return fmt.Errorf("resolver: sync requires a non-empty interface for search domains")
	}
	if len(domains) == 0 {
		if _, err := powershell(fmt.Sprintf(
			"Set-DnsClient -InterfaceAlias %s -ConnectionSpecificSuffix ''", psQuote(iface),
		)); err != nil {
			return fmt.Errorf("resolver: clear connection-specific suffix on %s: %w", iface, err)
		}
		return nil
	}
	if _, err := powershell(fmt.Sprintf(
		"Set-DnsClient -InterfaceAlias %s -ConnectionSpecificSuffix %s", psQuote(iface), psQuote(domains[0]),
	)); err != nil {
		return fmt.Errorf("resolver: set connection-specific suffix on %s: %w", iface, err)
	}
	if len(domains) > 1 {
		return fmt.Errorf("%w: only one search domain is supported per adapter on Windows; applied %q, %d more not applied (%s)",
			ErrSearchDomainsUnsupported, domains[0], len(domains)-1, strings.Join(domains[1:], ", "))
	}
	return nil
}

// Dump reports the live NRPT rules owned by tag, plus iface's current
// connection-specific DNS suffix — queries PowerShell directly rather than
// reporting anything gravinet remembers creating, so it reflects reality even
// if a rule or suffix was changed by something else since the last Sync.
func Dump(tag, iface string) (string, error) {
	out, err := powershell(fmt.Sprintf(
		"Get-DnsClientNrptRule | Where-Object { $_.Comment -eq %s } | ForEach-Object { \"$($_.Namespace) -> $($_.NameServers -join ',')\" }",
		psQuote(comment(tag)),
	))
	if err != nil {
		return "", fmt.Errorf("resolver: list nrpt rules: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		out = "(no NRPT rules owned by this network)"
	}
	if iface != "" {
		suffix, sErr := powershell(fmt.Sprintf(
			"(Get-DnsClient -InterfaceAlias %s).ConnectionSpecificSuffix", psQuote(iface),
		))
		if sErr == nil {
			suffix = strings.TrimSpace(suffix)
			if suffix == "" {
				suffix = "(none)"
			}
			out += "\n\nsearch domain (connection-specific suffix): " + suffix
		}
	}
	return out, nil
}

// ownedRules returns the set of domains with an existing NRPT rule whose
// Comment matches tag's marker, by asking PowerShell for Namespace+Comment.
func ownedRules(tag string) (map[string]struct{}, error) {
	owned := map[string]struct{}{}
	// One line per rule, tab-separated "namespace\tcomment", so parsing
	// doesn't depend on PowerShell's default table formatting.
	out, err := powershell(
		"Get-DnsClientNrptRule | ForEach-Object { \"$($_.Namespace)`t$($_.Comment)\" }",
	)
	if err != nil {
		return nil, fmt.Errorf("resolver: list nrpt rules: %w", err)
	}
	want := comment(tag)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) != want {
			continue
		}
		ns := strings.TrimPrefix(strings.TrimSpace(parts[0]), ".")
		if ns != "" {
			owned[ns] = struct{}{}
		}
	}
	return owned, nil
}

func addRule(tag, domain string, servers []string) error {
	_, err := powershell(fmt.Sprintf(
		"Add-DnsClientNrptRule -Namespace %s -NameServers %s -Comment %s",
		psQuote("."+domain), psQuoteArray(servers), psQuote(comment(tag)),
	))
	if err != nil {
		return fmt.Errorf("resolver: add nrpt rule for %s: %w", domain, err)
	}
	return nil
}

func removeRule(tag, domain string) error {
	_, err := powershell(fmt.Sprintf(
		"Get-DnsClientNrptRule | Where-Object { $_.Namespace -eq %s -and $_.Comment -eq %s } | Remove-DnsClientNrptRule -Force",
		psQuote("."+domain), psQuote(comment(tag)),
	))
	if err != nil {
		return fmt.Errorf("resolver: remove nrpt rule for %s: %w", domain, err)
	}
	return nil
}

// psQuote wraps s in single quotes for PowerShell, doubling any embedded
// single quote (PowerShell's escaping convention).
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// psQuoteArray builds a PowerShell array literal from vals, e.g.
// psQuoteArray([]string{"10.0.0.1", "192.168.0.2"}) -> `'10.0.0.1','192.168.0.2'`.
// This matters for parameters like -NameServers that bind to a string array:
// a single quoted, comma-joined string (`'10.0.0.1,192.168.0.2'`) is a
// one-element array containing that whole malformed value, not two elements
// — and Add-DnsClientNrptRule accepts it at creation time but the NRPT
// policy store then silently drops it as an invalid server on readback,
// which is why the rule (and its domain) appeared while the server list came
// back empty.
func psQuoteArray(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = psQuote(v)
	}
	return strings.Join(quoted, ",")
}

func powershell(script string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
