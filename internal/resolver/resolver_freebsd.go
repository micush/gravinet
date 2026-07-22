//go:build freebsd

// FreeBSD's split-DNS primitive is local-unbound, the caching resolver
// FreeBSD ships in the base system (see the "shell out to the base-system
// tool" discipline explained in ipfwd_freebsd.go and tun_freebsd.go — the
// same reasoning applies here: no reliable /proc-style file interface to
// read/write directly, so go through the CLI FreeBSD already provides).
// Once `sysrc local_unbound_enable=YES` and `service local_unbound start`
// have brought it up, local-unbound-control(8) — a thin base-system wrapper
// around unbound-control(8), talking to local-unbound's control socket — can
// add or remove forward zones on the running daemon with
// forward_add/forward_remove, each zone keeping its own independent server
// set. That per-zone independence matches macOS's /etc/resolver and
// Windows's NRPT (unlike Linux's systemd-resolved, which only takes one
// shared server set per link); see resolver.go's package doc.
//
// The one place FreeBSD falls short of the other three: a forward zone is
// just a name and an address list, with nowhere to stash gravinet's
// ownership tag the way the marker line does on macOS or the -Comment does
// on Windows. So instead of reading ownership back out of local-unbound's
// own state, this backend keeps its own record of which domains it applied
// for tag under stateDir. That's still restart-safe in the sense the package
// doc asks for — it's a file on disk, not in-memory daemon state that a
// gravinet restart would lose — and every Sync cross-checks that record
// against local-unbound's actual live forwards (via list_forwards) before
// removing anything, so a stale or lost record can at worst leave gravinet
// forgetting to clean up a zone; it can never make Sync remove a zone
// gravinet didn't add itself.
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

// controlBin is the base-system wrapper for local-unbound's control socket.
// A var so tests could redirect it, though Sync/Dump's actual exec calls
// aren't exercised by this repo's Linux test host, same as resolvectl on
// Linux and powershell.exe on Windows.
var controlBin = "local-unbound-control"

// unboundConfigPath is where local-unbound-setup(8) writes local-unbound's
// config once it's ever been set up (via `service local_unbound start`, or
// interactively during bsdinstall). local-unbound-control and the base
// binaries it wraps ship in every stock FreeBSD install regardless of
// whether local_unbound has ever been enabled, so exec.LookPath(controlBin)
// succeeding is not evidence local-unbound is actually configured — only
// that the wrapper binary exists. A var so tests can redirect it.
var unboundConfigPath = "/var/unbound/unbound.conf"

// unboundConfigured reports whether local-unbound has ever been set up on
// this host. A host that installed FreeBSD without opting into the local
// resolver (or that manages DNS via resolvconf(8) pointing straight at
// upstream servers, as on a stock install) never has this file: local-
// unbound-control then fails fast with its own "could not open ...
// unbound.conf" error rather than the control-socket timeout controlTimeout
// guards against below. That's a distinct, common failure mode from "enabled
// but not started" and deserves its own actionable message instead of
// surfacing unbound-control's raw stderr.
func unboundConfigured() bool {
	_, err := os.Stat(unboundConfigPath)
	return err == nil
}

// errNotConfigured is returned when local-unbound-control is on PATH (so the
// exec.LookPath checks below pass) but local-unbound has never actually been
// set up on this host.
func errNotConfigured() error {
	return fmt.Errorf("resolver: local-unbound is not configured on this host (this FreeBSD install manages DNS another way, e.g. resolvconf pointing directly at upstream servers) — enable it: sysrc local_unbound_enable=YES && service local_unbound start")
}

// controlTimeout bounds every local-unbound-control invocation. local-unbound
// ships in the base system but is off by default, so the most likely failure
// here isn't "command not found" (caught by the exec.LookPath checks below)
// but a control socket that's present yet unreachable — local-unbound
// installed, never enabled. Without a bound, that can make a single call
// hang far longer than a CLI caller would tolerate, and Sync/Clear run from
// gravinet's own shutdown sequence (clearStaleDNSForwards in
// cmd/gravinet/main.go): a wedged call there doesn't just fail slowly, it
// keeps the whole process from exiting. daemon(8) is watching that process
// and won't itself exit (or release the rc.d pidfile) until it does, and
// rc.subr's `service gravinet stop` waits indefinitely for that pidfile's
// process to disappear — so an unbounded call here can hang not just Sync,
// but `service gravinet stop` and, transitively, install-freebsd.sh's
// upgrade-in-place step.
const controlTimeout = 5 * time.Second

func statePath(tag string) string { return filepath.Join(stateDir, tag+".json") }

// loadState returns the domains gravinet recorded as applied for tag. A
// missing or corrupt state file is treated as "nothing owned yet" rather
// than an error, so a first-ever Sync (or a state file lost some other way)
// just starts clean instead of wedging.
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

