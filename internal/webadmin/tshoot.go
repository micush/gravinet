package webadmin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Troubleshooting bundle: one download containing everything needed to
// diagnose this node without a back-and-forth.
//
// The motivating experience was a day spent reconstructing a mesh's state one
// screenshot at a time — peer tables cropped above the row that mattered,
// routes from one node but not its peer, drop counters never checked because
// nobody knew they existed. Every question asked in that session is answered
// here in a single file, from every network, with the fields that actually
// discriminate rather than the ones that fit in a table column.
//
// Two rules govern what goes in:
//
//   - Structured sections are emitted as indented JSON of the source structs
//     rather than hand-formatted. Hand-formatting silently omits fields added
//     later, which is exactly how a diagnostic goes stale; marshalling the
//     struct means a new field appears in the bundle the day it is added.
//   - Secrets never appear. The config file is included because misconfiguration
//     is a leading cause of the problems this exists to diagnose, but every
//     key-bearing line is redacted on the way out, and the redaction is
//     allow-list-free: anything whose key name looks secret is replaced, so a
//     newly added secret field is redacted by default rather than leaked by
//     default.
const tshootMaxLogBytes = 4 << 20 // tail of the log; enough for hours of context

// secretish matches configuration keys whose values must never leave the node.
// Deliberately broad: a false positive costs a redacted line in a diagnostic,
// a false negative leaks a private key.
var secretish = regexp.MustCompile(`(?i)(key|secret|password|passwd|token|credential|psk|private|seed_?phrase|hash|salt)`)

// extraGlobalICMPSysctls are the echo-suppression and rate-limit knobs that
// don't fit handleTshoot's per-scope conf/<scope>/ collection loop below
// (Linux has no per-interface variant of either — both are process-wide).
// Kept as a package-level var, not inlined into that loop, specifically so
// TestExtraGlobalICMPSysctlsPaths can assert the exact paths directly:
// diagnosing a real "peer receives mcfed's ICMPv6 echo request on its own
// mesh interface — confirmed via a live packet capture — and never replies"
// report, with the host firewall (nft/ip6tables, confirmed empty) and
// gravinet's own per-network firewall (confirmed disabled in the CONFIG
// section) both cleanly ruled out by the rest of a real tshoot bundle,
// echo_ignore_all was the one remaining explanation this tool couldn't
// actually confirm or rule out, because it wasn't being collected at all.
// The two echo_ignore_all paths are NOT symmetric in shape — IPv6's is
// nested under an icmp/ subdirectory, IPv4's isn't — confirmed against the
// kernel patch that introduced the IPv6 knob (it didn't exist before 2018,
// added specifically because no IPv6 equivalent of the long-standing IPv4
// one did) rather than assumed from the IPv4 name with a substitution.
var extraGlobalICMPSysctls = []string{
	"/proc/sys/net/ipv6/icmp/echo_ignore_all",
	"/proc/sys/net/ipv4/icmp_echo_ignore_all",
	"/proc/sys/net/ipv6/icmp/ratelimit",
	"/proc/sys/net/ipv4/icmp_ratelimit",
}

