package webadmin

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"gravinet/internal/hosts"
	"gravinet/internal/resolver"
)

// handleAbout reports build and host identity for the Info → About tab: the
// gravinet version/commit, the OS and a best-effort OS version string, the
// architecture, and the Go runtime version.
func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"gravinet_version": s.version,
		"gravinet_commit":  s.commit,
		"os":               runtime.GOOS,
		"os_version":       osVersion(),
		"arch":             runtime.GOARCH,
		"go_version":       runtime.Version(),
	})
}

// handleLocalHosts returns the contents of the local hosts file (the same file
// the daemon writes peer/advertised records into), for the Info → Hosts tab.
func (s *Server) handleLocalHosts(w http.ResponseWriter, r *http.Request) {
	path := hosts.DefaultPath()
	b, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"text": "", "path": path, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": string(b), "path": path})
}

// dnsNetDump is one network's live conditional-forwarding state, for the
// Info → DNS tab.
type dnsNetDump struct {
	Name  string `json:"name"`
	Iface string `json:"iface"`
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
}

// handleLocalDNS reports what's actually registered with this host's OS
// resolver right now, per network — read live via internal/resolver.Dump
// (resolvectl on Linux, /etc/resolver on macOS, NRPT on Windows, local-unbound
// forward zones on FreeBSD), not from anything gravinet remembers applying,
// so it reflects reality even if a Sync silently failed or something else
// changed the registration since. This is the single most direct way to
// answer "is conditional forwarding actually working," short of a raw shell.
func (s *Server) handleLocalDNS(w http.ResponseWriter, r *http.Request) {
	var out []dnsNetDump
	for _, ifc := range s.be.Interfaces() {
		// Must match dnssync.go's tag derivation exactly (DNSTag is never set
		// from config today, so it always falls through to this form).
		tag := fmt.Sprintf("%016x", ifc.NetworkID)
		text, err := resolver.Dump(tag, ifc.Iface)
		d := dnsNetDump{Name: ifc.Name, Iface: ifc.Iface, Text: text}
		if err != nil {
			d.Error = err.Error()
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": out, "os": runtime.GOOS})
}

// latencyPeer is one peer's ping result, for the Info → Latency tab.
type latencyPeer struct {
	NodeID   string  `json:"node_id"`
	Hostname string  `json:"hostname"`
	Overlay  string  `json:"overlay"` // address actually pinged (v4 preferred, v6 fallback)
	RTTMs    float64 `json:"rtt_ms,omitempty"`
	OK       bool    `json:"ok"`
	Error    string  `json:"error,omitempty"`
}

type latencyNet struct {
	Name  string        `json:"name"`
	Peers []latencyPeer `json:"peers"`
}

// handleLocalLatency pings every other peer on every up network over the
// overlay (so it measures the mesh path, not the underlay), concurrently so
// the response time is bounded by one ping timeout rather than the sum of all
// of them. A peer with no overlay address assigned yet (still handshaking) is
// reported as not-ok rather than silently skipped, so the peer is still
// visible in the list.
func (s *Server) handleLocalLatency(w http.ResponseWriter, r *http.Request) {
	var out []latencyNet
	for _, ifc := range s.be.Interfaces() {
		peers := s.be.ListPeers(ifc.NetworkID)
		results := make([]latencyPeer, len(peers))
		var wg sync.WaitGroup
		for i, p := range peers {
			addr := p.Overlay4
			if addr == "" {
				addr = p.Overlay6
			}
			results[i] = latencyPeer{NodeID: p.NodeID, Hostname: p.Hostname, Overlay: addr}
			if addr == "" {
				results[i].Error = "no overlay address yet"
				continue
			}
			wg.Add(1)
			go func(i int, addr string) {
				defer wg.Done()
				ms, err := pingRTT(addr)
				if err != nil {
					results[i].Error = err.Error()
					return
				}
				results[i].OK = true
				results[i].RTTMs = ms
			}(i, addr)
		}
		wg.Wait()
		out = append(out, latencyNet{Name: ifc.Name, Peers: results})
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": out})
}

var pingTimeRe = regexp.MustCompile(`time[=<]([0-9.]+)\s*ms`)

// pingArgsForOS returns the ping(1) argv (excluding argv0) used to send two
// probes to addr with an approximately one-second per-probe timeout on goos.
// Split out from pingRTT so the per-OS flag mapping can be exercised directly
// by a test without depending on the test binary's own runtime.GOOS (fixed at
// build time) or actually running ping. This exact split caught the bug:
// openbsd fell through to the Linux/default case and got "-W 1", a flag
// OpenBSD's ping doesn't have — the command errored out before sending a
// packet, so Info → Latency read "no reply" for every peer on an OpenBSD
// host, indistinguishable from a real, universal timeout.
func pingArgsForOS(goos, addr string) []string {
	switch goos {
	case "windows":
		return []string{"-n", "2", "-w", "1000", addr}
	case "darwin", "freebsd":
		// Both are BSD ping and share -t (overall timeout, in seconds) with
		// identical semantics. Deliberately not -W here: FreeBSD's ping -W is
		// in milliseconds, unlike Linux's -W which is in seconds — passing
		// the Linux/default branch's "-W 1" to FreeBSD's ping would mean
		// "wait 1 millisecond for a reply," not 1 second, which is short
		// enough that every single probe times out regardless of whether
		// the peer actually replied. -t sidesteps the units mismatch
		// entirely by using the one flag both BSD pings agree on.
		return []string{"-c", "2", "-t", "1", addr}
	case "openbsd":
		// OpenBSD's ping has neither Linux's -W nor FreeBSD/Darwin's -t: its
		// own -t sets the IP TTL, not a timeout, and it has no overall-timeout
		// flag at all. -w maxwait is the closest equivalent — OpenBSD
		// documents it as the max seconds to wait for a reply to a given
		// packet before sending the next one, the same per-probe budget -W
		// gives on Linux.
		return []string{"-c", "2", "-w", "1", addr}
	default: // linux and other unix-likes
		return []string{"-c", "2", "-W", "1", addr}
	}
}

// pingRTT shells out to the OS's native ping (2 probes, ~1s budget each) and
// parses the reported round-trip time. Used instead of a raw ICMP socket
// (golang.org/x/net/icmp) to avoid an external module dependency and the
// platform-specific raw-socket privilege handling that comes with it — the
// daemon already runs with the privilege system ping needs, and its output
// format is stable enough to parse reliably across Linux/macOS/Windows/BSD.
// Reports the fastest of the probes that succeeded (packet loss on one probe
// shouldn't hide a real, working RTT reported by the other).
func pingRTT(addr string) (float64, error) {
	cmd := exec.Command("ping", pingArgsForOS(runtime.GOOS, addr)...)
	out, runErr := cmd.CombinedOutput()
	best := -1.0
	for _, m := range pingTimeRe.FindAllStringSubmatch(string(out), -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && (best < 0 || v < best) {
			best = v
		}
	}
	if best < 0 {
		if len(out) == 0 && runErr != nil {
			// The command itself never produced output — most likely ping
			// isn't installed, not "peer didn't reply". Worth distinguishing:
			// one means "check this host's setup", the other "check the peer".
			return 0, fmt.Errorf("could not run ping: %w", runErr)
		}
		return 0, fmt.Errorf("no reply")
	}
	return best, nil
}

// routeRow is one entry of the host routing table, as shown in Info → Routes.
type routeRow struct {
	Dest    string `json:"dest"`    // destination prefix (CIDR), "default" for 0-length
	Gateway string `json:"gateway"` // next hop, or "" when on-link
	Iface   string `json:"iface"`
	Metric  int    `json:"metric"`
	Family  int    `json:"family"` // 4 or 6
}

// handleLocalRoutes returns the host's kernel routing table. On Linux it is
// parsed from /proc/net/route and /proc/net/ipv6_route into structured rows (no
// external binary, matching how routes are installed). On other platforms it
// falls back to the OS's route-listing command and returns the raw text.
func (s *Server) handleLocalRoutes(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS == "linux" {
		rows := readProcRoutes()
		writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "os": "linux"})
		return
	}
	text, err := nativeRouteText()
	resp := map[string]any{"entries": []routeRow{}, "text": text, "os": runtime.GOOS}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// osVersion is a best-effort human-readable OS version. On Linux it reads the
// PRETTY_NAME from /etc/os-release and appends the kernel release; on other
// platforms it returns the GOOS (callers also have the arch/Go version).
func osVersion() string {
	if runtime.GOOS == "linux" {
		pretty := osReleasePretty()
		kernel := strings.TrimSpace(readFileString("/proc/sys/kernel/osrelease"))
		switch {
		case pretty != "" && kernel != "":
			return pretty + " (kernel " + kernel + ")"
		case pretty != "":
			return pretty
		case kernel != "":
			return "Linux (kernel " + kernel + ")"
		}
	}
	return runtime.GOOS
}

func osReleasePretty() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// readProcRoutes parses the IPv4 and IPv6 kernel routing tables from /proc.
func readProcRoutes() []routeRow {
	rows := readProcRoutes4()
	rows = append(rows, readProcRoutes6()...)
	if rows == nil {
		rows = []routeRow{}
	}
	return rows
}

func readProcRoutes4() []routeRow {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseProcRoutes4(f)
}

// parseProcRoutes4 parses the /proc/net/route format. Columns (tab-separated):
// Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
// Destination, Gateway and Mask are little-endian hex IPv4 words.
func parseProcRoutes4(rd io.Reader) []routeRow {
	var out []routeRow
	sc := bufio.NewScanner(rd)
	first := true
	for sc.Scan() {
		if first { // header
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 {
			continue
		}
		dest := hexToIP4(fields[1])
		gw := hexToIP4(fields[2])
		mask := hexToIP4(fields[7])
		metric, _ := strconv.Atoi(fields[6])
		if !dest.IsValid() {
			continue
		}
		bits := maskBits(mask)
		destStr := "default"
		if !(dest.Compare(netip.IPv4Unspecified()) == 0 && bits == 0) {
			destStr = netip.PrefixFrom(dest, bits).String()
		}
		gwStr := ""
		if gw.IsValid() && gw.Compare(netip.IPv4Unspecified()) != 0 {
			gwStr = gw.String()
		}
		out = append(out, routeRow{Dest: destStr, Gateway: gwStr, Iface: fields[0], Metric: metric, Family: 4})
	}
	return out
}

func readProcRoutes6() []routeRow {
	f, err := os.Open("/proc/net/ipv6_route")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseProcRoutes6(f)
}

// parseProcRoutes6 parses the /proc/net/ipv6_route format. Columns (space-sep
// hex): dest destlen src srclen nexthop metric refcnt use flags iface
func parseProcRoutes6(rd io.Reader) []routeRow {
	var out []routeRow
	sc := bufio.NewScanner(rd)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		dest := hexToIP6(fields[0])
		destLen, _ := strconv.ParseInt(fields[1], 16, 0)
		nexthop := hexToIP6(fields[4])
		metric64, _ := strconv.ParseInt(fields[5], 16, 0)
		iface := fields[9]
		if !dest.IsValid() {
			continue
		}
		destStr := "default"
		if !(dest.Compare(netip.IPv6Unspecified()) == 0 && destLen == 0) {
			destStr = netip.PrefixFrom(dest, int(destLen)).String()
		}
		gwStr := ""
		if nexthop.IsValid() && nexthop.Compare(netip.IPv6Unspecified()) != 0 {
			gwStr = nexthop.String()
		}
		out = append(out, routeRow{Dest: destStr, Gateway: gwStr, Iface: iface, Metric: int(metric64), Family: 6})
	}
	return out
}

// hexToIP4 decodes a little-endian 8-hex-digit IPv4 word (as in /proc/net/route).
func hexToIP4(h string) netip.Addr {
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return netip.Addr{}
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(v))
	return netip.AddrFrom4(b)
}

// hexToIP6 decodes a 32-hex-digit IPv6 address (as in /proc/net/ipv6_route).
func hexToIP6(h string) netip.Addr {
	if len(h) != 32 {
		return netip.Addr{}
	}
	var b [16]byte
	for i := 0; i < 16; i++ {
		v, err := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
		if err != nil {
			return netip.Addr{}
		}
		b[i] = byte(v)
	}
	return netip.AddrFrom16(b)
}

// maskBits returns the prefix length of a contiguous IPv4 netmask.
func maskBits(mask netip.Addr) int {
	if !mask.Is4() {
		return 0
	}
	b := mask.As4()
	bits := 0
	for _, octet := range b {
		for i := 7; i >= 0; i-- {
			if octet&(1<<uint(i)) != 0 {
				bits++
			} else {
				return bits
			}
		}
	}
	return bits
}

// nativeRouteText runs the platform's route-listing command (non-Linux fallback)
// and returns its raw text. Linux never reaches this — it parses /proc instead.
// routeCommandForOS returns the argv0/args used to list goos's kernel routing
// table natively, or ("", nil) if none is known — the caller then returns an
// empty (not erroring) result. Split out from nativeRouteText so the per-OS
// mapping can be exercised directly by a test without depending on the test
// binary's own runtime.GOOS (fixed at build time) or actually executing
// anything. This exact split caught the bug: openbsd was simply missing from
// the switch, so Monitor → Route Table silently rendered an empty page there
// with no error at all — indistinguishable, from the UI, between "no routes"
// and "this OS isn't handled yet".
func routeCommandForOS(goos string) (name string, args []string) {
	switch goos {
	case "darwin", "freebsd", "openbsd":
		return "netstat", []string{"-rn"}
	case "windows":
		return "route", []string{"print"}
	default:
		return "", nil
	}
}

func nativeRouteText() (string, error) {
	name, args := routeCommandForOS(runtime.GOOS)
	if name == "" {
		return "", nil
	}
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
