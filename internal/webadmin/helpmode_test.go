package webadmin

import (
	"regexp"
	"strings"
	"testing"
)

// v904 moved Interfaces' permanent secHint behind a toggle; v906 did the same
// for every other section and deleted secHint itself. The tests below pin the
// parts of that arrangement that break silently — a note attached to a column
// that does not exist, a HELP key that matches no section, a tab whose prose
// can never be reached. None of those produce an error at runtime. They render
// nothing, or render against the wrong column, and look fine.

// helpEntries returns each section key in HELP mapped to that entry's source.
func helpEntries(t *testing.T) map[string]string {
	t.Helper()
	tbl := between(t, indexHTML, "const HELP = {", "\nconst HELP_KEY")
	out := map[string]string{}
	re := regexp.MustCompile(`(?m)^  '([a-z0-9-]+)': \{$`)
	loc := re.FindAllStringSubmatchIndex(tbl, -1)
	for i, m := range loc {
		end := len(tbl)
		if i+1 < len(loc) {
			end = loc[i+1][0]
		}
		out[tbl[m[2]:m[3]]] = tbl[m[1]:end]
	}
	if len(out) == 0 {
		t.Fatal("no HELP entries parsed; the table's shape changed")
	}
	return out
}

// sectionRenderSrc returns the source of the function that renders a section,
// resolved through renderSection's dispatch table so the test follows the same
// mapping the app does.
func sectionRenderSrc(t *testing.T, section string) string {
	t.Helper()
	disp := between(t, indexHTML, "({ networks:secNetworks", "}[state.section])")
	m := regexp.MustCompile(`'?` + regexp.QuoteMeta(section) + `'?\s*:\s*(\w+)`).FindStringSubmatch(disp)
	if m == nil {
		return ""
	}
	return uiFuncSrc(t, m[1])
}

// The toggle starts off. A default that flipped to on — by writing '1' on
// first read, or by treating an absent key as enabled — would put the prose
// back on every page for every operator, which is the thing being undone.
func TestHelpModeDefaultsOff(t *testing.T) {
	src := uiFuncSrc(t, "helpModeOn")
	if !strings.Contains(src, "=== '1'") {
		t.Error("helpModeOn should be true only for a stored '1', so anything else (including an absent key) reads as off")
	}
	if !strings.Contains(src, "catch (_) { return false; }") {
		t.Error("a throwing localStorage (private windows) should fall back to off, not on")
	}
	if strings.Contains(src, "setItem") {
		t.Error("helpModeOn writes to storage; reading the mode should not create it")
	}
}

// Visibility is driven off a single .help-on class on #content rather than by
// the toggle finding and touching each element. Several sections build their
// tables from async fetches, so an annotation row can land long after the
// toggle was clicked — CSS is what makes that ordering irrelevant.
func TestHelpModeIsCSSDriven(t *testing.T) {
	for _, want := range []string{
		".help-topic { display:none; }",
		".help-on .help-topic { display:block; }",
		"tr.help-ann { display:none; }",
		".help-on tr.help-ann { display:table-row; }",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("missing help-mode rule %q", want)
		}
	}
	// display:table-row, not block: a <tr> forced to block collapses its
	// cells out of the table's columns, which is the whole point of the row.
	if strings.Contains(indexHTML, ".help-on tr.help-ann { display:block") {
		t.Error("annotation row is shown as block; it must stay a table-row to keep its cells in column")
	}
	// The toggle sits at the right edge of the heading. It is floated rather
	// than flexed because Resolver, SNMP and LLDP append status pills to the
	// same h2.sec, and those read as part of the title — a flex heading would
	// push them to the opposite edge along with this one.
	if !strings.Contains(indexHTML, ".help-tog { float:right;") {
		t.Error("help toggle is not right-justified")
	}
	if regexp.MustCompile(`h2\.sec[^{]*\{[^}]*display:\s*flex`).MatchString(indexHTML) {
		t.Error("h2.sec was made a flex container; that relocates the status pills other sections append to it")
	}
	// The pill's label is fixed and its colour carries the state. Rewriting
	// the text on click would change the pill's width, and since it is
	// right-justified its left edge would move — sliding the thing just
	// clicked out from under the pointer.
	if !strings.Contains(indexHTML, `.help-tog.on { color:var(--acc);`) {
		t.Error("the on state is not carried by colour, which is the only thing distinguishing it")
	}
	if strings.Contains(uiFuncSrc(t, "secHelp"), "tog.textContent") {
		t.Error("secHelp rewrites the pill's label; the label is fixed and only the colour changes")
	}
}

