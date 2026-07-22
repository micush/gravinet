package upgrade

import "testing"

// Guards the regex against the real tree, not just a fixture: if main.go's
// version line is ever reformatted, this fails here rather than silently
// showing an empty version to an operator mid-fleet-push.
func TestSourceVersionAgainstThisActualTree(t *testing.T) {
	if got := SourceVersion("../.."); got == "" {
		t.Fatal("SourceVersion cannot read this repository's own main.go — the regex has drifted from the source")
	} else {
		t.Logf("this tree reports version %s", got)
	}
}
