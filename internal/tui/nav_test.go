package tui

// The rail's parity with the web admin's, checked the way
// cmd/gravinet/navparity_test.go checks the CLI's: by reading NAV_GROUPS out
// of internal/webadmin/ui.go at test time rather than copying it into a table
// here. That file's own comment explains why, and it applies with more force
// now that there are three copies instead of two — a copied table is a stale
// paragraph with a compiler behind it, and the next page added to the rail
// would leave both the copy and this test wrong while the test kept passing.
//
// Unlike the CLI's version, this compares the descriptions too. They are the
// line under each page's heading here and the tooltip in the browser, and a
// page whose description says something the web admin's no longer does is a
// page telling two operators two different stories.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// uiSource reads ui.go relative to this package. Skips rather than fails when
// it is absent: a stripped source tree is a legitimate state to build in, and
// an unfindable file is not evidence of drift.
func uiSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "webadmin", "ui.go"))
	if err != nil {
		t.Skipf("can't read ui.go (%v) — nothing to compare against", err)
	}
	return string(b)
}

// parsedGroup is one NAV_GROUPS entry as read out of ui.go.
type parsedGroup struct {
	name  string
	items []navItem
}

// parseNavGroups reads the literal. Deliberately a narrow parse of the exact
// shape that file uses — { name:'x', items: [ ['key', 'tip'], ... ]} — so a
// change to the shape breaks this loudly rather than silently matching
// nothing and reporting success.
func parseNavGroups(t *testing.T, src string) []parsedGroup {
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
	itemRe := regexp.MustCompile(`^\s*\['([a-z0-9-]+)',\s*'(.*)'\],?\s*$`)

	var out []parsedGroup
	var cur *parsedGroup
	for _, l := range strings.Split(body, "\n") {
		if m := groupRe.FindStringSubmatch(l); m != nil {
			out = append(out, parsedGroup{name: m[1]})
			cur = &out[len(out)-1]
			continue
		}
		if m := itemRe.FindStringSubmatch(l); m != nil && cur != nil {
			cur.items = append(cur.items, navItem{key: m[1], desc: unescapeJS(m[2])})
		}
	}
	if len(out) == 0 {
		t.Fatal("parsed no groups out of NAV_GROUPS — the literal's shape changed")
	}
	return out
}

// unescapeJS turns the two escapes ui.go's tooltips actually use — \uXXXX for
// the typographic characters, and \' inside a single-quoted string — into the
// runes navGroups holds. Anything else is left alone: this is not a
// JavaScript string parser and should not grow into one, and an unhandled
// escape shows up as a mismatch rather than as a silent pass.
func unescapeJS(s string) string {
	s = strings.ReplaceAll(s, `\'`, `'`)
	re := regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)
	return re.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseInt(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
}

func TestRailMatchesNavGroups(t *testing.T) {
	nav := parseNavGroups(t, uiSource(t))

	if len(nav) != len(navGroups) {
		t.Fatalf("group count: ui.go has %d, the rail has %d", len(nav), len(navGroups))
	}
	for i, want := range nav {
		got := navGroups[i]
		if got.name != want.name {
			t.Errorf("group %d: ui.go says %q, the rail says %q (order is load-bearing)", i, want.name, got.name)
			continue
		}
		if len(got.items) != len(want.items) {
			t.Errorf("group %q: ui.go has %d pages, the rail has %d", want.name, len(want.items), len(got.items))
			continue
		}
		for j, wi := range want.items {
			gi := got.items[j]
			if gi.key != wi.key {
				t.Errorf("group %q position %d: ui.go says %q, the rail says %q", want.name, j, wi.key, gi.key)
			}
			if gi.desc != wi.desc {
				t.Errorf("page %q description drifted:\n  ui.go: %q\n  rail:  %q", wi.key, wi.desc, gi.desc)
			}
		}
	}
}

// TestEverySectionHasABuilder is the other half: a page in the rail with no
// builder would render the "this is a bug" card, which is a thing to find in
// a test rather than at 3am.
func TestEverySectionHasABuilder(t *testing.T) {
	for _, sec := range sectionKeys() {
		if _, ok := pageBuilders[sec]; !ok {
			t.Errorf("section %q is in the rail with no builder in pageBuilders", sec)
		}
	}
	if _, ok := pageBuilders[helpSection]; !ok {
		t.Error("the help page has no builder")
	}
}

// TestNoStrayBuilders catches the reverse: a builder for a page that is not in
// the rail is unreachable, which usually means a section was renamed in one
// place and not the other.
func TestNoStrayBuilders(t *testing.T) {
	known := map[string]bool{helpSection: true}
	for _, sec := range sectionKeys() {
		known[sec] = true
	}
	for sec := range pageBuilders {
		if !known[sec] {
			t.Errorf("pageBuilders has %q, which is not in the rail — unreachable", sec)
		}
	}
}

