package main

import (
	"reflect"
	"testing"
)

func TestMatchPrefixGroups(t *testing.T) {
	groups := [][]string{
		{"network", "net"},
		{"naming"},
		{"nat"},
		{"managed"},
		{"manager"},
		{"host", "hosts"},
		{"generate", "gen"},
		{"delete", "del", "remove"},
		{"monitor"},
		{"mesh"},
	}
	cases := []struct {
		in    string
		want  string
		cands []string
	}{
		{"monitor", "monitor", nil}, // exact
		{"mon", "monitor", nil},     // unique prefix
		{"me", "mesh", nil},
		{"net", "net", nil},      // exact alias wins as itself
		{"netw", "network", nil}, // prefix past the alias
		{"ho", "host", nil},      // host+hosts are one command, not ambiguous
		{"g", "generate", nil},   // generate+gen are one command
		{"del", "del", nil},      // exact alias, no expansion needed
		{"rem", "delete", nil},   // alias prefix resolves to the arm's canonical
		{"man", "", []string{"managed", "manager"}},
		{"n", "", []string{"network", "naming", "nat"}},
		{"m", "", []string{"managed", "manager", "monitor", "mesh"}},
		{"zzz", "", nil},   // unknown
		{"", "", nil},      // empty
		{"-h", "", nil},    // flags never match
		{"--net", "", nil}, // even flag-shaped near-names
	}
	for _, c := range cases {
		m, cands := matchPrefixGroups(c.in, groups)
		if m != c.want {
			t.Errorf("%q: match = %q, want %q", c.in, m, c.want)
		}
		if !reflect.DeepEqual(cands, c.cands) {
			t.Errorf("%q: candidates = %v, want %v", c.in, cands, c.cands)
		}
	}
}

func TestExpandVerb(t *testing.T) {
	ex := func(arg string) string {
		return expandVerb(arg, v("list"), v("delete", "del", "remove"), v("disable"), v("enable"))
	}
	for in, want := range map[string]string{
		"l":       "list",
		"li":      "list",
		"del":     "del",    // exact alias untouched — switch matches it anyway
		"dele":    "delete", // unambiguous continuation
		"rem":     "delete", // alias prefix canonicalizes
		"d":       "d",      // delete-vs-disable ambiguous: unchanged, switch default handles it
		"en":      "enable",
		"unknown": "unknown", // unchanged
		"-h":      "-h",      // flags untouched
	} {
		if got := ex(in); got != want {
			t.Errorf("expandVerb(%q) = %q, want %q", in, got, want)
		}
	}
}
