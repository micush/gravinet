//go:build linux

package tun

import (
	"bufio"
	"encoding/binary"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
)

// routeMetricInProcV4 returns the metric column for prefix on iface, and whether
// the route was found.
func routeMetricInProcV4(t *testing.T, iface string, p netip.Prefix) (int, bool) {
	t.Helper()
	f, err := os.Open("/proc/net/route")
	if err != nil {
		t.Fatalf("open /proc/net/route: %v", err)
	}
	defer f.Close()
	a := p.Addr().As4()
	wantDest := binary.LittleEndian.Uint32(a[:])
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[0] != iface {
			continue
		}
		var dest uint32
		_, _ = fmtSscanHex(fields[1], &dest)
		if dest == wantDest {
			m, _ := strconv.Atoi(fields[6]) // Metric is column index 6
			return m, true
		}
	}
	return 0, false
}

// routeInProcV4 reports whether /proc/net/route lists prefix on iface.
func routeInProcV4(t *testing.T, iface string, p netip.Prefix) bool {
	t.Helper()
	f, err := os.Open("/proc/net/route")
	if err != nil {
		t.Fatalf("open /proc/net/route: %v", err)
	}
	defer f.Close()
	a := p.Addr().As4()
	wantDest := binary.LittleEndian.Uint32(a[:]) // /proc shows dest in host (LE) order
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[0] != iface {
			continue
		}
		var dest uint32
		_, _ = fmtSscanHex(fields[1], &dest)
		if dest == wantDest {
			return true
		}
	}
	return false
}

func fmtSscanHex(s string, out *uint32) (int, error) {
	var v uint32
	for _, c := range s {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint32(c-'A') + 10
		}
	}
	*out = v
	return 1, nil
}

func TestAddDelRouteLinux(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root / CAP_NET_ADMIN")
	}
	d, err := New("gravirt0", 1280)
	if err != nil {
		t.Skipf("cannot create TUN here: %v", err)
	}
	defer d.Close()
	if err := d.AddIPv4(netip.MustParseAddr("10.99.0.1"), 24); err != nil {
		t.Fatalf("AddIPv4: %v", err)
	}

	p := netip.MustParsePrefix("10.20.20.0/24")
	const metric = 77
	if err := d.AddRoute(p, metric); err != nil {
		t.Fatalf("AddRoute: %v", err)
	}
	if !routeInProcV4(t, "gravirt0", p) {
		t.Fatal("route 10.20.20.0/24 not present in /proc/net/route after AddRoute")
	}
	if m, ok := routeMetricInProcV4(t, "gravirt0", p); !ok || m != metric {
		t.Fatalf("expected metric %d in /proc/net/route, got %d (found=%v)", metric, m, ok)
	}

	if err := d.DelRoute(p, metric); err != nil {
		t.Fatalf("DelRoute: %v", err)
	}
	if routeInProcV4(t, "gravirt0", p) {
		t.Fatal("route still present after DelRoute")
	}

	// IPv6 add should not error (kernel may lack v6 in this netns; tolerate).
	p6 := netip.MustParsePrefix("fd00:dead:beef::/48")
	if err := d.AddRoute(p6, 0); err != nil {
		t.Logf("v6 AddRoute returned: %v (acceptable if v6 disabled here)", err)
	} else {
		t.Log("v6 AddRoute OK")
		_ = d.DelRoute(p6, 0)
	}
}
