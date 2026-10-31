package webadmin

import (
	"strings"
	"testing"
)

// The theme used to be detected once and then pinned: the start-up code wrote
// whatever prefers-color-scheme said into localStorage, so it read back as a
// choice on every later visit and an OS switch to dark mode never reached the
// page. Auto (no stored value) must follow the OS live, and only an explicit
// Light or Dark may be stored.
func TestThemeAutoFollowsTheOS(t *testing.T) {
	apply := uiFuncSrc(t, "applyTheme")
	if !strings.Contains(apply, "themeMQ.matches") {
		t.Error("applyTheme does not consult prefers-color-scheme in Auto")
	}
	if strings.Contains(apply, "setItem") {
		t.Error("applyTheme writes to storage; applying the detected theme must not turn it into a stored choice")
	}
	if !strings.Contains(indexHTML, "themeMQ.addEventListener('change', applyTheme)") {
		t.Error("Auto does not listen for the OS switching between light and dark")
	}
	set := uiFuncSrc(t, "setThemeMode")
	if !strings.Contains(set, "removeItem(THEME_KEY)") {
		t.Error("choosing Auto does not clear the stored choice")
	}
	if !strings.Contains(indexHTML, "localStorage.removeItem('gravinet-theme')") {
		t.Error("the old auto-pinned 'gravinet-theme' key is not dropped")
	}
}

func TestThemeRowOffersAutoLightDark(t *testing.T) {
	src := uiFuncSrc(t, "secSettingsGeneral")
	for _, opt := range []string{`<option value="auto">Auto</option>`, `<option value="light">Light</option>`, `<option value="dark">Dark</option>`} {
		if !strings.Contains(src, opt) {
			t.Errorf("the Theme row is missing %s", opt)
		}
	}
	if !strings.Contains(src, "thSel.value = themeMode();") {
		t.Error("the Theme row does not show the current choice when the tab renders")
	}
}
