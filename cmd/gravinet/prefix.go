package main

import "strings"

// This file makes every CLI command name accept any unambiguous prefix:
// "gravinet mon met" is "gravinet monitor metrics", "gravinet me ne" is
// "gravinet mesh networks". The rules, applied identically at every level
// (top-level command, group leaf, nested verb):
//
//   - An exact match always wins, even when it is also a prefix of another
//     name ("net" stays the network alias; "del" stays "del" even though it
//     prefixes "delete").
//   - Otherwise, a prefix matching names in exactly one *command* resolves to
//     that command. Aliases of the same command count as one candidate, not
//     several: "g" under key means "generate" even though it prefixes both
//     "generate" and its alias "gen", and "ho" at the top level is host even
//     though it prefixes both "host" and "hosts".
//   - A prefix spanning several distinct commands is ambiguous: the dispatch
//     levels say so and list the candidates; the nested-verb helper returns
//     the input unchanged, which lands in that switch's existing default —
//     its usual usage/help output — rather than guessing.
//   - Anything starting with "-" is a flag, never a command, and is returned
//     untouched.
//
// Exact names therefore behave exactly as before, which is what keeps every
// script that calls commands by full name (install-*.sh, the upgrade
// preflight's "gravinet selftest"/"gravinet version") completely unaffected.

// matchPrefixGroups resolves arg against commands, each expressed as a group
// of equivalent names (the literals of one switch case arm: canonical name
// first, aliases after). It returns the matched name — arg itself on an exact
// hit, or the sole matching group's first (canonical) name — plus, when the
// prefix spans several groups, one representative candidate per group for the
// "ambiguous" message. No match at all returns ("", nil).
func matchPrefixGroups(arg string, groups [][]string) (string, []string) {
	if arg == "" || strings.HasPrefix(arg, "-") {
		return "", nil
	}
	var hitGroups [][]string
	for _, g := range groups {
		for _, n := range g {
			if n == arg {
				return n, nil // exact always wins, alias or canonical alike
			}
		}
	}
	for _, g := range groups {
		for _, n := range g {
			if strings.HasPrefix(n, arg) {
				hitGroups = append(hitGroups, g)
				break // one hit per group: aliases are the same command
			}
		}
	}
	switch len(hitGroups) {
	case 0:
		return "", nil
	case 1:
		return hitGroups[0][0], nil
	}
	cands := make([]string, len(hitGroups))
	for i, g := range hitGroups {
		cands[i] = g[0]
	}
	return "", cands
}

// expandVerb resolves arg against a nested switch's case arms — one variadic
// group per arm, aliases together — returning the unique expansion if there
// is one and arg unchanged otherwise (exact, ambiguous, unknown, and
// flag-like inputs all fall through to the switch as-is). Each call site
// mirrors the literals of the switch right below it: if you add a case, add
// its verbs here too, or the new verb simply won't abbreviate.
func expandVerb(arg string, groups ...[]string) string {
	if m, _ := matchPrefixGroups(arg, groups); m != "" {
		return m
	}
	return arg
}

// v is shorthand for one case arm's name group at expandVerb call sites.
func v(names ...string) []string { return names }
