package service

// GravinetGroup gating: which local OS accounts may sign in under
// gravinet's system-auth modes (pam on linux/macos/freebsd, bsd_auth on
// openbsd, LogonUser on windows) is decided by membership in a single OS
// group, "gravinet" — plus root, which always may, matching the Unix
// convention that a break-glass root login must never depend on group
// bookkeeping being right (parapet, whose Users page this one mirrors, makes
// the identical exception for the identical reason).
//
// This replaced an earlier design based on web_admin.allow_users, a config
// list gravinet owned and checked against a snapshot built once at startup.
// The group is better on both counts a config list was worse at: it's the
// OS's own concept of "who's allowed to do this," so `usermod -aG gravinet
// bob` by hand and a page click here mean exactly the same thing with no
// gravinet-specific bookkeeping to keep in sync; and because IsGroupMember
// checks live, on every login attempt, rather than a map built once in
// New(), a membership change takes effect immediately — no restart, unlike
// almost everything else this admin UI edits.
//
// System > Users manages this group's membership; it does not manage
// web_admin.allow_users at all anymore. That config field still exists
// (JSON backward-compatibility for old config files) but is no longer
// consulted for pam/system/windows auth — see auth_pam.go, auth_bsdauth.go,
// and auth_windows.go's Authenticate methods, which call IsGroupMember
// instead of checking an allow map.
//
// Membership is read from two sources on every unix platform, not one:
// the group's own supplementary member list, AND any account whose
// *primary* group happens to be this one (which never shows up in the
// supplementary list). Accounts created through this page are never given
// gravinet as a primary group — only ever added as a supplementary member —
// so the primary-group case only matters for an account an operator
// configured differently by hand; it's checked anyway, for the same
// completeness reason parapet's own group_members() checks both.

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const GravinetGroup = "gravinet"

// IsGroupMember reports whether name may sign in under a system-auth mode:
// root always may; anyone else only while they're currently a member of
// GravinetGroup. Checked fresh against the OS on every call — deliberately
// not cached — so this is safe (and intended) to call on every login
// attempt. A host with no group tooling at all, or where the group simply
// doesn't exist yet, fails closed: membership reads as empty rather than
// erroring open, so "can't tell" never accidentally means "anyone may."
func IsGroupMember(name string) bool {
	if name == "root" {
		return true
	}
	members, _ := groupMembers(GravinetGroup)
	for _, m := range members {
		if m == name {
			return true
		}
	}
	return false
}

// EnsureGravinetGroup creates GravinetGroup if it doesn't already exist.
// Idempotent and safe to call on every daemon startup and every System >
// Users page load: a node that reaches this feature by an in-place upgrade
// rather than a fresh install was never touched by an installer that could
// have created the group ahead of time (the same bootstrapping gap v607's
// SyncInstalledDocs closed for installed doc files — a capability added
// after a node's last install has to make itself exist at runtime, not
// assume the installer already did it). Returns (true, "") once the group
// exists (whether it already did or was just created), or (false, hint) if
// it could not be created.
func EnsureGravinetGroup() (bool, string) {
	if ok, _ := groupExists(GravinetGroup); ok {
		return true, ""
	}
	return createGroup(GravinetGroup)
}

// groupExists reports whether group is a real OS group on this host.
func groupExists(group string) (bool, error) {
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd":
		lines, err := readGroupFileLines()
		if err != nil {
			return false, err
		}
		for _, ln := range lines {
			if f := strings.SplitN(ln, ":", 2); len(f) > 0 && f[0] == group {
				return true, nil
			}
		}
		return false, nil
	case "darwin":
		err := exec.Command("dscl", ".", "-read", "/Groups/"+group).Run()
		return err == nil, nil
	case "windows":
		err := exec.Command("net", "localgroup", group).Run()
		return err == nil, nil
	default:
		return false, nil
	}
}

func createGroup(group string) (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("groupadd", group).CombinedOutput(); err != nil {
			return false, cmdErr("groupadd", out, err)
		}
	case "freebsd":
		if out, err := exec.Command("pw", "groupadd", group).CombinedOutput(); err != nil {
			return false, cmdErr("pw groupadd", out, err)
		}
	case "openbsd":
		if out, err := exec.Command("groupadd", group).CombinedOutput(); err != nil {
			return false, cmdErr("groupadd", out, err)
		}
	case "darwin":
		return createDarwinGroup(group)
	case "windows":
		if out, err := exec.Command("net", "localgroup", group, "/add").CombinedOutput(); err != nil {
			return false, cmdErr("net localgroup /add", out, err)
		}
	default:
		return false, "group management isn't supported on this operating system"
	}
	return true, ""
}

func createDarwinGroup(group string) (bool, string) {
	gid, hint := nextDarwinGID()
	if hint != "" {
		return false, hint
	}
	path := "/Groups/" + group
	steps := [][]string{
		{"-create", path},
		{"-create", path, "PrimaryGroupID", gid},
	}
	for _, args := range steps {
		full := append([]string{"."}, args...)
		if out, err := exec.Command("dscl", full...).CombinedOutput(); err != nil {
			exec.Command("dscl", ".", "-delete", path).Run() // best-effort cleanup of a partial create
			return false, cmdErr("dscl "+args[0], out, err)
		}
	}
	return true, ""
}

