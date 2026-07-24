package service

// Host hostname and default DNS resolver configuration — the backend for the
// web admin's System > Resolver page, recreated from parapet's Resolver tab and
// slotted into the System group ahead of Time (matching parapet's own order).
//
// This is a genuinely different job from the two DNS-shaped things gravinet
// already does, and the whole point of this file is to not step on either:
//
//   - internal/resolver (Mesh > DNS) registers *conditional-forwarding*
//     domains: only queries under a specific mesh domain go anywhere near it,
//     scoped to gravinet's own tun interface wherever the OS has a concept of
//     interface scoping at all. This page sets the host's *default* resolver —
//     what answers everything else. On Linux, macOS, and Windows the two live
//     in mechanisms that are structurally disjoint (global resolved.conf.d
//     drop-in vs. per-link resolvectl state; the default DNS service config vs.
//     per-domain /etc/resolver/<domain> files; the adapter's DNS servers vs.
//     per-domain NRPT rules), so there's no file or API call either page could
//     make that the other would ever touch.
//
//     FreeBSD and OpenBSD are the one place that isn't automatically true, and
//     deserve spelling out: Mesh DNS there only works at all when local-unbound
//     (FreeBSD) or unbound (OpenBSD) is already the box's active resolver
//     (/etc/resolv.conf pointed at 127.0.0.1 — an installer-time step, not
//     something either package does at runtime). If this page just followed
//     parapet's own model and overwrote /etc/resolv.conf's nameserver line with
//     whatever the operator typed here, it would silently redirect every query
//     around unbound and take Mesh DNS's per-domain forwarding down with it —
//     not by touching the same file mesh manages, but by unplugging the thing
//     mesh's forwarding depends on existing at all. So on these two platforms,
//     when unbound is the box's live resolver, this page's DNS-servers field is
//     applied as unbound's OWN default forward zone ("." — everything not more
//     specifically claimed by one of Mesh DNS's own zones) through the exact
//     same *-control tool Mesh DNS uses, and the search-domain field edits only
//     resolv.conf's "search" line, never its "nameserver" line. Only when
//     unbound *isn't* active (so Mesh DNS forwarding isn't functioning on this
//     host either) does this page fall back to owning resolv.conf outright, the
//     same as parapet.
//
//   - internal/hosts (Naming > Hosts) writes a delimited block into the OS
//     hosts file mapping *peer* names to their overlay addresses. It never
//     writes an entry for this node itself, and this page never touches the
//     hosts file at all, so there is no shared file to race on.
//
// One naming subtlety worth knowing about, not a conflict this file needs to
// resolve: gravinet already has a *separate* notion of a node's name —
// config.Hostname, "advertised to peers; OS hostname if empty" — which is read
// from the OS exactly once, at daemon startup, and cached for the process's
// lifetime. Changing the OS hostname here takes effect for the OS immediately,
// but has no effect on what this node advertises to mesh peers until gravinet
// itself restarts (and none at all if config.Hostname is set explicitly). The
// UI says this plainly rather than implying the two are the same knob.
//
// Structure mirrors hosttime.go: a typed read (HostResolver) plus one setter
// per field (SetHostname / SetHostDNS), each (ok, hint), each dispatching on
// runtime.GOOS. The host is the source of truth for everything that has a
// native persistent home (the hostname; resolv.conf; resolved's/NetworkManager's
// own config) — read live, written through, never duplicated into gravinet's
// own config. The one genuine exception is the FreeBSD/OpenBSD unbound "."
// zone: like every zone Mesh DNS itself adds, a *-control forward_add lives
// only in the running daemon's memory, gone on its next restart whether or not
// gravinet restarts too. So — and only for that one case — this file keeps a
// small on-disk breadcrumb of the last-applied server list and ReapplyBoot
// reasserts it once at gravinet startup, the same shape as (though a separate,
// disjoint file from) internal/resolver's own restart-safety bookkeeping.
//
// Per-platform tooling, all optional and all probed before use:
//
//	linux:   hostnamectl for the hostname, falling back to a direct
//	         /etc/hostname write; systemd-resolved's global resolved.conf.d
//	         drop-in when active, else NetworkManager's dns=none + a direct
//	         resolv.conf write, else a direct resolv.conf write.
//	darwin:  scutil --set HostName; networksetup -setdnsservers/
//	         -setsearchdomains on whichever network service currently carries
//	         the default route.
//	freebsd: sysrc hostname=+ a live `hostname` call; local-unbound-control's
//	         "." forward zone when local-unbound is active, else a direct
//	         resolv.conf write.
//	openbsd: /etc/myname + a live `hostname` call; unbound-control's "." zone
//	         when unbound is reachable, else a direct resolv.conf write.
//	windows: Rename-Computer (takes effect on next restart — Windows has no
//	         live hostname change); Set-DnsClientServerAddress /
//	         Set-DnsClient -ConnectionSpecificSuffix on the default-route
//	         adapter.
//
// Every external command is run through exec.Command with separate arguments —
// never a shell string — and every value that reaches one is validated first
// (validHostname / validSearchDomain / validDNSServerAddr).

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"gravinet/internal/tun"
)