func (s *Server) handleTshoot(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	now := time.Now()

	sec := func(title string) {
		fmt.Fprintf(&b, "\n\n========== %s ==========\n", title)
	}
	dump := func(label string, v any) {
		fmt.Fprintf(&b, "\n--- %s ---\n", label)
		enc, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintf(&b, "(could not encode: %v)\n", err)
			return
		}
		b.Write(enc)
		b.WriteByte('\n')
	}

	fmt.Fprintf(&b, "gravinet troubleshooting bundle\n")
	fmt.Fprintf(&b, "generated:  %s (%s)\n", now.Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "version:    %s (commit %s)\n", s.version, s.commit)
	fmt.Fprintf(&b, "go:         %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if h, err := os.Hostname(); err == nil {
		fmt.Fprintf(&b, "hostname:   %s\n", h)
	}
	fmt.Fprintf(&b, "\nCollected from ONE node. Peers hold their own view and the two can\n")
	fmt.Fprintf(&b, "legitimately disagree — a session torn down on one side is not always\n")
	fmt.Fprintf(&b, "torn down on the other. For any problem involving a specific peer,\n")
	fmt.Fprintf(&b, "collect this from BOTH ends: the disagreement is often the diagnosis.\n")

	sec("NODE")
	nat4, nat6 := s.be.NATStatusStrings()
	fmt.Fprintf(&b, "NAT (v4): %s\nNAT (v6): %s\n", nat4, nat6)
	dump("interfaces", s.be.Interfaces())

	sec("NETWORKS")
	ids := s.be.NetworkIDs()
	fmt.Fprintf(&b, "networks: %d\n", len(ids))
	for _, id := range ids {
		fmt.Fprintf(&b, "\n\n---------- network %x ----------\n", id)

		// Peers first and in full. Every field matters somewhere: reach and
		// relay identify the path, time distinguishes a stable session from
		// one being rebuilt on a loop, path_mtu and the fragment counters
		// catch amplification, and the drop counters name a cause instead of
		// leaving a lost packet anonymous.
		peers := s.be.ListPeers(id)
		fmt.Fprintf(&b, "peers: %d\n", len(peers))
		dump("peers", peers)

		dump("routes", s.be.Routes(id))
		dump("bans", s.be.ListBans(id))
		dump("disabled peers", s.be.DisabledPeers(id))

		if rules, err := s.be.FirewallRules(id); err != nil {
			fmt.Fprintf(&b, "\n--- firewall rules ---\n(unavailable: %v)\n", err)
		} else {
			dump("firewall rules", rules)
		}
		dump("firewall exemptions", s.be.FirewallExemptsFor(id))
	}

	// The host's own routing table. Included because gravinet's view of what
	// it installed (its overlay route list, above) and the kernel's actual
	// table can disagree, and when they do, every routing-related symptom
	// becomes undiagnosable from gravinet's side alone. An investigation
	// stalled on exactly this: peer underlay traffic was demonstrably being
	// steered into the tunnel, and nothing in the bundle could show which
	// route was doing it.
	sec("HOST ROUTING TABLE")
	for _, c := range [][]string{
		{"ip", "-4", "route", "show"},
		{"ip", "-6", "route", "show"},
		{"netstat", "-rn"}, // BSD/macOS fallback
	} {
		out, err := runDiag(c[0], c[1:]...)
		if err != nil {
			continue // not this platform's tool
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", strings.Join(c, " "), out)
	}

	// Address state, not just routes. The routing table above answers "where
	// does a packet go"; this answers "is the address it's addressed to even
	// usable yet" — a peer's overlay6 address can be gossiped, listed, and
	// look perfectly normal everywhere in gravinet's own view while sitting
	// tentative (still running Duplicate Address Detection) or dadfailed at
	// the kernel level, in which case the kernel silently drops real traffic
	// to it — indistinguishable from a firewall drop or a dead session from
	// gravinet's side alone. `ip`/`ifconfig`/`netsh` all print that state
	// inline per address (Linux: "tentative"/"dadfailed" flags; Windows:
	// State: Tentative/Duplicate via the verbose form), which is the whole
	// reason this is its own section instead of folded into routing above.
	sec("HOST INTERFACE ADDRESSES")
	for _, c := range [][]string{
		{"ip", "addr", "show"},
		{"ifconfig", "-a"}, // BSD/macOS, and Linux hosts that still have net-tools
		{"netsh", "interface", "ipv4", "show", "address"},
		{"netsh", "interface", "ipv6", "show", "address", "level=verbose"},
	} {
		out, err := runDiag(c[0], c[1:]...)
		if err != nil {
			continue // not this platform's tool
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", strings.Join(c, " "), out)
	}

	// The host OS's own firewall — distinct from gravinet's own per-network
	// overlay ACL dumped above (that's gravinet's mesh-level allow/deny list;
	// this is whatever nftables/iptables/pf/Windows Firewall is doing to the
	// same host underneath it, including rules gravinet didn't put there).
	// A peer whose distro ships a default-deny IPv6 INPUT policy, or a rule
	// that happens to permit ICMPv4 but not ICMPv6 specifically, produces
	// exactly a "v4 replies, v6 never does" symptom that looks identical to
	// a broken tunnel from every other section in this bundle.
	sec("HOST FIREWALL (OS-level, not gravinet's overlay ACL above)")
	for _, c := range [][]string{
		{"nft", "list", "ruleset"},
		{"ip6tables", "-L", "-n", "-v"},
		{"iptables", "-L", "-n", "-v"},
		{"pfctl", "-sr"},
		// pfctl -sr alone only shows the main ruleset — a rule living inside a
		// named anchor (pass/block added by a hardening script, authpf,
		// another tool, or by hand) stays invisible to it. '*' as the anchor
		// name is pfctl's own documented way to print every anchor
		// recursively alongside the main ruleset (see pfctl(8)); worth the
		// extra command specifically because a real diagnosis got stuck here
		// — a box's main ruleset showed no ICMPv6 pass rule at all under a
		// default-block base policy, which should in principle have meant
		// IPv4 was blocked too, and it visibly wasn't, so something pfctl -sr
		// alone wasn't showing had to be the explanation.
		{"pfctl", "-a", "*", "-sr"},
		{"netsh", "advfirewall", "show", "allprofiles"},
	} {
		out, err := runDiag(c[0], c[1:]...)
		if err != nil {
			continue // not this platform's tool
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", strings.Join(c, " "), out)
	}

	// Live IPv6 DAD and redirect-related sysctls, per overlay interface —
	// not just what gravinet logged applying at startup (see "IPv4/IPv6
	// redirects disabled" in the log tail below), because a later profile
	// change (NetworkManager/systemd-networkd), a manual sysctl, or a
	// per-interface default set at interface-creation time can all diverge
	// from that. Read directly from procfs on Linux — the same values
	// internal/ipfwd reads and writes — so this reports the kernel's actual
	// current state rather than gravinet's belief about it.
	sec("IPv6 / REDIRECT SYSCTLS")
	if runtime.GOOS == "linux" {
		readSysctl := func(path string) string {
			v, err := os.ReadFile(path)
			if err != nil {
				return "(absent)"
			}
			return strings.TrimSpace(string(v))
		}
		type sysctlRow struct{ Path, Value string }
		var rows []sysctlRow
		add := func(scope, family, knob string) {
			path := fmt.Sprintf("/proc/sys/net/%s/conf/%s/%s", family, scope, knob)
			rows = append(rows, sysctlRow{Path: path, Value: readSysctl(path)})
		}
		scopes := []string{"all", "default"}
		for _, ifc := range s.be.Interfaces() {
			if ifc.Iface != "" {
				scopes = append(scopes, ifc.Iface)
			}
		}
		for _, sc := range scopes {
			add(sc, "ipv4", "forwarding")
			add(sc, "ipv4", "accept_redirects")
			add(sc, "ipv4", "send_redirects")
			add(sc, "ipv6", "forwarding")
			add(sc, "ipv6", "accept_redirects")
			add(sc, "ipv6", "accept_dad")
			add(sc, "ipv6", "dad_transmits")
			add(sc, "ipv6", "disable_ipv6")
		}
		// See extraGlobalICMPSysctls' own doc comment for why these two
		// knobs (echo suppression + rate limiting) live outside the
		// conf/<scope>/ loop above instead of being folded into add().
		for _, path := range extraGlobalICMPSysctls {
			rows = append(rows, sysctlRow{Path: path, Value: readSysctl(path)})
		}
		dump("procfs (linux, live)", rows)
	}
	for _, c := range [][]string{
		{"sysctl", "net.inet.ip.forwarding", "net.inet.ip.redirect", "net.inet.icmp.drop_redirect",
			"net.inet.icmp.rediraccept", "net.inet6.ip6.forwarding", "net.inet6.ip6.redirect",
			"net.inet6.icmp6.rediraccept", "net.inet6.ip6.dad_count"}, // darwin/freebsd/openbsd naming varies; unknown names are skipped by sysctl itself with the rest still printed
		{"netsh", "interface", "ipv4", "show", "global"},
		{"netsh", "interface", "ipv6", "show", "global"},
	} {
		out, err := runDiag(c[0], c[1:]...)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", strings.Join(c, " "), out)
	}

	// Kernel-level ICMP counters: whether echo requests/replies are even
	// reaching/leaving the IP stack at all, independent of what any single
	// ping attempt observed. A peer stuck at InEchoReps stuck at 0 across an
	// entire bundle, despite peers reporting they're pinging it, points at
	// the kernel dropping the request before userspace ever sees it (DAD,
	// firewall) rather than gravinet's overlay losing the packet in transit.
	sec("ICMP STATISTICS")
	if runtime.GOOS == "linux" {
		for _, path := range []string{"/proc/net/snmp6", "/proc/net/snmp"} {
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "\n--- %s (Icmp lines) ---\n", path)
			for _, ln := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(ln, "Icmp") {
					b.WriteString(ln)
					b.WriteByte('\n')
				}
			}
		}
	}
	for _, c := range [][]string{
		{"netstat", "-s", "-p", "icmpv6"}, // windows, and some *nix netstat builds
		{"netstat", "-s"},                 // macOS/general; includes an Icmp6 section
	} {
		out, err := runDiag(c[0], c[1:]...)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", strings.Join(c, " "), out)
	}

	sec("CONFIG (secrets redacted)")
	if s.configPath == "" {
		b.WriteString("(config view not enabled on this node)\n")
	} else if raw, err := os.ReadFile(s.configPath); err != nil {
		fmt.Fprintf(&b, "(could not read %s: %v)\n", s.configPath, err)
	} else {
		fmt.Fprintf(&b, "path: %s\n\n", s.configPath)
		b.WriteString(redactConfig(string(raw)))
	}

	sec("LOG (tail)")
	if s.logPath == "" {
		b.WriteString("(file logging disabled; set \"log_file\" in the config to enable it)\n")
	} else {
		fmt.Fprintf(&b, "path: %s\n", s.logPath)
		tail, err := tailFile(s.logPath, tshootMaxLogBytes)
		if err != nil {
			fmt.Fprintf(&b, "(could not read: %v)\n", err)
		} else {
			fmt.Fprintf(&b, "(last %d bytes)\n\n", len(tail))
			b.Write(tail)
		}
	}

	b.WriteString("\n\n========== END ==========\n")

	// Packaged as a .tgz rather than served raw: the bundle is plain text and
	// compresses hard (long repeated log/JSON structure), and a single
	// familiar archive format is easier to attach to a support ticket or
	// chat than a multi-megabyte .txt. The inner member keeps the .txt name
	// and extension so it's still directly readable once extracted, or
	// openable in-place by anything that peeks inside an archive (most
	// editors, `tar tOf`, GitHub's own viewer) without renaming it first.
	txtName := fmt.Sprintf("gravinet-tshoot-%s.txt", now.Format("20060102-150405"))
	tgzName := fmt.Sprintf("gravinet-tshoot-%s.tgz", now.Format("20060102-150405"))
	txt := b.String()

	archived, err := packTshootTgz(txtName, txt, now)
	if err != nil {
		// Archiving an in-memory buffer essentially can't fail, but if it
		// somehow does, hand back the plain bundle rather than a 500 — a
		// person troubleshooting a live problem still gets what they asked
		// for, just not gzipped.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+txtName+"\"")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(txt))
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+tgzName+"\"")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archived)
}