// nextDarwinGID picks a GroupID one above the current highest, mirroring
// nextDarwinUID's approach for new user accounts (see sysusers.go), starting
// no lower than 501.
func nextDarwinGID() (string, string) {
	out, err := exec.Command("dscl", ".", "-list", "/Groups", "PrimaryGroupID").CombinedOutput()
	if err != nil {
		return "", cmdErr("dscl -list", out, err)
	}
	max := 500
	for _, ln := range strings.Split(string(out), "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		if n, err := strconv.Atoi(fields[len(fields)-1]); err == nil && n > max {
			max = n
		}
	}
	return strconv.Itoa(max + 1), ""
}

// addToGroup makes name a (supplementary) member of group. Idempotent in
// effect on every backend used here: re-adding an existing member is a
// harmless no-op rather than an error.
func addToGroup(name, group string) (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("gpasswd", "-a", name, group).CombinedOutput(); err != nil {
			return false, cmdErr("gpasswd -a", out, err)
		}
	case "freebsd":
		if out, err := exec.Command("pw", "groupmod", group, "-m", name).CombinedOutput(); err != nil {
			return false, cmdErr("pw groupmod -m", out, err)
		}
	case "openbsd":
		return setOpenBSDSupplementalGroups(name, group, true)
	case "darwin":
		if out, err := exec.Command("dscl", ".", "-append", "/Groups/"+group, "GroupMembership", name).CombinedOutput(); err != nil {
			return false, cmdErr("dscl -append", out, err)
		}
	case "windows":
		if out, err := exec.Command("net", "localgroup", group, name, "/add").CombinedOutput(); err != nil {
			return false, cmdErr("net localgroup /add", out, err)
		}
	default:
		return false, "group management isn't supported on this operating system"
	}
	return true, ""
}

// removeFromGroup drops name from group's supplementary membership. Called
// as a best-effort, belt-and-suspenders step after DeleteSystemUser's
// userdel/dscl-delete/pw-userdel already removed the account entirely (which
// on every platform here also drops it from every group on its own) — so a
// failure here is logged by the caller at most, never treated as the
// operation having failed.
func removeFromGroup(name, group string) (bool, string) {
	switch runtime.GOOS {
	case "linux":
		if out, err := exec.Command("gpasswd", "-d", name, group).CombinedOutput(); err != nil {
			return false, cmdErr("gpasswd -d", out, err)
		}
	case "freebsd":
		return setFreeBSDGroupMembers(group, name, false)
	case "openbsd":
		return setOpenBSDSupplementalGroups(name, group, false)
	case "darwin":
		if out, err := exec.Command("dscl", ".", "-delete", "/Groups/"+group, "GroupMembership", name).CombinedOutput(); err != nil {
			return false, cmdErr("dscl -delete", out, err)
		}
	case "windows":
		if out, err := exec.Command("net", "localgroup", group, name, "/delete").CombinedOutput(); err != nil {
			return false, cmdErr("net localgroup /delete", out, err)
		}
	default:
		return false, "group management isn't supported on this operating system"
	}
	return true, ""
}

// setFreeBSDGroupMembers adds or removes name from group's membership by
// reading the current full list and writing it back with `pw groupmod -M`
// (replace-all), rather than trusting a per-member delete flag: pw(8)'s
// groupmod has no documented single-member removal option the way `-m`
// unambiguously means add, so read-modify-write-the-whole-list is the
// version of this that doesn't depend on an assumption.
func setFreeBSDGroupMembers(group, name string, add bool) (bool, string) {
	out, err := exec.Command("pw", "groupshow", group).CombinedOutput()
	if err != nil {
		return false, cmdErr("pw groupshow", out, err)
	}
	// name:*:gid:member1,member2
	f := strings.SplitN(strings.TrimSpace(string(out)), ":", 4)
	var members []string
	if len(f) == 4 && f[3] != "" {
		members = strings.Split(f[3], ",")
	}
	if add {
		if !containsStr(members, name) {
			members = append(members, name)
		}
	} else {
		members = removeStr(members, name)
	}
	if o, err := exec.Command("pw", "groupmod", group, "-M", strings.Join(members, ",")).CombinedOutput(); err != nil {
		return false, cmdErr("pw groupmod -M", o, err)
	}
	return true, ""
}

// setOpenBSDSupplementalGroups adds or removes group from name's full
// supplementary-group list via `usermod -G`, which — unlike Linux's `usermod
// -aG` — replaces the whole list rather than appending to it, so this reads
// the account's current groups first (via `id -Gn`) and writes the complete
// resulting list back.
func setOpenBSDSupplementalGroups(name, group string, add bool) (bool, string) {
	out, err := exec.Command("id", "-Gn", name).CombinedOutput()
	if err != nil {
		return false, cmdErr("id -Gn", out, err)
	}
	current := strings.Fields(string(out))
	var next []string
	if add {
		next = current
		if !containsStr(next, group) {
			next = append(next, group)
		}
	} else {
		next = removeStr(current, group)
	}
	if o, err := exec.Command("usermod", "-G", strings.Join(next, ","), name).CombinedOutput(); err != nil {
		return false, cmdErr("usermod -G", o, err)
	}
	return true, ""
}