// ctxTimeout is a small context.WithTimeout wrapper so every *-control call in
// this file gets the same bounded-timeout treatment without repeating the
// import and call at each site.
func ctxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// ResolverInfo is the live hostname/DNS state of the host, as read from the OS.
type ResolverInfo struct {
	Hostname     string   // current OS hostname (always read live via os.Hostname)
	DNSServers   []string // this host's current default nameservers, as gravinet applies/reads them
	SearchDomain string   // this host's current default search domain
	Manager      string   // which mechanism is in play, for display — e.g. "systemd-resolved", "local-unbound (shared with Mesh DNS)"
	CanHostname  bool
	CanDNS       bool
	Hint         string
}

// HostResolver reads the host's current hostname and default DNS
// configuration. Never fails: anything that can't be determined leaves the
// corresponding Can* false with Hint explaining why.
func HostResolver() ResolverInfo {
	name, _ := os.Hostname()
	info := ResolverInfo{Hostname: name}
	switch runtime.GOOS {
	case "linux":
		readLinuxResolver(&info)
	case "darwin":
		readDarwinResolver(&info)
	case "freebsd":
		readFreeBSDResolver(&info)
	case "openbsd":
		readOpenBSDResolver(&info)
	case "windows":
		readWindowsResolver(&info)
	default:
		info.Hint = "gravinet can't manage resolver settings on " + runtime.GOOS
	}
	return info
}

// SetHostname sets the OS hostname. Unlike SetHostDNS, there is no "clear it"
// state — an empty string is rejected rather than treated as a no-op, because
// there is nothing sensible to revert to; the UI never sends one (an emptied
// field just reverts to the current value on blur, like the Timezone field on
// System > Time).
func SetHostname(name string) (bool, string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, "hostname must not be empty"
	}
	if err := validHostname(name); err != nil {
		return false, err.Error()
	}

	switch runtime.GOOS {
	case "linux":
		return setLinuxHostname(name)
	case "darwin":
		if !haveCmd("scutil") {
			return false, "setting the hostname needs scutil, which isn't on this host"
		}
		if out, err := runInput("scutil", name, "--set", "HostName"); err != nil {
			return false, cmdErr("set the hostname", out, err)
		}
		return true, ""
	case "freebsd":
		if !haveCmd("sysrc") {
			return false, "setting the hostname needs sysrc, which isn't on this host"
		}
		if out, err := exec.Command("sysrc", "hostname="+name).CombinedOutput(); err != nil {
			return false, cmdErr("persist the hostname", out, err)
		}
		if haveCmd("hostname") {
			if out, err := exec.Command("hostname", name).CombinedOutput(); err != nil {
				return true, "persisted for next boot, but couldn't set it live: " + trimOneLine(string(out))
			}
		}
		return true, ""
	case "openbsd":
		if err := writeFilePreserving("/etc/myname", []byte(name+"\n"), 0o644); err != nil {
			return false, "couldn't write /etc/myname: " + err.Error()
		}
		if haveCmd("hostname") {
			if out, err := exec.Command("hostname", name).CombinedOutput(); err != nil {
				return true, "persisted for next boot, but couldn't set it live: " + trimOneLine(string(out))
			}
		}
		return true, ""
	case "windows":
		if !haveCmd("powershell") && !haveCmd("pwsh") {
			return false, "setting the hostname needs PowerShell, which isn't on this host"
		}
		shell := "powershell"
		if !haveCmd(shell) {
			shell = "pwsh"
		}
		out, err := exec.Command(shell, "-NoProfile", "-Command", "Rename-Computer -NewName '"+name+"' -Force").CombinedOutput()
		if err != nil {
			return false, cmdErr("rename this computer", out, err)
		}
		return true, "renamed — this takes effect after this host restarts; Windows has no live hostname change"
	default:
		return false, "gravinet can't set the hostname on " + runtime.GOOS
	}
}

// SetHostDNS sets (or, given an empty servers list, clears) this host's
// default nameservers, and separately sets or clears its default search
// domain. Clearing servers reverts to whatever the host would otherwise use
// (typically DHCP-provided) — it does not disable name resolution the way
// clearing NTP servers on System > Time disables sync, so this carries no
// confirmation prompt.
func SetHostDNS(servers []string, search string) (bool, string) {
	clean := make([]string, 0, len(servers))
	for _, s := range servers {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if err := validDNSServerAddr(s); err != nil {
			return false, err.Error()
		}
		clean = append(clean, s)
	}
	search = strings.TrimSpace(search)
	if search != "" {
		if err := validSearchDomain(search); err != nil {
			return false, err.Error()
		}
	}

	switch runtime.GOOS {
	case "linux":
		return setLinuxDNS(clean, search)
	case "darwin":
		return setDarwinDNS(clean, search)
	case "freebsd":
		return setFreeBSDDNS(clean, search)
	case "openbsd":
		return setOpenBSDDNS(clean, search)
	case "windows":
		return setWindowsDNS(clean, search)
	default:
		return false, "gravinet can't set DNS servers on " + runtime.GOOS
	}
}

