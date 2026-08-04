package main

// These tests are the point of the v781 change, more than any individual leaf
// is. Every divergence they check for had already happened at least once, and
// each one was invisible because the only thing asserting "the CLI mirrors the
// web admin's nav" was a paragraph in a comment — which went stale silently,
// twice, in the same file that warns about exactly that failure mode:
//
//   - the whole "system" NAV_GROUPS entry (nine pages) had no CLI group at all
//   - "upgrade" stayed filed under info after the GUI moved it to system
//   - info's "api" page and monitor's "l2-peers" page had no leaf
//   - settings drifted from 11 rows to 27 while the CLI group stayed at 11
//
// So the source of truth is read from internal/webadmin/ui.go itself, not
// copied into a table here. A copied table is just the stale paragraph again
// with a compiler behind it: the next page added to the rail would leave both
// the copy and the CLI wrong, and the test would pass.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// uiSource reads internal/webadmin/ui.go relative to this package. Skips
// rather than fails if it isn't there: a stripped source tree is a legitimate
// state to build in, and an unfindable file is not evidence of drift.
func uiSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "webadmin", "ui.go"))
	if err != nil {
		t.Skipf("can't read ui.go (%v) — nothing to compare against", err)
	}
	return string(b)
}

// navGroups parses NAV_GROUPS out of ui.go: group name in order, then each
// item's section key in order. Deliberately a narrow parse of the exact
// literal shape that file uses ({ name:'x', items: [ ['key', '...'], ... ]}),
// so a change to that shape breaks this loudly instead of silently matching
// nothing and reporting success.
func navGroups(t *testing.T, src string) [][2]any {
	t.Helper()
	start := strings.Index(src, "const NAV_GROUPS = [")
	if start < 0 {
		t.Fatal("NAV_GROUPS not found in ui.go — this parser needs updating, not deleting")
	}
	end := strings.Index(src[start:], "\n];")
	if end < 0 {
		t.Fatal("NAV_GROUPS literal is unterminated")
	}
	body := src[start : start+end]

	groupRe := regexp.MustCompile(`\{\s*name:'([a-z0-9-]+)'`)
	itemRe := regexp.MustCompile(`^\s*\['([a-z0-9-]+)',`)

	var out [][2]any
	var cur string
	var items []string
	flush := func() {
		if cur != "" {
			out = append(out, [2]any{cur, items})
		}
		items = nil
	}
	for _, line := range strings.Split(body, "\n") {
		if m := groupRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = m[1]
			continue
		}
		if m := itemRe.FindStringSubmatch(line); m != nil {
			items = append(items, m[1])
		}
	}
	flush()
	if len(out) == 0 {
		t.Fatal("parsed no groups out of NAV_GROUPS — the literal's shape changed")
	}
	return out
}

// cliGroups is the CLI's own side of the comparison, by group name.
func cliGroups() map[string][]groupLeaf {
	return map[string][]groupLeaf{
		"mesh":    meshGroup,
		"traffic": trafficGroup,
		"naming":  namingGroup,
		"monitor": monitorGroup,
		"system":  systemGroup,
		"info":    infoGroup,
	}
}

// leafNames maps GUI section keys to the CLI leaf that covers them where the
// two legitimately differ. Only two do, and both are cases where the CLI name
// matches what the GUI *displays* rather than its internal key: ui.go's
// label() renders the 'bandwidth' section as "Shaping", and 'l2disco' as
// "L2 Disco". Keeping this map tiny is deliberate — it's the exemption list,
// and every entry is a place where someone reading the rail and typing the
// obvious command gets it wrong, so an entry has to earn itself.
var guiSectionToCLILeaf = map[string]string{
	"bandwidth": "shaping",
}

func TestCLIGroupsMatchNavGroups(t *testing.T) {
	src := uiSource(t)
	nav := navGroups(t, src)
	cli := cliGroups()

	for _, g := range nav {
		name := g[0].(string)
		sections := g[1].([]string)
		leaves, ok := cli[name]
		if !ok {
			t.Errorf("NAV_GROUPS has group %q with %d page(s) and the CLI has no such group", name, len(sections))
			continue
		}
		have := map[string]bool{}
		var order []string
		for _, l := range leaves {
			have[l.name] = true
			order = append(order, l.name)
		}
		var want []string
		for _, sec := range sections {
			leaf := sec
			if alt, mapped := guiSectionToCLILeaf[sec]; mapped {
				leaf = alt
			}
			want = append(want, leaf)
			if !have[leaf] {
				t.Errorf("%s: web admin has page %q with no CLI leaf (expected \"gravinet %s %s\")", name, sec, name, leaf)
			}
		}
		// Order too, not just membership: the group listing a bare
		// "gravinet <group>" prints is a menu, and a menu in a different
		// order from the rail it mirrors is its own small lie.
		if strings.Join(order, ",") != strings.Join(want, ",") {
			t.Errorf("%s: CLI leaf order %v does not match the rail's %v", name, order, want)
		}
	}

	// And the other direction: a CLI group the rail doesn't have.
	navNames := map[string]bool{}
	for _, g := range nav {
		navNames[g[0].(string)] = true
	}
	for name := range cli {
		if !navNames[name] {
			t.Errorf("CLI has group %q that NAV_GROUPS doesn't — either the rail dropped it or this group was invented", name)
		}
	}
}