// groupMembers returns every member of group on this host, from both
// sources described in this file's package comment: the group's own
// supplementary member list, and (linux/freebsd/openbsd/darwin) any account
// whose primary group is this one. known is false only when the read itself
// couldn't be attempted at all (e.g. no group tooling on this platform);
// a group that exists but has no members is (empty slice, true), not
// (nil, false) — those are different answers and the System > Users page
// (via ListSystemUsers's own Can*/Known fields) needs to tell them apart.
func groupMembers(group string) ([]string, bool) {
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd":
		return unixGroupMembers(group)
	case "darwin":
		return darwinGroupMembers(group)
	case "windows":
		return windowsGroupMembers(group)
	default:
		return nil, false
	}
}

// unixGroupMembers implements groupMembers for linux/freebsd/openbsd, all of
// which share the standard /etc/group and /etc/passwd formats.
func unixGroupMembers(group string) ([]string, bool) {
	lines, err := readGroupFileLines()
	if err != nil {
		return nil, false
	}
	var gid string
	var members []string
	for _, ln := range lines {
		f := strings.SplitN(ln, ":", 4)
		if len(f) < 3 || f[0] != group {
			continue
		}
		gid = f[2]
		if len(f) == 4 && f[3] != "" {
			members = strings.Split(f[3], ",")
		}
	}
	if gid == "" {
		return nil, false // group doesn't exist
	}
	// Primary-group members: anyone in /etc/passwd whose gid field matches,
	// not already counted above.
	pw, err := os.ReadFile(passwdFilePath)
	if err == nil {
		for _, ln := range strings.Split(string(pw), "\n") {
			f := strings.Split(ln, ":")
			if len(f) < 4 {
				continue
			}
			if f[3] == gid && !containsStr(members, f[0]) {
				members = append(members, f[0])
			}
		}
	}
	return members, true
}

// groupFilePath and passwdFilePath are /etc/group and /etc/passwd, as
// variables so tests can point them at fixtures instead of the real files —
// which, unlike sysusers.go's shadowPath, this package's own tests actively
// need to do: groups.go's mutating operations shell out to real, privileged
// system tools (groupadd, gpasswd, ...), so no automated test here may ever
// risk exercising one against the machine running the tests. Read-side
// parsing is tested against fixtures through these vars instead.
var groupFilePath = "/etc/group"
var passwdFilePath = "/etc/passwd"

func readGroupFileLines() ([]string, error) {
	b, err := os.ReadFile(groupFilePath)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(b), "\n"), nil
}

// darwinGroupMembers reads a macOS group's GroupMembership list via dscl,
// plus any account whose PrimaryGroupID matches this group's own — the same
// two-source read as the flat-file platforms, via Directory Services
// equivalents of /etc/group and /etc/passwd.
func darwinGroupMembers(group string) ([]string, bool) {
	out, err := exec.Command("dscl", ".", "-read", "/Groups/"+group, "GroupMembership").CombinedOutput()
	if err != nil {
		return nil, false // group doesn't exist, or dscl unavailable
	}
	members := strings.Fields(strings.TrimPrefix(strings.TrimSpace(string(out)), "GroupMembership:"))

	gidOut, err := exec.Command("dscl", ".", "-read", "/Groups/"+group, "PrimaryGroupID").CombinedOutput()
	if err != nil {
		return members, true
	}
	gid := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(gidOut)), "PrimaryGroupID:"))
	if gid == "" {
		return members, true
	}
	listOut, err := exec.Command("dscl", ".", "-list", "/Users", "PrimaryGroupID").CombinedOutput()
	if err != nil {
		return members, true
	}
	for _, ln := range strings.Split(string(listOut), "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == gid && !containsStr(members, fields[0]) {
			members = append(members, fields[0])
		}
	}
	return members, true
}

// windowsGroupMembers parses `net localgroup <group>`'s fixed-format output:
// a header block, a line of dashes, one member per line, then a trailing
// status line ("The command completed successfully."). Members are
// collected strictly between the dash line and that trailing line, so
// neither is ever mistaken for an account name.
func windowsGroupMembers(group string) ([]string, bool) {
	out, err := exec.Command("net", "localgroup", group).CombinedOutput()
	if err != nil {
		return nil, false // group doesn't exist, or net unavailable
	}
	lines := strings.Split(string(out), "\n")
	inMembers := false
	var members []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if !inMembers {
			if strings.HasPrefix(t, "----") {
				inMembers = true
			}
			continue
		}
		if t == "" || strings.HasPrefix(t, "The command completed") {
			continue
		}
		members = append(members, t)
	}
	return members, true
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func removeStr(list []string, s string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