// The annotation row is a second header, not data. It has as many cells as a
// data row and no colspan, so without an explicit exclusion enhanceTable sorts
// it in among the rows and a filter hides it — which is how v904 shipped it.
func TestHelpAnnotationRowIsNotSortedOrFiltered(t *testing.T) {
	src := uiFuncSrc(t, "enhanceTable")
	if !strings.Contains(src, "!r.classList.contains('help-ann')") {
		t.Error("isData does not exclude the annotation row, so it will sort and filter as if it were a peer, route or rule")
	}
	if !strings.Contains(src, "helpAnnotate(table, header)") {
		t.Error("enhanceTable no longer injects annotations, so no section will show them")
	}
}

// secHint is gone. Its callers all moved into HELP, and a new one would put
// prose back on a page permanently while looking like every other section.
func TestSecHintIsGone(t *testing.T) {
	if strings.Contains(indexHTML, "secHint(") {
		t.Error("secHint is back; section prose belongs in HELP, revealed by the toggle")
	}
}

// Every HELP key must name a real section, or its prose is unreachable: the
// toggle is only rendered for HELP[state.section], and state.section only ever
// holds a key from SECTIONS.
func TestHelpKeysAreRealSections(t *testing.T) {
	nav := between(t, indexHTML, "const NAV_GROUPS = [", "const SECTIONS =")
	known := map[string]bool{}
	for _, m := range regexp.MustCompile(`\['([a-z0-9-]+)',`).FindAllStringSubmatch(nav, -1) {
		known[m[1]] = true
	}
	known["settings"] = true // reached from the rail's own link, not a NAV_GROUPS item
	for sec := range helpEntries(t) {
		if !known[sec] {
			t.Errorf("HELP has an entry for %q, which is not a section; its prose can never be shown", sec)
		}
	}
}

// An entry with no prose renders a toggle that reveals nothing.
func TestEveryHelpEntryHasATopic(t *testing.T) {
	for sec, body := range helpEntries(t) {
		if !strings.Contains(body, "topic:") {
			t.Errorf("HELP[%q] has no topic", sec)
		}
	}
}

// Column notes are matched to columns by header text, so a key matching no
// header is silently dropped: the note never appears, with nothing to see in
// the UI and no error anywhere. This is the check that catches a column
// renamed out from under its note.
func TestHelpColumnKeysMatchRealColumns(t *testing.T) {
	th := regexp.MustCompile(`<th[^>]*>([^<]{1,40})</th>`)
	checked := 0
	for sec, body := range helpEntries(t) {
		if !strings.Contains(body, "cols: {") {
			continue
		}
		cols := between(t, body, "cols: {", "    },")
		src := sectionRenderSrc(t, sec)
		if src == "" {
			t.Errorf("%s: cannot resolve a render function, so its column notes cannot be checked", sec)
			continue
		}
		headers := map[string]bool{}
		for _, m := range th.FindAllStringSubmatch(src, -1) {
			headers[strings.TrimSpace(m[1])] = true
		}
		for _, m := range regexp.MustCompile(`(?m)^      '([^']+)':`).FindAllStringSubmatch(cols, -1) {
			checked++
			if !headers[m[1]] {
				t.Errorf("HELP[%q].cols annotates %q, which is not a column of that section's table", sec, m[1])
			}
		}
	}
	if checked == 0 {
		t.Error("no column notes were checked; the parse found none, so this test is not guarding anything")
	}
}

// A tabbed section's topic keys have to match the tab ids, or a tab's prose is
// unreachable: helpTopic looks the current tab up by name and appends nothing
// when it misses.
func TestTabbedHelpTopicsMatchTheirTabs(t *testing.T) {
	for sec, body := range helpEntries(t) {
		m := regexp.MustCompile(`tab: '(\w+)'`).FindStringSubmatch(body)
		if m == nil {
			continue
		}
		src := sectionRenderSrc(t, sec)
		bar := regexp.MustCompile(`buildTabBar\(\[(.*?)\], state\.` + m[1]).FindStringSubmatch(src)
		if bar == nil {
			t.Errorf("%s declares tab %q but renders no tab bar bound to it", sec, m[1])
			continue
		}
		tabs := map[string]bool{}
		for _, tm := range regexp.MustCompile(`\['(\w+)',`).FindAllStringSubmatch(bar[1], -1) {
			tabs[tm[1]] = true
		}
		topic := between(t, body, "topic: {", "    },")
		for _, km := range regexp.MustCompile(`(?m)^      (\w+):`).FindAllStringSubmatch(topic, -1) {
			if km[1] == "_" {
				continue
			}
			if !tabs[km[1]] {
				t.Errorf("HELP[%q].topic has prose for tab %q, which is not one of that section's tabs", sec, km[1])
			}
		}
		for tab := range tabs {
			if !strings.Contains(topic, "\n      "+tab+":") {
				t.Errorf("HELP[%q] has no prose for its %q tab", sec, tab)
			}
		}
	}
}

