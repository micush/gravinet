package webadmin

import (
	"regexp"
	"strings"
	"testing"
)

// System > Upgrade shows the running version beside the newest tag on GitHub,
// with the tag linking to its source tarball — the same archive the picker
// directly above it expects.

// The line sits between the source picker and the peer selector, which is
// where it was asked for and also where it is useful: it names the version the
// picker is about to replace.
func TestUpgradeVersionLineSitsBetweenPickerAndPeers(t *testing.T) {
	src := uiFuncSrc(t, "secUpgrade")
	picker := strings.Index(src, "stCard.appendChild(up);")
	line := strings.Index(src, "renderUpgradeVersions(verLine)")
	peers := strings.Index(src, "Pick specific peers to upgrade")
	if picker < 0 || line < 0 || peers < 0 {
		t.Fatal("picker, version line or peer selector not found on the upgrade page")
	}
	if !(picker < line && line < peers) {
		t.Error("the version line is not between the source picker and the peer selector")
	}
}

// The running version must come from the node this page acts on. /api/about is
// not in LOCAL_API so it follows the peer selected in the header, while every
// /api/upgrade/* endpoint is local-only — left to default, the line would
// report a remote node's version beside a control that upgrades the local one.
func TestUpgradeVersionReadsTheLocalNode(t *testing.T) {
	src := uiFuncSrc(t, "renderUpgradeVersions")
	if !strings.Contains(src, "api('/api/about', {}, null)") {
		t.Error("the version lookup does not force the local node, so it reports the selected peer's version instead")
	}
	// The premise of the above: if /api/about ever became local-only, the
	// explicit target would be redundant and this test misleading.
	local := between(t, indexHTML, "const LOCAL_API = [", "];")
	if strings.Contains(local, "'/api/about'") {
		t.Error("/api/about is now in LOCAL_API; the explicit null target in renderUpgradeVersions is no longer what makes this correct")
	}
}

// The download URL is the tag's source tarball, built from the tag exactly as
// GitHub reported it. Normalising the tag here would 404 for any repository
// that tags without a leading v.
func TestUpgradeDownloadLinkTargetsTheTagTarball(t *testing.T) {
	src := uiFuncSrc(t, "renderUpgradeVersions")
	if !strings.Contains(src, "'https://github.com/'+GH_REPO+'/archive/refs/tags/'+encodeURIComponent(latest)+'.tar.gz'") {
		t.Error("the download link is not the tag's .tar.gz on GitHub, or the tag is not URL-encoded")
	}
	if !strings.Contains(src, `rel="noopener noreferrer"`) {
		t.Error("the outbound link has no rel=noopener noreferrer")
	}
	if !strings.Contains(indexHTML, "const GH_REPO = 'micush/gravinet';") {
		t.Error("GH_REPO is not the upstream repository")
	}
}

// An offline or rate-limited node must still show what it is running: that
// half is useful alone, and an offline node is exactly where the GitHub
// lookup fails.
func TestUpgradeVersionSurvivesAFailedLookup(t *testing.T) {
	src := uiFuncSrc(t, "renderUpgradeVersions")
	if !strings.Contains(src, "if (!latest){") || !strings.Contains(src, "could not be checked") {
		t.Error("a failed GitHub lookup has no fallback, so the running version is lost with it")
	}
	if strings.Index(src, "const latest = await latestGravinetTag()") < strings.Index(src, "if (!cur){") {
		t.Error("the GitHub lookup runs before the local version is resolved; a hang there would delay both")
	}
	look := uiFuncSrc(t, "latestGravinetTag")
	if !strings.Contains(look, "catch (_)") {
		t.Error("latestGravinetTag can throw, which would leave the line showing its placeholder forever")
	}
}

// Tags are picked by number, not by position or string order: the tags
// endpoint orders by GitHub's own rules, and as strings v1000 sorts below
// v999.
func TestLatestTagIsChosenNumerically(t *testing.T) {
	src := uiFuncSrc(t, "latestGravinetTag")
	if !strings.Contains(src, "parseInt(") || !strings.Contains(src, "n > bestN") {
		t.Error("the tags fallback does not choose the highest tag numerically")
	}
	if !strings.Contains(src, "releases/latest") {
		t.Error("releases are not consulted first, so a repository that marks a latest release is ignored")
	}
	// Cached for the page's life — this section re-renders on every visit and
	// GitHub allows 60 unauthenticated requests an hour per address.
	if !strings.Contains(src, "if (state.latestTag !== undefined) return state.latestTag;") {
		t.Error("the GitHub lookup is not cached, so revisiting the page re-requests it")
	}
}

// The line reports live state, so it must not be hidden with the help text.
func TestUpgradeVersionLineIsNotHelpText(t *testing.T) {
	src := uiFuncSrc(t, "secUpgrade")
	m := regexp.MustCompile(`verLine = \$\('<div class="([^"]*)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("the version line's markup changed shape")
	}
	for _, cls := range []string{"help-desc", "help-topic", "settings-desc"} {
		if strings.Contains(m[1], cls) {
			t.Errorf("the version line is classed %q, so it disappears when help is off", cls)
		}
	}
}

// The line ends at the two version numbers. No verdict, badge or "up to date"
// marker after them — the reader can compare two numbers that are side by
// side, and a restatement is a third thing to render, keep correct and read
// past. Removing it in v910 is also why nothing here may reintroduce a
// comparison between cur and latest for display purposes.
func TestUpgradeVersionLineHasNoVerdict(t *testing.T) {
	src := uiFuncSrc(t, "renderUpgradeVersions")
	for _, bad := range []string{"up to date", "vs(cur) === vs(latest)", "cur === latest"} {
		if strings.Contains(src, bad) {
			t.Errorf("the version line restates the comparison (%q); the two numbers already show it", bad)
		}
	}
}