// ── linux ───────────────────────────────────────────────────────────────────

// resolvedDropIn is where this page's own systemd-resolved settings live —
// deliberately its own file under resolved.conf.d, never the distro's main
// resolved.conf, so removing it (when both fields go empty) can never remove
// anything gravinet didn't add. A var so tests can redirect it.
var resolvedDropIn = "/etc/systemd/resolved.conf.d/gravinet-system-resolver.conf"

// nmDropIn hands DNS on this host back to a direct resolv.conf write, the same
// technique parapet uses: NetworkManager's own global-dns-domain config syntax
// is version-dependent, while dns=none plus a plain resolv.conf write behaves
// the same everywhere. A var so tests can redirect it.
var nmDropIn = "/etc/NetworkManager/conf.d/gravinet-dns.conf"

// resolvConfPath is /etc/resolv.conf on every unix platform this file
// touches. A var (not a const) so tests can redirect every read and write in
// this file to a temp file instead of the real system resolver config — the
// same reason timesyncdConfPath is a var in hosttime.go.
var resolvConfPath = "/etc/resolv.conf"

// systemResolverStateDir holds the FreeBSD/OpenBSD default-forward-zone
// breadcrumb (see rootForwardStatePath). Deliberately the same parent
// directory internal/resolver's own (unexported) stateDir uses, by
// convention only — this file never reads or writes that package's state,
// only its own disjointly-named files within it. A var so tests can redirect
// it.
var systemResolverStateDir = "/var/db/gravinet/resolver"

func readLinuxResolver(info *ResolverInfo) {
	info.CanHostname = haveCmd("hostnamectl") || true // /etc/hostname write always available
	switch {
	case unitActive("systemd-resolved"):
		info.Manager = "systemd-resolved"
		info.CanDNS = haveCmd("resolvectl")
		kv := resolvedConfSection(resolvedDropIn)
		info.DNSServers = strings.Fields(kv["DNS"])
		info.SearchDomain = kv["Domains"]
	case unitActive("NetworkManager"):
		info.Manager = "NetworkManager (direct resolv.conf)"
		info.CanDNS = true
		info.DNSServers = directiveValues(resolvConfPath, "nameserver")
		info.SearchDomain = firstOrEmpty(directiveValues(resolvConfPath, "search"))
	default:
		info.Manager = resolvConfPath
		info.CanDNS = true
		info.DNSServers = directiveValues(resolvConfPath, "nameserver")
		info.SearchDomain = firstOrEmpty(directiveValues(resolvConfPath, "search"))
	}
	if !info.CanDNS {
		info.Hint = "no DNS management tool found on this host"
	}
}

