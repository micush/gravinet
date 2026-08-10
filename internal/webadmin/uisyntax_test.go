package webadmin

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var scriptBlock = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)

// The admin UI is one large script embedded in a Go string, and a syntax error
// anywhere in it does not degrade — it stops the whole file parsing, so every
// page renders as a blank screen. No Go test notices, because the string still
// compiles and every guard that greps it for a substring still passes.
//
// That is not hypothetical: a botched edit left a bare ">'" mid-expression and
// shipped, and the symptom was an empty grey page with no clue in it. This
// parses the extracted script with node so a broken edit fails here instead of
// in front of an operator.
//
// Skips when node is unavailable rather than failing, since it is a
// development-time check and not everything that builds gravinet has a JS
// runtime. The trade is that a machine without node loses the check entirely,
// which is worth knowing when relying on a green run.
func TestUIScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; UI syntax cannot be checked here")
	}

	var js strings.Builder
	for _, m := range scriptBlock.FindAllStringSubmatch(indexHTML, -1) {
		js.WriteString(m[1])
		js.WriteString("\n")
	}
	if js.Len() == 0 {
		t.Fatal("no <script> blocks found in indexHTML — the extraction broke, not the page")
	}

	path := filepath.Join(t.TempDir(), "ui.js")
	if err := os.WriteFile(path, []byte(js.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("the admin UI script does not parse — every page would render blank:\n%s", out)
	}
}
