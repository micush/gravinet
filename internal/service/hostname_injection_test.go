package service

import (
	"os"
	"strings"
	"testing"
)

// CodeQL's go/command-injection flagged the Windows rename, which built a
// PowerShell command as "Rename-Computer -NewName '" + name + "' -Force".
//
// It was not exploitable. validHostname runs first and permits only letters,
// digits, hyphens and dots, and TestValidHostnameRejectsInjection above
// already pins that — including "host'name", with a comment naming this exact
// PowerShell quote. The validator is not retested here.
//
// What was missing is that the safety lived 750 lines from the use, leaving it
// conditional on a reader finding the validator and on nobody ever loosening
// it. v920 passes the name through the environment instead, following the
// convention psRun already uses for passwords, so the property is local and
// holds whatever the validator permits.

// The name must not appear in the script text at all.
func TestWindowsRenameDoesNotInterpolateTheName(t *testing.T) {
	branch := setHostnameWindowsBranch(t)
	if strings.Contains(branch, "+name+") {
		t.Error("the hostname is interpolated into the PowerShell command again; pass it through the environment as psRun does")
	}
	if !strings.Contains(branch, "$env:GRAVINET_NEW_HOSTNAME") {
		t.Error("the rename script does not read the name from the environment")
	}
	if !strings.Contains(branch, `cmd.Env = append(os.Environ(), "GRAVINET_NEW_HOSTNAME="+name)`) {
		t.Error("the name is not supplied through the environment")
	}
}

// setHostnameWindowsBranch returns the windows case of SetHostname — anchored
// inside that function, because the file has several other windows branches
// and matching the first one would inspect the wrong code.
func setHostnameWindowsBranch(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("hostresolver.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := strings.Index(body, "func SetHostname(")
	if fn < 0 {
		t.Fatal("SetHostname not found")
	}
	end := strings.Index(body[fn+1:], "\nfunc ")
	if end < 0 {
		t.Fatal("could not bound SetHostname")
	}
	fnBody := body[fn : fn+1+end]
	i := strings.Index(fnBody, `case "windows":`)
	if i < 0 {
		t.Fatal("SetHostname has no windows branch")
	}
	return fnBody[i:]
}

// Validation has to run before the OS dispatch: it protects the argv call
// sites too — sysrc, scutil and hostname all take the name as an argument,
// where a leading hyphen would be read as an option — not just the PowerShell
// one. The existing tests check validHostname itself; this checks that
// SetHostname actually consults it, and consults it first.
func TestSetHostnameValidatesBeforeDispatch(t *testing.T) {
	src, err := os.ReadFile("hostresolver.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func SetHostname(")
	if i < 0 {
		t.Fatal("SetHostname not found")
	}
	fn := body[i : i+1200]
	check := strings.Index(fn, "validHostname(name)")
	dispatch := strings.Index(fn, "switch runtime.GOOS")
	if check < 0 || dispatch < 0 {
		t.Fatal("SetHostname's shape changed")
	}
	if check > dispatch {
		t.Error("validHostname runs after the OS dispatch, so an unvalidated name reaches the command")
	}
	// And it is a refusal, not a note alongside success.
	if ok, _ := SetHostname("x'; whoami; '"); ok {
		t.Error("SetHostname accepted a name containing shell metacharacters")
	}
	if ok, _ := SetHostname("-flag"); ok {
		t.Error("SetHostname accepted a name that would be read as a command-line flag")
	}
}
