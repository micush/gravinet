//go:build darwin

package service

import "testing"

// TestInstallPathUsesLaunchdLabel is darwin-only because InstallPath branches
// on runtime.GOOS internally — see service_test.go's
// TestLaunchdLabelConsistentAcrossUses for the cross-platform half of this
// (LaunchdPlist deriving its Label from the same helper).
func TestInstallPathUsesLaunchdLabel(t *testing.T) {
	o := testOpts()
	want := "/Library/LaunchDaemons/" + LaunchdLabel(o) + ".plist"
	if got := InstallPath(o); got != want {
		t.Fatalf("InstallPath = %q, want %q (built from LaunchdLabel)", got, want)
	}
}