// resolvedConfSection reads DNS=/Domains= out of gravinet's own drop-in
// directly, the same "read the file I write, don't ask the daemon" discipline
// hosttime.go's timesyncdServers uses for NTP= — the drop-in is the one place
// this page's own setting can live, so it's the one honest answer to "what did
// I last ask for," without needing to parse resolvectl's merged live view.
func resolvedConfSection(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	kv := map[string]string{}
	inTime := false
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			inTime = strings.EqualFold(t, "[Resolve]")
			continue
		}
		if !inTime {
			continue
		}
		if k, v, ok := strings.Cut(t, "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}

func setLinuxHostname(name string) (bool, string) {
	if haveCmd("hostnamectl") {
		if out, err := exec.Command("hostnamectl", "set-hostname", name).CombinedOutput(); err == nil {
			return true, ""
		} else if !haveCmd("hostname") && !fileExists("/etc/hostname") {
			return false, cmdErr("set the hostname", out, err)
		}
	}
	if err := writeFilePreserving("/etc/hostname", []byte(name+"\n"), 0o644); err != nil {
		return false, "couldn't write /etc/hostname: " + err.Error()
	}
	if haveCmd("hostname") {
		runQuiet("hostname", name)
	}
	return true, ""
}

func setLinuxDNS(servers []string, search string) (bool, string) {
	switch {
	case unitActive("systemd-resolved"):
		return setLinuxResolvedDNS(servers, search)
	case unitActive("NetworkManager"):
		if len(servers) == 0 && search == "" {
			os.Remove(nmDropIn)
			runQuiet("nmcli", "general", "reload")
			return unixDirectResolvConf(nil, "")
		}
		if err := os.MkdirAll(filepath.Dir(nmDropIn), 0o755); err != nil {
			return false, "couldn't create " + filepath.Dir(nmDropIn) + ": " + err.Error()
		}
		if err := writeFilePreserving(nmDropIn, []byte("# Managed by [gravinet] (System > Resolver).\n[main]\ndns=none\n"), 0o644); err != nil {
			return false, "couldn't write " + nmDropIn + ": " + err.Error()
		}
		runQuiet("nmcli", "general", "reload")
		return unixDirectResolvConf(servers, search)
	default:
		return unixDirectResolvConf(servers, search)
	}
}

// setLinuxResolvedDNS writes (or, when both fields are empty, removes) this
// page's own resolved.conf.d drop-in, then asks resolved to pick it up. This
// is global [Resolve] config — the fallback servers/search domain used for
// anything not claimed by a more specific per-link setting — structurally
// separate from the per-link `resolvectl dns`/`resolvectl domain` state Mesh
// DNS applies to gravinet's own tun interface (see package doc), so this never
// competes with it.
func setLinuxResolvedDNS(servers []string, search string) (bool, string) {
	if !haveCmd("resolvectl") {
		return false, "DNS changes need resolvectl, which isn't on this host"
	}
	if len(servers) == 0 && search == "" {
		os.Remove(resolvedDropIn)
	} else {
		if err := os.MkdirAll(filepath.Dir(resolvedDropIn), 0o755); err != nil {
			return false, "couldn't create " + filepath.Dir(resolvedDropIn) + ": " + err.Error()
		}
		var b strings.Builder
		b.WriteString("# Managed by [gravinet] (System > Resolver).\n[Resolve]\n")
		if len(servers) > 0 {
			fmt.Fprintf(&b, "DNS=%s\n", strings.Join(servers, " "))
		}
		if search != "" {
			fmt.Fprintf(&b, "Domains=%s\n", search)
		}
		if err := writeFilePreserving(resolvedDropIn, []byte(b.String()), 0o644); err != nil {
			return false, "couldn't write " + resolvedDropIn + ": " + err.Error()
		}
	}
	if out, err := exec.Command("resolvectl", "reload").CombinedOutput(); err != nil {
		runQuiet("systemctl", "reload-or-restart", "systemd-resolved")
		_ = out
	}
	return true, ""
}

// ── darwin ──────────────────────────────────────────────────────────────────

func readDarwinResolver(info *ResolverInfo) {
	info.CanHostname = haveCmd("scutil")
	svc, err := defaultServiceName()
	if err != nil {
		info.Manager = "networksetup"
		info.Hint = err.Error()
		return
	}
	info.Manager = "networksetup (service \"" + svc + "\")"
	info.CanDNS = true
	if out := cmdOut("networksetup", "-getdnsservers", svc); !strings.Contains(out, "aren't any") {
		for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				info.DNSServers = append(info.DNSServers, ln)
			}
		}
	}
	if out := cmdOut("networksetup", "-getsearchdomains", svc); !strings.Contains(out, "aren't any") {
		info.SearchDomain = strings.TrimSpace(strings.Split(strings.TrimSpace(out), "\n")[0])
	}
}

func setDarwinDNS(servers []string, search string) (bool, string) {
	if !haveCmd("networksetup") {
		return false, "DNS changes need networksetup, which isn't on this host"
	}
	svc, err := defaultServiceName()
	if err != nil {
		return false, err.Error()
	}
	// networksetup's documented convention for "go back to DHCP-provided": pass
	// the single literal argument "Empty" rather than no arguments at all.
	dnsArg := []string{"Empty"}
	if len(servers) > 0 {
		dnsArg = servers
	}
	if out, err := exec.Command("networksetup", append([]string{"-setdnsservers", svc}, dnsArg...)...).CombinedOutput(); err != nil {
		return false, cmdErr("set DNS servers on \""+svc+"\"", out, err)
	}
	searchArg := []string{"Empty"}
	if search != "" {
		searchArg = []string{search}
	}
	if out, err := exec.Command("networksetup", append([]string{"-setsearchdomains", svc}, searchArg...)...).CombinedOutput(); err != nil {
		return false, cmdErr("set the search domain on \""+svc+"\"", out, err)
	}
	return true, ""
}

// defaultServiceName finds the macOS network *service* name (as networksetup
// names it, e.g. "Wi-Fi") that currently carries the default route, since
// -setdnsservers/-setsearchdomains take a service name, not an interface.
// Deliberately re-resolved on every call rather than cached: which service is
// "the" default one can change (Wi-Fi drops, Ethernet takes over), and this is
// a one-shot apply, not a maintained subscription — the same "apply what's true
// right now" model as everything else on this page.
func defaultServiceName() (string, error) {
	gw, err := sysDefaultGatewayFn(syscall.AF_INET, 0)
	if err != nil || !gw.Addr.IsValid() {
		return "", fmt.Errorf("could not determine this host's default network route")
	}
	iface, err := net.InterfaceByIndex(int(gw.IfIndex))
	if err != nil {
		return "", fmt.Errorf("could not resolve the default route's interface: %v", err)
	}
	ports, err := hardwarePortsFn()
	if err != nil {
		return "", err
	}
	return resolveServiceName(iface.Name, ports)
}