// The two other kinds of explanatory text are hidden by the same class. Both
// have to be listed: .settings-desc is the paragraph under a settings row's
// label (Settings, BGP, Time, LLDP, Upgrade, Mesh Routes), .help-desc is prose
// describing a sub-card. .hint is deliberately not hidden wholesale — it also
// carries errors, loading lines, empty states and live status.
func TestSettingsAndCardProseAreHidden(t *testing.T) {
	for _, want := range []string{
		".settings-desc, .help-desc { display:none; }",
		".help-on .settings-desc, .help-on .help-desc { display:block; }",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("missing rule %q", want)
		}
	}
	if regexp.MustCompile(`(?m)^\s*\.hint \{[^}]*display:none`).MatchString(indexHTML) {
		t.Error(".hint is hidden wholesale; it also carries errors, empty states and live status, which must stay visible")
	}
	// The conditional remote-node warning on Upgrade is a .hint and must not
	// have been swept into .help-desc along with the descriptions. Anchor on
	// the markup: the same phrase appears earlier in a code comment, and
	// searching for the bare text finds that instead.
	m := regexp.MustCompile(`class="hint([^"]*)"[^>]*>Upgrades are local-only`).FindStringSubmatch(indexHTML)
	if m == nil {
		t.Fatal("Upgrade's remote-node warning is gone")
	}
	if strings.Contains(m[1], "help-desc") {
		t.Error("the remote-node warning was reclassed as help-desc; it is a conditional warning, not a description")
	}
}

// Every section must be able to reveal what it hides. .settings-desc and
// .help-desc are hidden by CSS wherever they appear, so a section with hidden
// text and no toggle would have no way to bring it back — which is why the
// toggle no longer depends on a HELP entry existing.
func TestEverySectionCanRevealItsText(t *testing.T) {
	src := uiFuncSrc(t, "secHelp")
	if !strings.Contains(src, "HELP_OMIT.includes(section)") {
		t.Fatal("secHelp no longer gates on HELP_OMIT")
	}
	if !strings.Contains(src, "const h = HELP[section] || {};") {
		t.Error("secHelp still returns early when a section has no HELP entry, so that section can never reveal its settings-desc")
	}
	omit := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([a-z-]+)'`).FindAllStringSubmatch(
		between(t, indexHTML, "const HELP_OMIT = [", "];"), -1) {
		omit[m[1]] = true
	}
	// A section listed as having nothing to explain must really have nothing.
	for sec := range omit {
		if s := sectionRenderSrc(t, sec); strings.Contains(s, "settings-desc") || strings.Contains(s, "help-desc") {
			t.Errorf("%q is in HELP_OMIT but renders hidden prose, which nothing can then reveal", sec)
		}
	}
}

// Latency's prose quotes its own poll and window durations. v906 deleted that
// secHint without carrying the text into HELP — the extractor that built the
// table matched a quoted string and this one was a concatenation, so it was
// removed but never re-added, and the page lost all of its text. Any topic
// that interpolates must resolve, and latency must have one.
func TestHelpPlaceholdersResolve(t *testing.T) {
	vars := between(t, indexHTML, "const HELP_VARS = {", "};")
	known := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([^']+)':`).FindAllStringSubmatch(vars, -1) {
		known[m[1]] = true
	}
	for sec, body := range helpEntries(t) {
		for _, m := range regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*(?:/[0-9]+)?)\}`).FindAllStringSubmatch(body, -1) {
			if !known[m[1]] {
				t.Errorf("HELP[%q] interpolates {%s}, which HELP_VARS does not define; it renders literally", sec, m[1])
			}
		}
	}
	if _, ok := helpEntries(t)["latency"]; !ok {
		t.Error("latency has no HELP entry; v906 deleted its text and this is the guard against that recurring")
	}
}

// Seeds, Peers, Bans and Mesh Peers described themselves with a plain .hint in
// their card markup rather than a secHint call, which is why v906's sweep
// missed them. Their prose is in HELP now and the inline block is gone.
func TestSectionsWithoutSecHintWereNotMissed(t *testing.T) {
	for _, sec := range []string{"seeds", "peers", "bans", "mesh-peers"} {
		body, ok := helpEntries(t)[sec]
		if !ok {
			t.Errorf("%q has no HELP entry", sec)
			continue
		}
		if !strings.Contains(body, "topic:") {
			t.Errorf("%q has no topic", sec)
		}
		if s := sectionRenderSrc(t, sec); strings.Contains(s, `<div class="hint" style="margin:0 0 10px">`) {
			t.Errorf("%q still prints its description inline as well as in HELP", sec)
		}
	}
}