// packTshootTgz wraps txt as a single-member gzip-compressed tar archive
// named memberName, for handleTshoot's response body. Split out from
// handleTshoot so the archiving itself — independent of gathering the
// bundle's actual content — has something to test directly.
func packTshootTgz(memberName, txt string, modTime time.Time) ([]byte, error) {
	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{
		Name:    memberName,
		Mode:    0o644,
		Size:    int64(len(txt)),
		ModTime: modTime,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write([]byte(txt)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gzw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// redactConfig blanks the value of any line whose key looks secret, for both
// "key: value" and "key = value" shapes, leaving structure and comments intact
// so the file is still readable as configuration.
func redactConfig(src string) string {
	lines := strings.Split(src, "\n")
	secretIndent := -1 // indent of an open secret-looking block, or -1

	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		indent := len(ln) - len(strings.TrimLeft(ln, " \t"))

		// Anything nested under a secret-looking key is itself secret — a list
		// of keys has no per-item key name to match on, so the block has to be
		// tracked rather than each line judged alone.
		if secretIndent >= 0 {
			if indent > secretIndent || strings.HasPrefix(trimmed, "-") {
				lines[i] = redactKeepingIndent(ln, "<redacted>")
				continue
			}
			secretIndent = -1
		}

		sepIdx := -1
		for _, sep := range []string{":", "="} {
			if j := strings.Index(ln, sep); j >= 0 && (sepIdx < 0 || j < sepIdx) {
				sepIdx = j
			}
		}
		if sepIdx < 0 {
			if len(trimmed) > 24 && !strings.ContainsAny(trimmed, " \t") {
				lines[i] = redactKeepingIndent(ln, "<redacted>")
			}
			continue
		}
		if !secretish.MatchString(ln[:sepIdx]) {
			continue
		}
		if strings.TrimSpace(ln[sepIdx+1:]) == "" {
			// "keys:" with the values on following lines.
			secretIndent = indent
			continue
		}
		lines[i] = ln[:sepIdx+1] + " <redacted>"
	}
	return strings.Join(lines, "\n")
}

func redactKeepingIndent(ln, with string) string {
	i := 0
	for i < len(ln) && (ln[i] == ' ' || ln[i] == '\t' || ln[i] == '-') {
		i++
	}
	return ln[:i] + with
}

// tailFile returns the last max bytes of path, starting at a line boundary so
// the bundle never opens mid-record.
func tailFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if st.Size() > max {
		start = st.Size() - max
	}
	if _, err := f.Seek(start, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, st.Size()-start)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	buf = buf[:n]
	if start > 0 {
		if j := strings.IndexByte(string(buf), '\n'); j >= 0 && j+1 < len(buf) {
			buf = buf[j+1:]
		}
	}
	return buf, nil
}

// runDiag executes a short read-only diagnostic command, bounded so a missing
// or wedged tool cannot hold up the bundle.
func runDiag(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", err
	}
	return string(out), nil
}