// resolveServiceName is the pure lookup half of defaultServiceName, split out
// so the "no matching service" error path (the case that matters most —
// gravinet's own tun interface holding the default route during full-tunnel
// mode is exactly the shape that hits it) is testable without a real gateway
// read or a real networksetup.
func resolveServiceName(ifaceName string, ports map[string]string) (string, error) {
	if svc, ok := ports[ifaceName]; ok {
		return svc, nil
	}
	return "", fmt.Errorf("the default route is on %s, which isn't a network service networksetup manages "+
		"(likely a VPN or tunnel interface — possibly gravinet's own) — connect a physical network, or set DNS "+
		"on the intended service by hand with networksetup", ifaceName)
}

// hardwarePortsFn indirects hardwarePorts so defaultServiceName is testable
// without a real networksetup binary, the same reasoning as sysDefaultGatewayFn.
var hardwarePortsFn = hardwarePorts

// hardwarePorts maps a BSD interface name ("en0") to the network service name
// networksetup knows it by ("Wi-Fi"), parsed from -listallhardwareports'
// "Hardware Port: X\nDevice: Y\n" blocks.
func hardwarePorts() (map[string]string, error) {
	if !haveCmd("networksetup") {
		return nil, fmt.Errorf("could not enumerate network services: networksetup isn't on this host")
	}
	return parseHardwarePorts(cmdOut("networksetup", "-listallhardwareports")), nil
}

// parseHardwarePorts is the pure parsing half of hardwarePorts, split out so
// it's testable against fixed sample output without a real networksetup
// binary or a real Mac.
func parseHardwarePorts(out string) map[string]string {
	ports := map[string]string{}
	var port string
	for _, ln := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(ln, "Hardware Port: "); ok {
			port = strings.TrimSpace(p)
			continue
		}
		if d, ok := strings.CutPrefix(ln, "Device: "); ok && port != "" {
			ports[strings.TrimSpace(d)] = port
			port = ""
		}
	}
	return ports
}

// sysDefaultGatewayFn indirects tun.DefaultGateway so tests can swap it
// without a real routing table, the same pattern internal/mesh's
// defaultGatewayFn uses.
var sysDefaultGatewayFn = tun.DefaultGateway

// ── freebsd ─────────────────────────────────────────────────────────────────

func readFreeBSDResolver(info *ResolverInfo) {
	info.CanHostname = haveCmd("sysrc")
	if unboundUsable("local-unbound-control", unboundConfPathFreeBSD) {
		info.Manager = "local-unbound (shared with Mesh DNS)"
		info.CanDNS = true
		info.DNSServers = liveRootForward("local-unbound-control")
		info.SearchDomain = firstOrEmpty(directiveValues(resolvConfPath, "search"))
		return
	}
	info.Manager = resolvConfPath
	info.CanDNS = true
	info.DNSServers = directiveValues(resolvConfPath, "nameserver")
	info.SearchDomain = firstOrEmpty(directiveValues(resolvConfPath, "search"))
}

// unboundConfPathFreeBSD mirrors internal/resolver's own unboundConfigPath —
// kept as a separate constant (not imported) because that one is unexported in
// a different package, but the value and the reasoning are identical: its
// presence is what local-unbound-setup(8) leaves behind once local-unbound has
// ever been brought up.
var unboundConfPathFreeBSD = "/var/unbound/unbound.conf"

func setFreeBSDDNS(servers []string, search string) (bool, string) {
	if unboundUsable("local-unbound-control", unboundConfPathFreeBSD) {
		if ok, hint := setRootForward("local-unbound-control", "freebsd", servers); !ok {
			return false, hint
		}
		if err := setSearchLineOnly(search); err != nil {
			return true, "default servers set, but the search domain could not be: " + err.Error()
		}
		return true, ""
	}
	return unixDirectResolvConf(servers, search)
}

// ── openbsd ─────────────────────────────────────────────────────────────────

func readOpenBSDResolver(info *ResolverInfo) {
	info.CanHostname = true // /etc/myname always writable
	if unboundUsable("unbound-control", "") {
		info.Manager = "unbound (shared with Mesh DNS)"
		info.CanDNS = true
		info.DNSServers = liveRootForward("unbound-control")
		info.SearchDomain = firstOrEmpty(directiveValues(resolvConfPath, "search"))
		return
	}
	info.Manager = resolvConfPath
	info.CanDNS = true
	info.DNSServers = directiveValues(resolvConfPath, "nameserver")
	info.SearchDomain = firstOrEmpty(directiveValues(resolvConfPath, "search"))
}

