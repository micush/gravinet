package main

import (
	"reflect"
	"strings"
	"testing"
)

// This file covers the v784 change: values that were mandatory but spelled as
// flags became positional arguments. The three pieces with a rule in them —
// splitPositionals, upgradeRoute, upgradeArchiveArg — are tested here; the
// commands that use them are control-socket or config-file calls, and the
// argument handling is the part that changed.

// TestSplitPositionalsSkipsFlagValues is the reason splitPositionals exists
// rather than splitPositional being reused. splitPositional takes the first
// token without a leading dash, which is a flag's *value* whenever a
// value-carrying flag comes first — so "gravinet upgrade -sock /run/x.sock
// archive.tgz" would have tried to build the socket path.
func TestSplitPositionalsSkipsFlagValues(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		valueFlags []string
		wantPos    []string
		wantRest   []string
	}{
		{
			name:       "value flag before the positional",
			args:       []string{"-sock", "/run/x.sock", "archive.tgz"},
			valueFlags: []string{"sock"},
			wantPos:    []string{"archive.tgz"},
			wantRest:   []string{"-sock", "/run/x.sock"},
		},
		{
			name:       "positional first, bool flag after",
			args:       []string{"archive.tgz", "-dry-run"},
			valueFlags: []string{"sock"},
			wantPos:    []string{"archive.tgz"},
			wantRest:   []string{"-dry-run"},
		},
		{
			name:       "positional sandwiched between flags",
			args:       []string{"-sock", "/run/x.sock", "archive.tgz", "-dry-run"},
			valueFlags: []string{"sock"},
			wantPos:    []string{"archive.tgz"},
			wantRest:   []string{"-sock", "/run/x.sock", "-dry-run"},
		},
		{
			// "-sock=/path" carries its own value, so the next token is a
			// real argument and must not be swallowed as one.
			name:       "attached value does not consume the next token",
			args:       []string{"-sock=/run/x.sock", "archive.tgz"},
			valueFlags: []string{"sock"},
			wantPos:    []string{"archive.tgz"},
			wantRest:   []string{"-sock=/run/x.sock"},
		},
		{
			name:       "two positionals both returned, in order",
			args:       []string{"7", "2", "-net", "corp"},
			valueFlags: []string{"net"},
			wantPos:    []string{"7", "2"},
			wantRest:   []string{"-net", "corp"},
		},
		{
			name:       "a flag not named as value-carrying keeps its neighbour",
			args:       []string{"-mgmt", "ospf"},
			valueFlags: []string{"proto", "port"},
			wantPos:    []string{"ospf"},
			wantRest:   []string{"-mgmt"},
		},
		{
			name:       "double-dash spelling is recognised too",
			args:       []string{"--sock", "/run/x.sock", "archive.tgz"},
			valueFlags: []string{"sock"},
			wantPos:    []string{"archive.tgz"},
			wantRest:   []string{"--sock", "/run/x.sock"},
		},
		{
			// A value-carrying flag with nothing after it must not read past
			// the end of the slice.
			name:       "trailing value flag with no value",
			args:       []string{"-sock"},
			valueFlags: []string{"sock"},
			wantPos:    nil,
			wantRest:   []string{"-sock"},
		},
		{
			name:       "no arguments at all",
			args:       nil,
			valueFlags: []string{"sock"},
			wantPos:    nil,
			wantRest:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos, rest := splitPositionals(tc.args, tc.valueFlags...)
			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positionals = %q, want %q", pos, tc.wantPos)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("residue = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// TestSplitPositionalsPreservesResidueOrder guards the property the residue is
// handed to flag.FlagSet.Parse for: every flag and every flag value still
// present, still adjacent, still in the order they were written.
func TestSplitPositionalsPreservesResidueOrder(t *testing.T) {
	args := []string{"-dry-run", "-sock", "/run/x.sock", "archive.tgz", "-allow-downgrade"}
	pos, rest := splitPositionals(args, "sock", "src")
	if len(pos) != 1 || pos[0] != "archive.tgz" {
		t.Fatalf("positionals = %q, want [archive.tgz]", pos)
	}
	want := []string{"-dry-run", "-sock", "/run/x.sock", "-allow-downgrade"}
	if !reflect.DeepEqual(rest, want) {
		t.Fatalf("residue = %q, want %q", rest, want)
	}
}

// TestUpgradeRouteTreatsAnArchiveAsAnArchive is the headline change: the
// archive is the argument, with no "apply" verb and no -src flag in front
// of it.
func TestUpgradeRouteTreatsAnArchiveAsAnArchive(t *testing.T) {
	cases := []struct {
		args     []string
		wantVerb string
		wantRest []string
	}{
		// The new spelling, in the shapes an operator would actually type.
		{[]string{"/home/mcc/Downloads/gravinet-v781.tgz"}, "apply", []string{"/home/mcc/Downloads/gravinet-v781.tgz"}},
		{[]string{"./gravinet-src.tgz", "-dry-run"}, "apply", []string{"./gravinet-src.tgz", "-dry-run"}},
		{[]string{"src.zip"}, "apply", []string{"src.zip"}},

		// Verbs still win, exactly and by unambiguous prefix (prefix.go).
		{[]string{"status"}, "status", []string{}},
		{[]string{"rollback"}, "rollback", []string{}},
		{[]string{"stat"}, "status", []string{}},
		{[]string{"roll"}, "rollback", []string{}},
		{[]string{"status", "-sock", "/run/x.sock"}, "status", []string{"-sock", "/run/x.sock"}},

		// The retired spelling still parses, and consumes the verb.
		{[]string{"apply", "-src", "src.tgz"}, "apply", []string{"-src", "src.tgz"}},
		{[]string{"apply", "src.tgz"}, "apply", []string{"src.tgz"}},

		// A leading flag is never a verb, so it reaches the flag parser
		// rather than being reported as an unknown command.
		{[]string{"-dry-run", "src.tgz"}, "apply", []string{"-dry-run", "src.tgz"}},

		// Help, both spellings. expandVerb refuses to treat "-h" as a verb
		// (nothing with a leading dash is one), so the switch catches it
		// explicitly rather than letting it fall through to apply.
		{[]string{"help"}, "help", []string{}},
		{[]string{"-h"}, "help", []string{}},
		{[]string{"--help"}, "help", []string{}},
	}
	for _, tc := range cases {
		verb, rest := upgradeRoute(tc.args)
		if verb != tc.wantVerb {
			t.Errorf("upgradeRoute(%q) verb = %q, want %q", tc.args, verb, tc.wantVerb)
		}
		if !reflect.DeepEqual(rest, tc.wantRest) {
			t.Errorf("upgradeRoute(%q) rest = %q, want %q", tc.args, rest, tc.wantRest)
		}
	}
}

// TestUpgradeRouteNoArgs: no arguments means print usage, not apply nothing.
func TestUpgradeRouteNoArgs(t *testing.T) {
	if verb, rest := upgradeRoute(nil); verb != "" || rest != nil {
		t.Fatalf("upgradeRoute(nil) = (%q, %q), want (\"\", nil)", verb, rest)
	}
}

// TestUpgradeRouteDoesNotConsumeTheArchive is the mistake this routing could
// most easily have made: returning args[1:] on the default arm, which would
// drop the archive path on the floor and leave apply with nothing to build.
func TestUpgradeRouteDoesNotConsumeTheArchive(t *testing.T) {
	verb, rest := upgradeRoute([]string{"gravinet-v783.tgz"})
	if verb != "apply" {
		t.Fatalf("verb = %q, want apply", verb)
	}
	if len(rest) != 1 || rest[0] != "gravinet-v783.tgz" {
		t.Fatalf("rest = %q, want the archive path intact", rest)
	}
}

// TestKeySlotArg covers the subtlest change in the batch. "gravinet key set"
// takes a slot and a key, and both spellings have to land on the same two
// values: the new "key set 2 KEY" (slot positional, key left over) and the
// retired "key set KEY -slot 2" (slot in the flag, key left over untouched).
func TestKeySlotArg(t *testing.T) {
	cases := []struct {
		name     string
		slotFlag string
		args     []string
		wantSlot string
		wantRest []string
	}{
		{
			name:     "new spelling: slot is the first argument",
			args:     []string{"2", "BASE64KEY"},
			wantSlot: "2",
			wantRest: []string{"BASE64KEY"},
		},
		{
			// The old form's positional is the *key*, and -slot being present
			// is what says so. Consuming it as a slot here would send the key
			// to Atoi and reject a perfectly valid command.
			name:     "old spelling: -slot wins and the key is untouched",
			slotFlag: "2",
			args:     []string{"BASE64KEY"},
			wantSlot: "2",
			wantRest: []string{"BASE64KEY"},
		},
		{
			name:     "slot alone, as show/enable/delete take it",
			args:     []string{"0"},
			wantSlot: "0",
			wantRest: []string{},
		},
		{
			name:     "note text survives after the slot",
			args:     []string{"1", "rotated", "for", "Q3"},
			wantSlot: "1",
			wantRest: []string{"rotated", "for", "Q3"},
		},
		{
			// list and generate take no slot; nothing may be consumed from
			// under them, and no error is raised for the slot they never
			// wanted.
			name:     "no arguments at all",
			args:     nil,
			wantSlot: "",
			wantRest: nil,
		},
		{
			// A flag is never a slot, so it stays in the residue for the
			// caller's own parsing rather than being eaten as one.
			name:     "a leading flag is not a slot",
			args:     []string{"-no-restart"},
			wantSlot: "",
			wantRest: []string{"-no-restart"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slot, rest := keySlotArg(tc.slotFlag, tc.args)
			if slot != tc.wantSlot {
				t.Errorf("slot = %q, want %q", slot, tc.wantSlot)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// TestUpgradeArchiveArg covers the positional/-src precedence and the two ways
// of getting it wrong.
func TestUpgradeArchiveArg(t *testing.T) {
	if got, err := upgradeArchiveArg([]string{"a.tgz"}, ""); err != nil || got != "a.tgz" {
		t.Errorf("positional only: got (%q, %v), want (a.tgz, nil)", got, err)
	}
	if got, err := upgradeArchiveArg(nil, "b.tgz"); err != nil || got != "b.tgz" {
		t.Errorf("-src only: got (%q, %v), want (b.tgz, nil)", got, err)
	}
	// A half-converted script can supply both; the new spelling wins rather
	// than erroring, since both values are almost certainly the same path.
	if got, err := upgradeArchiveArg([]string{"a.tgz"}, "b.tgz"); err != nil || got != "a.tgz" {
		t.Errorf("both: got (%q, %v), want (a.tgz, nil) — the argument should win", got, err)
	}
	if _, err := upgradeArchiveArg(nil, ""); err == nil {
		t.Error("neither: want a usage error, got nil")
	} else if !strings.Contains(err.Error(), "ARCHIVE") {
		t.Errorf("neither: error %q should show the usage", err)
	}
	// Two archives is a mistake worth naming, not a first-one-wins.
	if _, err := upgradeArchiveArg([]string{"a.tgz", "b.tgz"}, ""); err == nil {
		t.Error("two archives: want an error, got nil")
	}
}