// Sync applies one local-unbound forward zone per entry. tag scopes only
// gravinet's own bookkeeping (see package doc above); local-unbound's forward
// zones are a single global namespace, like /etc/resolver on macOS and NRPT
// on Windows, so iface is unused here but kept for API symmetry with every
// other platform.
//
// searchDomains is accepted for API symmetry with Linux and Windows, but has
// no FreeBSD/local-unbound equivalent: a search domain is a stub-resolver
// concept (resolv.conf's "search" line), not something a forwarding zone can
// express, and local-unbound has no per-interface notion of one anyway.
// Routing domains (entries) are still applied normally; a non-empty
// searchDomains only adds a trailing error naming what wasn't applied.
func Sync(tag, iface string, entries []Entry, searchDomains []string) error {
	if err := syncRoutingDomains(tag, entries); err != nil {
		return err
	}
	if len(searchDomains) > 0 {
		return fmt.Errorf("%w: search domains are not supported on FreeBSD (%d domain(s) not applied) — "+
			"local-unbound has no per-interface search-domain mechanism", ErrSearchDomainsUnsupported, len(searchDomains))
	}
	return nil
}

func syncRoutingDomains(tag string, entries []Entry) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("resolver: sync requires a non-empty tag")
	}
	if _, err := exec.LookPath(controlBin); err != nil {
		if len(entries) == 0 {
			return nil // nothing requested, and nothing local-unbound to revert either
		}
		return fmt.Errorf("resolver: %s not found on PATH (enable local-unbound: sysrc local_unbound_enable=YES && service local_unbound start): %w", controlBin, err)
	}
	if !unboundConfigured() {
		if len(entries) == 0 {
			return nil // nothing requested, and nothing local-unbound to revert either
		}
		return errNotConfigured()
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

	// One reachability probe up front (bounded by controlTimeout — see its
	// doc comment), rather than discovering an unreachable control socket
	// separately on every domain's remove/add call below. Its result is
	// reused to skip a remove call entirely for a zone that isn't actually
	// live, rather than issuing one anyway and letting it fail.
	live, err := liveForwards()
	if err != nil {
		if len(entries) == 0 {
			return nil // Clear: unreachable means nothing is live to clear anyway
		}
		return fmt.Errorf("resolver: sync: %w", err)
	}

	var errs []string

	// Drop zones we previously owned that are no longer wanted. Best-effort:
	// the goal state doesn't include them either way, so a removal that
	// fails isn't treated as a hard error here — a genuine problem (as
	// opposed to the zone already being gone) will surface below anyway,
	// when the wanted zones fail to (re)add.
	for d := range prevOwned {
		if _, keep := want[d]; !keep {
			if _, isLive := live[d]; isLive {
				_, _ = run("forward_remove", d)
			}
		}
	}

	newOwned := make(map[string]struct{}, len(want))
	for d, servers := range want {
		// Clear any existing zone for d first so forward_add is never
		// rejected as a duplicate — the same remove-then-add discipline
		// resolver_windows.go uses for NRPT rules, and just as cheap to
		// recreate here as a rule is there.
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

// Dump reports the live local-unbound forward zones owned by tag — the
// current server list for each domain gravinet's state recorded, read back
// from local-unbound-control list_forwards rather than trusted blindly, so a
// zone changed or removed by something else since the last Sync shows up
// as such instead of silently reporting stale bookkeeping. iface is unused
// (see Sync's doc comment) but kept for API symmetry.
func Dump(tag, iface string) (string, error) {
	owned, err := loadState(tag)
	if err != nil {
		return "", err
	}
	if len(owned) == 0 {
		return "(no local-unbound forward zones owned by this network)", nil
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
			fmt.Fprintf(&b, "%s: (recorded as owned, but no longer present in local-unbound)", d)
		}
	}
	return b.String(), nil
}

// liveForwards asks local-unbound-control for its actual current forward
// zones (list_forwards), parsed into domain -> server list.
func liveForwards() (map[string][]string, error) {
	if _, err := exec.LookPath(controlBin); err != nil {
		return nil, fmt.Errorf("resolver: %s not found on PATH (enable local-unbound: sysrc local_unbound_enable=YES && service local_unbound start): %w", controlBin, err)
	}
	if !unboundConfigured() {
		return nil, errNotConfigured()
	}
	out, err := run("list_forwards")
	if err != nil {
		return nil, fmt.Errorf("resolver: list forwards: %w", err)
	}
	return parseListForwards(out), nil
}

// parseListForwards parses local-unbound-control list_forwards output. Each
// forward zone prints as one line: "<zone>. IN forward <addr> [addr ...]".
// Lines that don't match that shape (blank lines, or a "forward off"-style
// status line with no zone at all) are skipped rather than erroring, so an
// unexpected extra line from a future unbound version degrades gracefully
// instead of losing the whole dump.
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

// run invokes local-unbound-control with args and returns its combined
// output, wrapping any failure with the command and output for context —
// the same shape as resolver_linux.go's run helper, plus the bounded timeout
// controlTimeout's doc comment explains the need for.
func run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, controlBin, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s %s: timed out after %s waiting on local-unbound's control socket (enabled but not responding? sysrc local_unbound_enable=YES && service local_unbound restart)", controlBin, strings.Join(args, " "), controlTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", controlBin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