func setOpenBSDDNS(servers []string, search string) (bool, string) {
	if unboundUsable("unbound-control", "") {
		if ok, hint := setRootForward("unbound-control", "openbsd", servers); !ok {
			return false, hint
		}
		if err := setSearchLineOnly(search); err != nil {
			return true, "default servers set, but the search domain could not be: " + err.Error()
		}
		return true, ""
	}
	return unixDirectResolvConf(servers, search)
}

// ── freebsd/openbsd shared: cooperating with Mesh DNS's unbound ────────────

// unboundControlTimeout bounds every *-control invocation from this file, the
// same bound (and the same reasoning — a wedged call must never hang
// gravinet's own shutdown or an upgrade's apply step) as internal/resolver's
// own controlTimeout for these platforms.
const unboundControlTimeout = 5 * time.Second

// unboundUsable reports whether local-unbound/unbound is actually reachable
// for forward-zone control right now — a real probe (list_forwards), not just
// "is the binary on PATH" or "does a config file exist," matching how
// internal/resolver itself decides this for the exact same daemon. confPath is
// checked first on FreeBSD, where its absence is a fast, cheap, and much more
// common signal than a control-socket timeout (see internal/resolver's own
// unboundConfigured); OpenBSD has no such file (ships unbound.conf outright
// per install) so confPath is passed empty there and skipped.
func unboundUsable(bin, confPath string) bool {
	if !haveCmd(bin) {
		return false
	}
	if confPath != "" && !fileExists(confPath) {
		return false
	}
	ctx, cancel := ctxTimeout(unboundControlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "list_forwards")
	return cmd.Run() == nil
}

// liveRootForward reads the server list currently on the "." forward zone —
// the live daemon state, not gravinet's own bookkeeping, so a zone changed or
// cleared by something else since the last apply shows up as such.
func liveRootForward(bin string) []string {
	ctx, cancel := ctxTimeout(unboundControlTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "list_forwards").CombinedOutput()
	if err != nil {
		return nil
	}
	return parseRootForward(string(out))
}

// parseRootForward is the pure parsing half of liveRootForward, split out so
// it's testable against fixed sample list_forwards output without a real
// *-control binary or a running unbound. Same line shape internal/resolver's
// own parseListForwards parses — one line per zone, "<domain> IN forward
// <addr> <addr> ..." — so this looks for the line whose zone is the root
// ("." — the only zone name that normalizes to nothing once its trailing dot
// is stripped, the same emptiness check normalizeDomain uses in
// internal/resolver, reimplemented here rather than imported since that
// package's helper is unexported).
func parseRootForward(out string) []string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "IN" || fields[2] != "forward" {
			continue
		}
		if strings.TrimSuffix(fields[0], ".") == "" {
			return fields[3:]
		}
	}
	return nil
}

// setRootForward replaces the "." forward zone's server list, then persists
// (or, if servers is empty, clears) the on-disk breadcrumb ReapplyBoot uses —
// the zone itself lives only in the running daemon's memory (see package
// doc), so this is what survives a gravinet restart or an out-of-band unbound
// restart between them.
func setRootForward(bin, platform string, servers []string) (bool, string) {
	ctx, cancel := ctxTimeout(unboundControlTimeout)
	defer cancel()
	// Remove-then-add, unconditionally: the same discipline internal/resolver's
	// own FreeBSD/OpenBSD backends use for every zone they manage, so
	// forward_add is never rejected as a duplicate of gravinet's own prior
	// state. "." is reserved for this page's exclusive use (documented in the
	// package comment); an operator's own manual "." zone, if one somehow
	// existed, would be replaced the same way a mesh domain zone would be.
	exec.CommandContext(ctx, bin, "forward_remove", ".").Run()
	if len(servers) > 0 {
		args := append([]string{"forward_add", "."}, servers...)
		ctx2, cancel2 := ctxTimeout(unboundControlTimeout)
		defer cancel2()
		if out, err := exec.CommandContext(ctx2, bin, args...).CombinedOutput(); err != nil {
			return false, cmdErr("add the default forward zone", out, err)
		}
	}
	if err := saveRootForwardState(platform, servers); err != nil {
		// The zone itself is live either way; only the restart-survival
		// breadcrumb failed to write, so say so without calling this a
		// failure to apply.
		return true, "applied, but couldn't record it for reapplication after a restart: " + err.Error()
	}
	return true, ""
}

// rootForwardStatePath is where the last-applied default-forward server list
// is recorded, one file per platform since only one of the two ever runs.
// Deliberately its own file, not internal/resolver's own (unexported, and
// per-network-tag) state directory contents — same parent directory by
// convention, disjoint reserved name ("__system__", which can never collide
// with a 16-hex-digit network tag) so a listing of that directory makes the
// split obvious rather than needing a comment to explain it.
func rootForwardStatePath(platform string) string {
	return filepath.Join(systemResolverStateDir, "__system__-"+platform+".json")
}

