//go:build openbsd

// OpenBSD's split-DNS primitive is unbound(8) — the validating, recursive,
// caching resolver that ships in the OpenBSD base system — driven at runtime
// through unbound-control(8)'s forward_add / forward_remove / list_forwards
// commands, each forward zone keeping its own independent server set. This is
// the same mechanism resolver_freebsd.go uses; the only real differences are
// the control binary's name (unbound-control here vs. FreeBSD's
// local-unbound-control wrapper) and how the resolver gets enabled on the
// host (see the installer note below). That per-zone independence matches
// macOS's /etc/resolver and Windows's NRPT, unlike Linux's systemd-resolved,
// which only takes one shared server set per link; see resolver.go's package
// doc.
//
// Readiness: unlike FreeBSD — where local-unbound is off by default and the
// presence of /var/unbound/unbound.conf is a meaningful "has it been set up"
// signal — OpenBSD always ships /var/unbound/etc/unbound.conf and always has
// unbound-control in base, so neither is evidence unbound is actually usable
// for gravinet. What matters here is whether unbound is running with
// remote-control enabled, which the list_forwards probe in liveForwards
// detects directly. So this backend skips any config-file gate and instead
// treats an unreachable control socket as "not set up," surfacing enableHint()
// — install-openbsd.sh --unbound wires all of that up (enables remote-control,
// points resolv.conf at unbound, disables resolvd so it can't clobber it).
//
// As on FreeBSD, an unbound forward zone carries no field to stash gravinet's
// ownership tag, so this backend keeps its own on-disk record (per tag, under
// stateDir) of the domains it applied and reconciles that against unbound's
// live forwards on every Sync — restart-safe (a file on disk, not in-memory
// daemon state), and it can never remove a zone gravinet didn't add itself.
package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// stateDir holds one JSON file per tag recording the domains gravinet last
// applied there. A var (not const) so tests can redirect it to a temp
// directory rather than touching /var/db.
var stateDir = "/var/db/gravinet/resolver"

// controlBin is OpenBSD's base-system unbound control tool. A var so tests can
// redirect it, though Sync/Dump's actual exec calls aren't exercised by this
// repo's Linux test host, same as resolvectl on Linux and powershell.exe on
// Windows.
var controlBin = "unbound-control"

// controlTimeout bounds every unbound-control invocation. The likely failure
// here isn't "command not found" (unbound-control is always in base, caught by
// the exec.LookPath checks below regardless) but a control socket that's
// unreachable — unbound not running, or running without remote-control
// enabled. Without a bound, such a call can hang far longer than a CLI caller
// tolerates, and Sync/Clear run from gravinet's own shutdown sequence
// (clearStaleDNSForwards in cmd/gravinet/main.go): a wedged call there keeps
// the whole process from exiting, and since gravinet runs in the foreground
// under rc.subr (rc_bg=YES), that in turn hangs `rcctl stop gravinet` and,
// transitively, install-openbsd.sh's upgrade-in-place step. Same reasoning as
// resolver_freebsd.go's controlTimeout.
const controlTimeout = 5 * time.Second

// enableHint is the actionable "unbound isn't ready" message, pointing at the
// installer that sets it up (and the manual equivalent). Surfaced whenever a
// real forwarding request can't reach unbound-control's socket.
func enableHint() string {
	return "enable unbound with remote control: run install/install-openbsd.sh --unbound " +
		"(or add 'remote-control: control-enable: yes' + 'control-interface: /var/run/unbound.sock' to " +
		"/var/unbound/etc/unbound.conf, point /etc/resolv.conf at 127.0.0.1, then rcctl enable unbound && rcctl start unbound)"
}

func statePath(tag string) string { return filepath.Join(stateDir, tag+".json") }

// loadState returns the domains gravinet recorded as applied for tag. A
// missing or corrupt state file is treated as "nothing owned yet" rather than
// an error, so a first-ever Sync (or a state file lost some other way) just
// starts clean instead of wedging.
func loadState(tag string) (map[string]struct{}, error) {
	data, err := os.ReadFile(statePath(tag))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("resolver: read state: %w", err)
	}
	var domains []string
	if err := json.Unmarshal(data, &domains); err != nil {
		return map[string]struct{}{}, nil
	}
	owned := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		owned[d] = struct{}{}
	}
	return owned, nil
}

// saveState atomically rewrites tag's state file to exactly domains.
func saveState(tag string, domains map[string]struct{}) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	list := make([]string, 0, len(domains))
	for d := range domains {
		list = append(list, d)
	}
	sort.Strings(list)
	data, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	path := statePath(tag)
	tmp, err := os.CreateTemp(stateDir, ".gravinet-resolver-*")
	if err != nil {
		return os.WriteFile(path, data, 0o644) // fall back to direct write
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
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
		return os.WriteFile(path, data, 0o644)
	}
	return nil
}

// Sync applies one unbound forward zone per entry. tag scopes only gravinet's
// own bookkeeping (see package doc); unbound's forward zones are a single
// global namespace, like /etc/resolver on macOS and NRPT on Windows, so iface
// is unused here but kept for API symmetry with every other platform.
//
// searchDomains is accepted for API symmetry with Linux and Windows but has no
// unbound equivalent: a search domain is a stub-resolver concept (resolv.conf's
// "search" line), not something a forwarding zone can express, and unbound has
// no per-interface notion of one. Routing domains (entries) are still applied
// normally; a non-empty searchDomains only adds a trailing error naming what
// wasn't applied.
func Sync(tag, iface string, entries []Entry, searchDomains []string) error {
	if err := syncRoutingDomains(tag, entries); err != nil {
		return err
	}
	if len(searchDomains) > 0 {
		return fmt.Errorf("%w: search domains are not supported on OpenBSD (%d domain(s) not applied) — "+
			"unbound has no per-interface search-domain mechanism", ErrSearchDomainsUnsupported, len(searchDomains))
	}
	return nil
}