// TestLabelMatchesUIGo pins the label() transcription against ui.go's own,
// parsed out of its if-chain. The rail's text is what an operator compares
// against a browser on the next screen; "Ipv6ra" in one and "v6 ra" in the
// other is a small lie of exactly the kind this file exists to prevent.
func TestLabelMatchesUIGo(t *testing.T) {
	src := uiSource(t)
	start := strings.Index(src, "function label(s){")
	if start < 0 {
		t.Skip("label() not found in ui.go")
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatal("label() is unterminated")
	}
	body := src[start : start+end]

	re := regexp.MustCompile(`if \(s==='([a-z0-9-]+)'\) return '([^']*)';`)
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		if got := label(m[1]); got != m[2] {
			t.Errorf("label(%q): ui.go says %q, this package says %q", m[1], m[2], got)
		}
	}

	// The acronym rule at the end of the chain, checked by walking the set
	// ui.go names rather than trusting that both lists have the same members.
	acroRe := regexp.MustCompile(`return s==='nat'((\|\|s==='[a-z0-9]+')+)`)
	if am := acroRe.FindStringSubmatch(body); am != nil {
		for _, k := range append([]string{"nat"}, regexp.MustCompile(`'([a-z0-9]+)'`).FindAllString(am[1], -1)...) {
			k = strings.Trim(k, "'")
			if got := label(k); got != strings.ToUpper(k) {
				t.Errorf("label(%q) should be the upper-cased acronym %q, got %q", k, strings.ToUpper(k), got)
			}
		}
	} else {
		t.Error("the acronym arm of ui.go's label() was not found — this parser needs updating")
	}
}

// TestSectionVisibleMatchesUIGo checks the gating set. A page hidden in the
// browser and shown here would be a page an operator can open and find empty,
// with no explanation, on a host that simply does not have the daemon behind
// it.
func TestSectionVisibleMatchesUIGo(t *testing.T) {
	src := uiSource(t)
	start := strings.Index(src, "function sectionVisible(sec){")
	if start < 0 {
		t.Skip("sectionVisible() not found in ui.go")
	}
	end := strings.Index(src[start:], "\n}")
	body := src[start : start+end]

	// Every section ui.go gates, and which flag gates it.
	gated := map[string]func(caps) bool{
		"bgp":       func(c caps) bool { return c.bgp },
		"bgp-peers": func(c caps) bool { return c.bgp },
		"ipv6ra":    func(c caps) bool { return c.ipv6ra },
		"dhcp":      func(c caps) bool { return c.dhcp },
		"snmp":      func(c caps) bool { return c.snmp },
		"lldp":      func(c caps) bool { return c.lldp },
		"l2-peers":  func(c caps) bool { return c.lldp },
		"syslog":    func(c caps) bool { return c.syslog },
	}

	re := regexp.MustCompile(`sec === '([a-z0-9-]+)'`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		seen[m[1]] = true
		if _, ok := gated[m[1]]; !ok {
			t.Errorf("ui.go gates section %q and this package does not", m[1])
		}
	}
	for sec := range gated {
		if !seen[sec] {
			t.Errorf("this package gates %q and ui.go does not — it would be hidden here and shown in a browser", sec)
		}
	}

	// And that the gating actually works both ways round.
	none := caps{}
	all := caps{bgp: true, ipv6ra: true, dhcp: true, snmp: true, lldp: true, syslog: true}
	for sec, pick := range gated {
		if sectionVisible(sec, none) {
			t.Errorf("%q is visible with no capabilities", sec)
		}
		if !sectionVisible(sec, all) {
			t.Errorf("%q is hidden with every capability present", sec)
		}
		if !pick(all) {
			t.Errorf("%q's capability picker is wrong", sec)
		}
	}
	// An ungated page stays visible on a bare host, which is the whole point
	// of the distinction.
	if !sectionVisible("networks", none) {
		t.Error("networks should be visible regardless of what is installed")
	}
}

// TestSectionHeadingSplits pins the two pages whose rail label and page
// heading deliberately differ, and that everything else uses one string for
// both.
func TestSectionHeadingSplits(t *testing.T) {
	if got := sectionHeading("ipv6ra"); got != "IPv6 Router Advertisements" {
		t.Errorf("ipv6ra heading = %q", got)
	}
	if got := sectionHeading("dhcp"); got != "DHCP Relay" {
		t.Errorf("dhcp heading = %q", got)
	}
	for _, sec := range sectionKeys() {
		if sec == "ipv6ra" || sec == "dhcp" {
			continue
		}
		if sectionHeading(sec) != label(sec) {
			t.Errorf("%q: heading %q and label %q differ with no reason recorded",
				sec, sectionHeading(sec), label(sec))
		}
	}
}
