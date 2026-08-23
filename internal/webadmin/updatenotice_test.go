package webadmin

import (
	"regexp"
	"strings"
	"testing"
)

// After login the header checks once whether a newer tag exists upstream and,
// if so, shows a red notice beside the brand that goes to System > Upgrade.
// Nothing is shown otherwise — not "up to date", not a placeholder.

// The check runs from dashboard(), which runs once per login. On a timer it
// would poll a third party in the background for an answer that cannot change
// without someone upgrading a node.
func TestUpdateCheckRunsOncePerLogin(t *testing.T) {
	dash := uiFuncSrc(t, "dashboard")
	if !strings.Contains(dash, "maybeShowUpdateNotice(top.querySelector('.brand'))") {
		t.Fatal("dashboard does not run the update check, so the notice can never appear")
	}
	if n := strings.Count(indexHTML, "maybeShowUpdateNotice("); n != 2 {
		t.Errorf("maybeShowUpdateNotice has %d references (want 2: its definition and the one call); a second caller means more than one check per login", n)
	}
	src := uiFuncSrc(t, "maybeShowUpdateNotice")
	for _, bad := range []string{"setInterval", "setTimeout"} {
		if strings.Contains(src, bad) {
			t.Errorf("the update check uses %s; it is a once-per-login check, not a poll", bad)
		}
	}
}

// Silence is the only negative. A header that reports "up to date" on every
// login is a header that gets tuned out, and an offline node, a rate limit and
// an untagged repository are all things the person logging in cannot act on.
func TestUpdateNoticeSaysNothingWhenThereIsNothingToSay(t *testing.T) {
	src := uiFuncSrc(t, "maybeShowUpdateNotice")
	if !strings.Contains(src, "if (!latest) return;") {
		t.Error("no early return when there is no newer tag, so something renders when nothing should")
	}
	if !strings.Contains(src, "catch (_) { return; }") {
		t.Error("a failed lookup is not swallowed; it should be silent, not an error in the header")
	}
	for _, bad := range []string{"up to date", "checking", "\u2026'"} {
		if strings.Contains(src, bad) {
			t.Errorf("the notice renders %q; the header shows a notice or nothing at all", bad)
		}
	}
}

// Only a strictly higher version counts, and one place decides that, so the
// header and the Upgrade page cannot disagree about whether a node is behind.
func TestUpdateAvailableComparesNumerically(t *testing.T) {
	src := uiFuncSrc(t, "updateAvailable")
	if !strings.Contains(src, "tagNumber(cur)") || !strings.Contains(src, "tagNumber(latest)") {
		t.Error("updateAvailable does not compare parsed version numbers")
	}
	if !strings.Contains(src, "b <= a") {
		t.Error("updateAvailable does not require the upstream tag to be strictly higher")
	}
	if !strings.Contains(src, "a === null || b === null") {
		t.Error("an unparseable tag is not excluded; a repository that starts tagging release candidates would light up the whole fleet")
	}
	if !strings.Contains(uiFuncSrc(t, "maybeShowUpdateNotice"), "await updateAvailable()") {
		t.Error("the header does not go through updateAvailable, so it can disagree with the Upgrade page")
	}
}

// Clicking the notice must land where a rail click lands.
func TestUpdateNoticeNavigatesToUpgrade(t *testing.T) {
	src := uiFuncSrc(t, "maybeShowUpdateNotice")
	for _, want := range []string{"state.section = 'upgrade';", "setActiveRailTab('upgrade');", "renderSection();"} {
		if !strings.Contains(src, want) {
			t.Errorf("the notice does not navigate like the rail does: missing %q", want)
		}
	}
	if !strings.Contains(src, "e.preventDefault()") {
		t.Error("the notice's href is not suppressed, so clicking it also jumps the page")
	}
	if !strings.Contains(indexHTML, ".update-avail { color:var(--danger);") {
		t.Error("the notice is not red")
	}
}

// The preference is browser-local and defaults on, including when localStorage
// cannot be read. It is not node configuration: it governs whether this
// browser reaches out, so proxying it to a peer would mean nothing.
func TestUpdateCheckPreferenceDefaultsOn(t *testing.T) {
	src := uiFuncSrc(t, "updateCheckOn")
	if !strings.Contains(src, "!== '0'") {
		t.Error("updateCheckOn should be false only for a stored '0', so an absent key reads as on")
	}
	if !strings.Contains(src, "catch (_) { return true; }") {
		t.Error("an unreadable localStorage should fall back to on for an advisory check")
	}
	if strings.Contains(src, "setItem") {
		t.Error("updateCheckOn writes to storage; reading a preference should not create it")
	}
	if !strings.Contains(uiFuncSrc(t, "maybeShowUpdateNotice"), "if (!updateCheckOn()") {
		t.Error("the notice does not honour the preference")
	}
	if strings.Contains(indexHTML, "'/api/update-check'") {
		t.Error("the preference became a node setting; it governs this browser's outbound request, not the node's behaviour")
	}
}

// The toggle lives in Settings > General, next to the other browser
// preference rather than among the proxied node settings.
func TestUpdateCheckToggleIsInSettingsGeneral(t *testing.T) {
	src := uiFuncSrc(t, "secSettingsGeneral")
	if !strings.Contains(src, `id="update-check-row"`) {
		t.Fatal("the update-check toggle is not on the General settings tab")
	}
	if !strings.Contains(src, "setUpdateCheck(ucCb.checked)") {
		t.Error("the toggle does not persist the preference")
	}
	if !strings.Contains(src, "ucCb.checked = updateCheckOn();") {
		t.Error("the toggle does not reflect the stored preference when the tab renders")
	}
	// It must sit above the Logging card: everything from there down is node
	// configuration that gets proxied to the selected peer, and a browser
	// preference filed among those reads as one of them.
	dark := strings.Index(src, `id="dark-mode-row"`)
	upd := strings.Index(src, `id="update-check-row"`)
	logging := strings.Index(src, "<h3>Logging</h3>")
	if !(dark < upd && upd < logging) {
		t.Error("the update-check toggle is not between Dark mode and the Logging card")
	}
	// Its description is a settings-desc, so v908 hides it with help off.
	if m := regexp.MustCompile(`Check for updates at login</div>\s*'\s*\+\s*'<div class="([^"]+)"`).FindStringSubmatch(src); m == nil || !strings.Contains(m[1], "settings-desc") {
		t.Error("the toggle's explanation is not a settings-desc, so it will not hide with the rest of the prose")
	}
}