func saveRootForwardState(platform string, servers []string) error {
	path := rootForwardStatePath(platform)
	if len(servers) == 0 {
		os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(servers)
	if err != nil {
		return err
	}
	return writeFilePreserving(path, b, 0o644)
}

func loadRootForwardState(platform string) []string {
	b, err := os.ReadFile(rootForwardStatePath(platform))
	if err != nil {
		return nil
	}
	var servers []string
	if json.Unmarshal(b, &servers) != nil {
		return nil
	}
	return servers
}

// ReapplyBoot reasserts the FreeBSD/OpenBSD default forward zone from its
// on-disk breadcrumb. A no-op everywhere else, and a no-op here too if nothing
// was ever recorded. Meant to be called once at gravinet startup, alongside
// main.go's clearStaleHostsBlocks/clearStaleDNSForwards — unlike those two,
// which *clear* stale state, this one *reasserts* desired state, because
// unlike a per-network mesh forward (which is only ever meaningful while this
// exact gravinet process is running and its mesh is up), the default resolver
// a host should use is a standing fact about the host that both a gravinet
// restart and an unrelated unbound restart can otherwise silently drop.
func ReapplyBoot() {
	switch runtime.GOOS {
	case "freebsd":
		if servers := loadRootForwardState("freebsd"); len(servers) > 0 && unboundUsable("local-unbound-control", unboundConfPathFreeBSD) {
			setRootForward("local-unbound-control", "freebsd", servers)
		}
	case "openbsd":
		if servers := loadRootForwardState("openbsd"); len(servers) > 0 && unboundUsable("unbound-control", "") {
			setRootForward("unbound-control", "openbsd", servers)
		}
	}
}

// setSearchLineOnly rewrites just resolv.conf's "search" line, leaving every
// other line — critically, the "nameserver 127.0.0.1" line pointing at
// unbound — exactly as it was. This is what keeps the search-domain field from
// ever being able to disturb the very thing Mesh DNS forwarding depends on.
func setSearchLineOnly(search string) error {
	var replacement []string
	if search != "" {
		replacement = []string{"search " + search}
	}
	return setDirectiveLines(resolvConfPath, []string{"search"}, replacement)
}

// ── windows ─────────────────────────────────────────────────────────────────

func readWindowsResolver(info *ResolverInfo) {
	info.CanHostname = haveCmd("powershell") || haveCmd("pwsh")
	idx, name, err := defaultAdapterIndex()
	if err != nil {
		info.Manager = "Set-DnsClientServerAddress"
		info.Hint = err.Error()
		return
	}
	info.Manager = "Set-DnsClientServerAddress (adapter \"" + name + "\")"
	info.CanDNS = true
	shell := psShell()
	if shell == "" {
		info.CanDNS = false
		info.Hint = "DNS changes need PowerShell, which isn't on this host"
		return
	}
	out := cmdOut(shell, "-NoProfile", "-Command",
		fmt.Sprintf("(Get-DnsClientServerAddress -InterfaceIndex %d -AddressFamily IPv4).ServerAddresses -join ' '", idx))
	info.DNSServers = strings.Fields(out)
	out = cmdOut(shell, "-NoProfile", "-Command",
		fmt.Sprintf("(Get-DnsClient -InterfaceIndex %d).ConnectionSpecificSuffix", idx))
	info.SearchDomain = strings.TrimSpace(out)
}

func setWindowsDNS(servers []string, search string) (bool, string) {
	shell := psShell()
	if shell == "" {
		return false, "DNS changes need PowerShell, which isn't on this host"
	}
	idx, name, err := defaultAdapterIndex()
	if err != nil {
		return false, err.Error()
	}
	var dnsCmd string
	if len(servers) > 0 {
		quoted := make([]string, len(servers))
		for i, s := range servers {
			quoted[i] = "'" + s + "'"
		}
		dnsCmd = fmt.Sprintf("Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses (%s)", idx, strings.Join(quoted, ","))
	} else {
		dnsCmd = fmt.Sprintf("Set-DnsClientServerAddress -InterfaceIndex %d -ResetServerAddresses", idx)
	}
	if out, err := exec.Command(shell, "-NoProfile", "-Command", dnsCmd).CombinedOutput(); err != nil {
		return false, cmdErr("set DNS servers on \""+name+"\"", out, err)
	}
	suffixCmd := fmt.Sprintf("Set-DnsClient -InterfaceIndex %d -ConnectionSpecificSuffix '%s'", idx, search)
	if out, err := exec.Command(shell, "-NoProfile", "-Command", suffixCmd).CombinedOutput(); err != nil {
		return false, cmdErr("set the search domain on \""+name+"\"", out, err)
	}
	return true, ""
}

// defaultAdapterIndex finds the Windows interface index currently carrying the
// default route, the same "apply to whatever's true right now" approach as
// defaultServiceName on macOS, using PowerShell's own interface index directly
// rather than needing a separate name-to-alias lookup.
func defaultAdapterIndex() (int, string, error) {
	gw, err := sysDefaultGatewayFn(syscall.AF_INET, 0)
	if err != nil || !gw.Addr.IsValid() {
		return 0, "", fmt.Errorf("could not determine this host's default network route")
	}
	iface, err := net.InterfaceByIndex(int(gw.IfIndex))
	if err != nil {
		return 0, "", fmt.Errorf("could not resolve the default route's interface: %v", err)
	}
	return int(gw.IfIndex), iface.Name, nil
}

func psShell() string {
	if haveCmd("powershell") {
		return "powershell"
	}
	if haveCmd("pwsh") {
		return "pwsh"
	}
	return ""
}

// ── shared: owning /etc/resolv.conf outright ────────────────────────────────

// resolvMarker is written as the first line of every resolv.conf gravinet
// writes outright, so a later call — including after a restart, with no
// in-memory record of what was previously written — can tell whether it's
// safe to remove the file entirely (both fields cleared) versus leaving alone
// a file that predates gravinet or that some other tool wrote.
const resolvMarker = "# Managed by [gravinet] (System > Resolver). Do not edit — changes here will be overwritten.\n"

// unixDirectResolvConf is the fallback used on every unix platform here when
// nothing more specific (systemd-resolved, NetworkManager, an active
// local-unbound/unbound) is managing DNS: gravinet owns /etc/resolv.conf
// outright, the same as parapet's own direct_resolv_conf. Never called for a
// host where Mesh DNS's unbound dependency could be live — see the package
// doc — so there is nothing here that needs to spare a "nameserver 127.0.0.1"
// line the way setSearchLineOnly does.
func unixDirectResolvConf(servers []string, search string) (bool, string) {
	path := resolvConfPath
	if len(servers) == 0 && search == "" {
		owned, _ := os.ReadFile(path)
		if strings.HasPrefix(string(owned), resolvMarker) {
			os.Remove(path)
		}
		return true, ""
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		os.Remove(path)
	}
	var b strings.Builder
	b.WriteString(resolvMarker)
	if search != "" {
		fmt.Fprintf(&b, "search %s\n", search)
	}
	for _, s := range servers {
		fmt.Fprintf(&b, "nameserver %s\n", s)
	}
	if err := writeFilePreserving(path, []byte(b.String()), 0o644); err != nil {
		return false, "couldn't write " + path + ": " + err.Error()
	}
	return true, ""
}

// ── validation ──────────────────────────────────────────────────────────────

// validHostname mirrors parapet's valid_hostname exactly: RFC-1123-style,
// dot-separated labels of letters/digits/hyphens, no empty label, no
// leading/trailing hyphen in a label, each label ≤63 bytes, the whole name
// ≤253 bytes. Ported rather than reused because parapet's is Rust; the rule
// itself is deliberately identical so the same hostname that validates in
// parapet validates here.
func validHostname(s string) error {
	if s == "" || len(s) > 253 {
		return fmt.Errorf("hostname must be 1-253 characters")
	}
	for _, label := range strings.Split(s, ".") {
		if !validLabel(label) {
			return fmt.Errorf("invalid hostname: %q", s)
		}
	}
	return nil
}

// validSearchDomain uses the same label rules as validHostname — parapet's own
// valid_search_domain is defined as literally calling valid_hostname, and a
// single label with no dots (e.g. "internal") already satisfies that rule
// without any special-casing.
func validSearchDomain(s string) error {
	if err := validHostname(s); err != nil {
		return fmt.Errorf("invalid search domain: %q", s)
	}
	return nil
}

func validLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// validDNSServerAddr requires a real IP address — matching parapet's own
// resolver::validate, which parses each entry as std::net::IpAddr and refuses
// anything that isn't one. A hostname here (unlike System > Time's NTP
// servers, which legitimately are hostnames most of the time) would be
// circular: the whole point of this field is to say how names get resolved in
// the first place.
func validDNSServerAddr(s string) error {
	if _, err := netip.ParseAddr(s); err != nil {
		return fmt.Errorf("invalid DNS server address: %q", s)
	}
	return nil
}

// ── small helpers ───────────────────────────────────────────────────────────

func firstOrEmpty(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// runInput runs a command with input piped to stdin — used for scutil, whose
// --set subcommands read the new value from stdin rather than argv, keeping a
// hostname (already validated, but see the general discipline this whole file
// follows) off the process's command-line arguments entirely.
func runInput(bin, input string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(input + "\n")
	return cmd.CombinedOutput()
}
