package webadmin

import (
	"regexp"
	"strings"
	"testing"
)

// Chrome and Firefox both offer "prevent this page from creating additional
// dialogs" once a page has shown a few, and this page shows a great many —
// nearly every action reports its outcome through alert(). Once that is
// active, window.confirm renders nothing and returns false, and window.alert
// is a no-op.
//
// That turned every confirm-guarded action into a click that did nothing, with
// no dialog, no error, and nothing to distinguish it from a broken button. The
// fleet rollout was reported as "it never gets to pushing" for exactly this
// reason: its first statement was a confirm() guard, which returned before the
// button label changed or anything was sent. It was reproduced in jsdom
// against the real handler with confirm stubbed to false, and the fix verified
// the same way.

// upgradeHandlerSrc returns the click handler that drives the rollout.
func upgradeHandlerSrc(t *testing.T) string {
	t.Helper()
	src := uiFuncSrc(t, "drawUpgrade")
	i := strings.Index(src, "upgradeBtn.onclick = async () => {")
	if i < 0 {
		t.Fatal("the upgrade click handler is not in drawUpgrade any more")
	}
	j := strings.Index(src[i:], "\n  if (remote){")
	if j < 0 {
		return src[i:]
	}
	return src[i : i+j]
}

// Nothing on the rollout path may depend on a native dialog, in either
// direction: a suppressed confirm answers no on the operator's behalf, and a
// suppressed alert throws away an outcome they needed to read.
func TestUpgradePathUsesNoNativeDialogs(t *testing.T) {
	h := upgradeHandlerSrc(t)
	native := regexp.MustCompile(`(?:^|[^.\w])(confirm|alert)\(`)
	if m := native.FindAllStringSubmatch(h, -1); m != nil {
		var found []string
		for _, x := range m {
			found = append(found, x[1])
		}
		t.Errorf("the upgrade path still calls native %v; a suppressed dialog answers for the operator and the click silently does nothing", found)
	}
	if !strings.Contains(h, "confirmModal(") {
		t.Error("the rollout is not guarded by confirmModal")
	}
}

// The replacement has to be a real dismissal, not a default. Escape, the
// backdrop and the close button all mean no — the point is that a person did
// it rather than the browser.
func TestConfirmModalResolvesOnDismissal(t *testing.T) {
	src := uiFuncSrc(t, "confirmModal")
	if !strings.Contains(src, "() => finish(false))") {
		t.Error("confirmModal does not resolve when the modal is dismissed, so a dismissed prompt hangs the click forever")
	}
	if !strings.Contains(src, "let done = false;") || !strings.Contains(src, "if (done) return;") {
		t.Error("confirmModal can resolve twice; clicking OK also runs the close handler")
	}
	if !strings.Contains(src, "ok.onclick = () => { finish(true); m.close(); };") {
		t.Error("confirmModal's accept path does not resolve true and close")
	}
	if !strings.Contains(uiFuncSrc(t, "noticeModal"), "confirmModal(") {
		t.Error("noticeModal is not built on confirmModal, so alerts and confirms can drift apart")
	}
	// A notice has no Cancel: its only outcome is acknowledgement, and a
	// Cancel button would imply the action could still be called off.
	if !strings.Contains(src, "if (!o.noticeOnly){") {
		t.Error("confirmModal always renders Cancel, so notices offer a choice that does not exist")
	}
}

// A click that returns early must not leave the last rollout's progress line
// on screen. Read as live, "Building on 17 peer(s)… 0 of 17" is precisely what
// made a dead click look like a stuck rollout.
func TestUpgradeClearsStaleResultsBeforeReturningEarly(t *testing.T) {
	h := upgradeHandlerSrc(t)
	clear := strings.Index(h, "if (resBox) resBox.innerHTML = '';")
	if clear < 0 {
		t.Fatal("the handler no longer clears the previous rollout's results")
	}
	// Every early return has to come after the clear, or the stale line
	// survives that path.
	for _, guard := range []string{
		"if (!fileIn.files[0])",
		"confirmModal(msg, { title: allThenLocal",
		"confirmModal('Build the selected archive",
	} {
		if at := strings.Index(h, guard); at < 0 {
			t.Errorf("guard %q not found", guard)
		} else if at < clear {
			t.Errorf("guard %q can return before the stale results are cleared", guard)
		}
	}
}

// v916 converted the whole UI, not just the upgrade path. Every native dialog
// is gone: 14 confirms and 187 alerts.
func TestNoNativeDialogsAnywhereInTheUI(t *testing.T) {
	// Excludes property access (foo.alert) and the modal helpers themselves.
	call := regexp.MustCompile(`(?:^|[^.\w])(confirm|alert)\(`)
	var bad []string
	for i, line := range strings.Split(indexHTML, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "*") {
			continue // prose about the dialogs is fine; calls are not
		}
		if m := call.FindStringSubmatch(line); m != nil && !strings.Contains(line, m[1]+"Modal(") {
			bad = append(bad, strings.TrimSpace(line))
			_ = i
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d native dialog call(s) are back; each is one browser suppression away from silently doing nothing:\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// Every confirm has to be awaited. An un-awaited confirmModal returns a
// Promise, which is always truthy — so `if (!confirmModal(...)) return;` would
// never stop anything, turning a guard into a rubber stamp on destructive
// actions. That is a worse failure than the bug this replaced.
func TestEveryConfirmModalIsAwaited(t *testing.T) {
	var bad []string
	for _, line := range strings.Split(indexHTML, "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "//") || strings.HasPrefix(s, "*") {
			continue
		}
		if !strings.Contains(line, "confirmModal(") {
			continue
		}
		if strings.Contains(line, "function confirmModal") || strings.Contains(line, "return confirmModal(") {
			continue // the definition, and noticeModal delegating to it
		}
		if !strings.Contains(line, "await confirmModal(") {
			bad = append(bad, s)
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d confirmModal call(s) are not awaited; a Promise is always truthy, so the guard would pass every time:\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// A notice offers no Cancel, and its default title names the section it came
// from so no call site has to pass one.
func TestNoticeModalShape(t *testing.T) {
	if !strings.Contains(uiFuncSrc(t, "noticeModal"), "noticeOnly: true") {
		t.Error("noticeModal does not suppress Cancel; a notice would offer a choice that does not exist")
	}
	if !strings.Contains(uiFuncSrc(t, "confirmModal"), "o.title || helpModalTitle()") {
		t.Error("no default title, so ~200 converted call sites would render an empty modal heading")
	}
	if !strings.Contains(uiFuncSrc(t, "helpModalTitle"), "sectionHeading(state.section)") {
		t.Error("the default title is not the current section's heading")
	}
	if !strings.Contains(uiFuncSrc(t, "helpModalTitle"), "catch (_)") {
		t.Error("helpModalTitle can throw during early render, which would break every modal that relies on the default")
	}
}