// TestUpgradeIsFiledUnderSystem pins the specific move that went unnoticed:
// the GUI relocated Upgrade from Info to System (it replaces the running
// binary and restarts the service; Info is read-only reference pages) and the
// CLI kept it under info. Covered by the parity test above as a side effect,
// but named separately because "why is upgrade under system" is a question
// someone will ask, and a failing test with this name is the answer.
func TestUpgradeIsFiledUnderSystem(t *testing.T) {
	for _, l := range infoGroup {
		if l.name == "upgrade" {
			t.Error("upgrade is under info; the web admin moved it to System because it changes the host")
		}
	}
	found := false
	for _, l := range systemGroup {
		if l.name == "upgrade" {
			found = true
		}
	}
	if !found {
		t.Error("upgrade is not under system")
	}
}

// TestFlatUpgradeStillWorks: moving which group a leaf is filed under must not
// disturb the flat command, which is what the install scripts and the upgrade
// preflight actually call. This checks the dispatch table in main.go still
// names it rather than invoking it (invoking it would try to upgrade the
// machine running the tests).
func TestFlatUpgradeStillWorks(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Skipf("can't read main.go: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `case "upgrade":`) {
		t.Error(`main.go no longer dispatches the flat "gravinet upgrade"`)
	}
	if !strings.Contains(src, `{"upgrade"}`) {
		t.Error(`"upgrade" is missing from main.go's prefix-expansion candidate list`)
	}
}

// TestSettingsGroupCoversSettingsPage is the settings half, which NAV_GROUPS
// doesn't describe — the Settings page is reached from the gear icon, and its
// rows live in ui.go's own settingsRows literal. Same principle: read the
// list, don't copy it.
func TestSettingsGroupCoversSettingsPage(t *testing.T) {
	src := uiSource(t)
	start := strings.Index(src, "const settingsRows = [")
	if start < 0 {
		t.Fatal("settingsRows not found in ui.go — this parser needs updating, not deleting")
	}
	end := strings.Index(src[start:], "\n  ];")
	if end < 0 {
		t.Fatal("settingsRows literal is unterminated")
	}
	rowRe := regexp.MustCompile(`^\s*\['([a-z0-9-]+)-row',`)
	var rows []string
	for _, line := range strings.Split(src[start:start+end], "\n") {
		if m := rowRe.FindStringSubmatch(line); m != nil {
			rows = append(rows, m[1])
		}
	}
	if len(rows) == 0 {
		t.Fatal("parsed no rows out of settingsRows — the literal's shape changed")
	}

	// Row id -> settings leaf. Ids are UI element names and leaves are typed
	// at a shell, so these genuinely differ in spelling more often than the
	// nav sections do; the mapping is the translation, not an exemption.
	rowToLeaf := map[string]string{
		"loginban-attempts":    "login-ban",
		"loginban-duration":    "login-ban",
		"config-history-limit": "history-limit",
		"cluster-managed":      "managed",
		"cluster-manager":      "manager",
		"shell-allow":          "shell",
		"accept-manager-upg":   "accept-mgr-upgrades",
		"geoip":                "geoip",
		"loglevel":             "log-level",
		"logsize":              "log-size",
		"routeadv":             "route-adv",
		"keepalive":            "keepalive",
		"peertimeout":          "peer-timeout",
		"udpport":              "udp-port",
		"tcpport":              "tcp-port",
		"natstate":             "nat-state",
		"ip-forwarding":        "ip-forwarding",
		"ip-redirects":         "ip-redirects",
		"upnp":                 "upnp",
		"worker-threads":       "worker-threads",
		"tun-queues":           "tun-queues",
		"listen-addrs":         "listen-addrs",
		"socket-buffer":        "socket-buffer",
		"udp-gso":              "udp-gso",
	}
	// The two rows with no CLI form, each for a stated reason — see
	// settingsGroup's doc comment. Listed explicitly so adding a third
	// exemption is a visible decision rather than a quiet edit to a map.
	noCLIForm := map[string]string{
		"dark-mode":       "per-browser preference; nothing in config.json behind it",
		"tls-cert-upload": "two PEM blobs for the cert your own browser is trusting",
		"tls-cert-reset":  "paired with tls-cert-upload",
	}

	have := map[string]bool{}
	for _, l := range settingsGroup {
		have[l.name] = true
	}
	for _, row := range rows {
		if why, exempt := noCLIForm[row]; exempt {
			_ = why
			continue
		}
		leaf, known := rowToLeaf[row]
		if !known {
			t.Errorf("Settings row %q is new: give it a leaf in settingsGroup, or an entry in noCLIForm with the reason", row)
			continue
		}
		if !have[leaf] {
			t.Errorf("Settings row %q has no CLI leaf (expected \"gravinet settings %s\")", row, leaf)
		}
	}
}

// TestUsageListsEveryGroup: usage() is the first thing anyone reads, and it
// spent several versions describing a grouping that no longer existed — naming
// five groups when there were six, filing upgrade under info, and claiming
// Monitor's host-state views had no CLI path long after they did. Cheap to
// check that at least every group name appears.
func TestUsageListsEveryGroup(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Skipf("can't read main.go: %v", err)
	}
	src := string(b)
	usageStart := strings.Index(src, "func usage() {")
	usageEnd := strings.Index(src[usageStart:], "\n}\n")
	if usageStart < 0 || usageEnd < 0 {
		t.Fatal("can't locate usage()")
	}
	text := src[usageStart : usageStart+usageEnd]
	for name := range cliGroups() {
		if !strings.Contains(text, name) {
			t.Errorf("usage() doesn't mention the %q group", name)
		}
	}
	if !strings.Contains(text, "settings") {
		t.Error("usage() doesn't mention the settings group")
	}
}
