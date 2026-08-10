package webadmin

import (
	"strings"
	"testing"
)

// The add row shipped without the negation toggles in v824: they were wired
// into startNATEdit only, so a new rule could not be negated until it had been
// saved once and reopened. The two editors are separate blocks of markup that
// have to stay in step, and nothing but this checks that they do.
//
// Asserted against the UI source because that is where the divergence lives —
// there is no server-side artifact that would differ between a page whose add
// row has the control and one whose add row does not.
func TestNATBothEditorsOfferNegation(t *testing.T) {
	src := indexHTML

	addRow := between(t, src, "function natAddRow(", "function startNATEdit(")
	editRow := between(t, src, "function startNATEdit(", "function qosClassLabel(")

	for _, tc := range []struct{ name, body string }{
		{"add row", addRow},
		{"edit row", editRow},
	} {
		for _, want := range []string{
			"nate-src-negate",  // the source toggle exists
			"nate-dst-negate",  // and the dest toggle
			"fwWireNegToggles", // and both are actually wired for clicks
			"source_negate",    // and the flags reach the API
			"dest_negate",
		} {
			if !strings.Contains(tc.body, want) {
				t.Errorf("%s is missing %q", tc.name, want)
			}
		}
		// The guard that stops a rule which can never match being saved.
		if !strings.Contains(tc.body, "would match nothing") {
			t.Errorf("%s does not refuse negation on an empty field", tc.name)
		}
	}
}

func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("anchor %q not found - the NAT editors moved and this guard needs updating, not deleting", start)
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		t.Fatalf("anchor %q not found after %q", end, start)
	}
	return s[i : i+j]
}