func syncRoutingDomains(tag string, entries []Entry) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("resolver: sync requires a non-empty tag")
	}
	if _, err := exec.LookPath(controlBin); err != nil {
		if len(entries) == 0 {
			return nil // nothing requested, and no unbound to revert either
		}
		return fmt.Errorf("resolver: %s not found on PATH (unexpected on OpenBSD, where it ships in base): %w", controlBin, err)
	}

	prevOwned, err := loadState(tag)
	if err != nil {
		return err
	}

	want := make(map[string][]string, len(entries))
	for _, e := range entries {
		d := normalizeDomain(e.Domain)
		if d == "" {
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

	// One reachability probe up front (bounded by controlTimeout), rather than
	// discovering an unreachable control socket separately on every domain's
	// remove/add call below. Its result is reused to skip a remove for a zone
	// that isn't actually live rather than issuing one that would fail.
	live, err := liveForwards()
	if err != nil {
		if len(entries) == 0 {
			return nil // Clear: unreachable means nothing is live to clear anyway
		}
		// A real request against an unreachable/unconfigured unbound: surface
		// the actionable hint rather than unbound-control's raw stderr.
		return fmt.Errorf("resolver: sync: %v — %s", err, enableHint())
	}

	var errs []string

	// Drop zones we previously owned that are no longer wanted. Best-effort:
	// the goal state excludes them either way, so a failed removal isn't a
	// hard error here — a genuine problem surfaces below when wanted zones
	// fail to (re)add.
	for d := range prevOwned {
		if _, keep := want[d]; !keep {
			if _, isLive := live[d]; isLive {
				_, _ = run("forward_remove", d)
			}
		}
	}

	newOwned := make(map[string]struct{}, len(want))
	for d, servers := range want {
		// Clear any existing zone for d first so forward_add is never rejected
		// as a duplicate — the same remove-then-add discipline
		// resolver_windows.go uses for NRPT rules.
		if _, isLive := live[d]; isLive {
			_, _ = run("forward_remove", d)
		}
		args := append([]string{"forward_add", d}, servers...)
		if _, err := run(args...); err != nil {
			errs = append(errs, fmt.Sprintf("add forward zone for %s: %v", d, err))
			continue
		}
		newOwned[d] = struct{}{}
	}

	if err := saveState(tag, newOwned); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return fmt.Errorf("resolver: sync: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Clear removes every forward zone previously applied for tag.
func Clear(tag, iface string) error {
	return Sync(tag, iface, nil, nil)
}

// Dump reports the live unbound forward zones owned by tag — the current
// server list for each domain gravinet's state recorded, read back from
// unbound-control list_forwards rather than trusted blindly, so a zone changed
// or removed by something else since the last Sync shows up as such instead of
// silently reporting stale bookkeeping. iface is unused (see Sync) but kept
// for API symmetry.
func Dump(tag, iface string) (string, error) {
	owned, err := loadState(tag)
	if err != nil {
		return "", err
	}
	if len(owned) == 0 {
		return "(no unbound forward zones owned by this network)", nil
	}
	live, err := liveForwards()
	if err != nil {
		return "", err
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
		if servers, ok := live[d]; ok {
			fmt.Fprintf(&b, "%s -> %s", d, strings.Join(servers, ", "))
		} else {
			fmt.Fprintf(&b, "%s: (recorded as owned, but no longer present in unbound)", d)
		}
	}
	return b.String(), nil
}

// liveForwards asks unbound-control for its actual current forward zones
// (list_forwards), parsed into domain -> server list.
func liveForwards() (map[string][]string, error) {
	if _, err := exec.LookPath(controlBin); err != nil {
		return nil, fmt.Errorf("resolver: %s not found on PATH (unexpected on OpenBSD, where it ships in base): %w", controlBin, err)
	}
	out, err := run("list_forwards")
	if err != nil {
		return nil, fmt.Errorf("resolver: list forwards: %w", err)
	}
	return parseListForwards(out), nil
}

// parseListForwards parses unbound-control list_forwards output. Each forward
// zone prints as one line: "<zone>. IN forward <addr> [addr ...]". Lines that
// don't match that shape (blank lines, or a "forward off"-style status line
// with no zone) are skipped rather than erroring, so an unexpected line from a
// future unbound version degrades gracefully instead of losing the whole dump.
func parseListForwards(out string) map[string][]string {
	result := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "IN" || fields[2] != "forward" {
			continue
		}
		domain := normalizeDomain(fields[0])
		if domain == "" {
			continue
		}
		result[domain] = fields[3:]
	}
	return result
}

// run invokes unbound-control with args and returns its combined output,
// wrapping any failure with the command and output for context, plus the
// bounded timeout controlTimeout's doc comment explains the need for.
func run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, controlBin, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s %s: timed out after %s waiting on unbound's control socket (running with remote-control enabled? %s)", controlBin, strings.Join(args, " "), controlTimeout, enableHint())
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", controlBin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
