package service

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// groupFixturePaths points groupFilePath/passwdFilePath at fixtures for the
// duration of the calling test, restoring them afterward. Every test in this
// file uses this — none may ever read the real /etc/group or /etc/passwd,
// since groupMembers is meant to work identically whether or not this
// process has the privilege to read the real files.
func groupFixturePaths(t *testing.T, groupBody, passwdBody string) {
	t.Helper()
	dir := t.TempDir()
	gp := filepath.Join(dir, "group")
	pp := filepath.Join(dir, "passwd")
	if err := os.WriteFile(gp, []byte(groupBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp, []byte(passwdBody), 0o644); err != nil {
		t.Fatal(err)
	}
	prevG, prevP := groupFilePath, passwdFilePath
	groupFilePath, passwdFilePath = gp, pp
	t.Cleanup(func() { groupFilePath, passwdFilePath = prevG, prevP })
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// TestUnixGroupMembersSupplementalOnly pins the ordinary case: a group with
// an explicit member list in /etc/group, no primary-group members.
func TestUnixGroupMembersSupplementalOnly(t *testing.T) {
	groupFixturePaths(t,
		"wheel:*:0:root\ngravinet:*:900:bob,alice\nother:*:901:\n",
		"root:*:0:0::/root:/bin/sh\nbob:*:1001:100::/home/bob:/usr/sbin/nologin\nalice:*:1002:100::/home/alice:/usr/sbin/nologin\n",
	)
	members, known := groupMembers("gravinet")
	if !known {
		t.Fatal("expected known=true for an existing group")
	}
	if got, want := sorted(members), []string{"alice", "bob"}; !equalStr(got, want) {
		t.Errorf("members = %v, want %v", got, want)
	}
}

// TestUnixGroupMembersIncludesPrimaryGroupUsers pins the dual-source read:
// an account whose *primary* gid matches the group must show up even though
// it's absent from the group file's own member list — this is exactly the
// case parapet's own group_members() special-cases, for the same reason.
func TestUnixGroupMembersIncludesPrimaryGroupUsers(t *testing.T) {
	groupFixturePaths(t,
		"gravinet:*:900:bob\n",
		"bob:*:1001:900::/home/bob:/usr/sbin/nologin\n"+ // supplementary member AND primary gid 900 — must not double-count
			"carol:*:1002:900::/home/carol:/usr/sbin/nologin\n"+ // primary gid 900, absent from the member list
			"dave:*:1003:100::/home/dave:/usr/sbin/nologin\n", // unrelated gid — must not appear
	)
	members, known := groupMembers("gravinet")
	if !known {
		t.Fatal("expected known=true")
	}
	if got, want := sorted(members), []string{"bob", "carol"}; !equalStr(got, want) {
		t.Errorf("members = %v, want %v (bob once despite both sources, carol via primary gid, dave excluded)", got, want)
	}
}

// TestUnixGroupMembersUnknownGroup checks a group absent from /etc/group
// reads as (nil, false) — "doesn't exist," not "exists with zero members."
func TestUnixGroupMembersUnknownGroup(t *testing.T) {
	groupFixturePaths(t, "wheel:*:0:root\n", "root:*:0:0::/root:/bin/sh\n")
	members, known := groupMembers("gravinet")
	if known {
		t.Errorf("expected known=false for a group that doesn't exist, got members=%v", members)
	}
}

// TestUnixGroupMembersEmptyGroup checks a group that exists but has no
// members at all reads as (empty, true) — a real, different answer from
// "doesn't exist."
func TestUnixGroupMembersEmptyGroup(t *testing.T) {
	groupFixturePaths(t, "gravinet:*:900:\n", "root:*:0:0::/root:/bin/sh\n")
	members, known := groupMembers("gravinet")
	if !known {
		t.Fatal("expected known=true for an existing, empty group")
	}
	if len(members) != 0 {
		t.Errorf("members = %v, want empty", members)
	}
}

// TestGroupExists pins groupExists against the same two fixture cases.
func TestGroupExists(t *testing.T) {
	groupFixturePaths(t, "wheel:*:0:root\ngravinet:*:900:bob\n", "root:*:0:0::/root:/bin/sh\n")
	if ok, err := groupExists("gravinet"); err != nil || !ok {
		t.Errorf("groupExists(gravinet) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := groupExists("nonexistent"); err != nil || ok {
		t.Errorf("groupExists(nonexistent) = %v, %v; want false, nil", ok, err)
	}
}

// TestIsGroupMemberRootAlwaysTrue pins the one exception in IsGroupMember
// that never touches the OS at all — root must pass even when the fixture
// group file doesn't exist, can't be read, or simply has no such member.
func TestIsGroupMemberRootAlwaysTrue(t *testing.T) {
	prevG, prevP := groupFilePath, passwdFilePath
	groupFilePath, passwdFilePath = "/nonexistent/group", "/nonexistent/passwd"
	defer func() { groupFilePath, passwdFilePath = prevG, prevP }()
	if !IsGroupMember("root") {
		t.Error("root must always be a member, independent of any group file state")
	}
}

// TestIsGroupMemberFailsClosed checks that a non-root name is refused when
// the group can't be read at all — "can't tell" must never mean "let
// everyone in."
func TestIsGroupMemberFailsClosed(t *testing.T) {
	prevG, prevP := groupFilePath, passwdFilePath
	groupFilePath, passwdFilePath = "/nonexistent/group", "/nonexistent/passwd"
	defer func() { groupFilePath, passwdFilePath = prevG, prevP }()
	if IsGroupMember("bob") {
		t.Error("an unreadable group file must fail closed (deny), not open (allow)")
	}
}

func TestIsGroupMemberOrdinaryCases(t *testing.T) {
	groupFixturePaths(t, "gravinet:*:900:bob\n", "bob:*:1001:100::/home/bob:/usr/sbin/nologin\n")
	if !IsGroupMember("bob") {
		t.Error("bob is a member and should be allowed")
	}
	if IsGroupMember("alice") {
		t.Error("alice is not a member and should be refused")
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestContainsAndRemoveStr(t *testing.T) {
	list := []string{"a", "b", "c"}
	if !containsStr(list, "b") {
		t.Error("containsStr should find an existing element")
	}
	if containsStr(list, "z") {
		t.Error("containsStr should not find a missing element")
	}
	got := removeStr(list, "b")
	if want := []string{"a", "c"}; !equalStr(got, want) {
		t.Errorf("removeStr = %v, want %v", got, want)
	}
	// Removing something absent is a no-op, not an error.
	if got := removeStr(list, "z"); !equalStr(got, list) {
		t.Errorf("removeStr of a missing element changed the list: %v", got)
	}
}
