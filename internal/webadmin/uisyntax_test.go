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

// uiScript is the page's JavaScript, concatenated out of its <script> blocks.
func uiScript(t *testing.T) string {
	t.Helper()
	var js strings.Builder
	for _, m := range scriptBlock.FindAllStringSubmatch(indexHTML, -1) {
		js.WriteString(m[1])
		js.WriteString("\n")
	}
	if js.Len() == 0 {
		t.Fatal("no <script> blocks found in indexHTML — the extraction broke, not the page")
	}
	return js.String()
}

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
// Skips when node is unavailable rather than failing, since not everything
// that builds gravinet has a JS runtime. That skip is why
// TestPageScriptHasNoUnterminatedStringLiteral below exists and does not skip:
// v976 shipped a blank page from a machine without node, where this test was
// the only thing that would have caught it and quietly did nothing.
func TestUIScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; only the built-in string-literal check runs here")
	}
	path := filepath.Join(t.TempDir(), "ui.js")
	if err := os.WriteFile(path, []byte(uiScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("the admin UI script does not parse — every page would render blank:\n%s", out)
	}
}

// A full parse needs node. This does not, and covers the one way this page has
// actually been broken twice: a string literal that never closes.
//
// v976's was the word "network's" written into a single-quoted help string.
// The apostrophe closed the literal, and every line of script after it became
// garbage. The neighbouring entries escape theirs — "it\'s", "key\'s",
// "node\'s" — which is exactly what makes a new unescaped one easy to add and
// impossible to see.
//
// Sound because the page has no multi-line strings: indexHTML is a Go raw
// string, so a backtick cannot appear and template literals cannot exist. Every
// literal therefore opens and closes on one line, and one that reaches end of
// line is broken. Pinned by TestPageScriptUsesNoTemplateLiterals.
func TestPageScriptHasNoUnterminatedStringLiteral(t *testing.T) {
	for _, p := range unterminatedStrings(uiScript(t)) {
		t.Errorf("unterminated %c string literal on script line %d — the page will render blank:\n    ...%s\n"+
			"    (in prose, write an apostrophe as \\' the way the entries around it do)",
			p.quote, p.line, p.context)
	}
}

func TestPageScriptUsesNoTemplateLiterals(t *testing.T) {
	if strings.Contains(indexHTML, "`") {
		t.Error("indexHTML contains a backtick, which a Go raw string cannot hold — if the " +
			"quoting changed, the line-based scan above is no longer sound")
	}
}

type badString struct {
	quote   rune
	line    int
	context string
}

// regexKeyword matches a keyword a regex literal may directly follow, so
// `return /["]/` reads as a pattern rather than as division into a string.
var regexKeyword = regexp.MustCompile(`\b(return|typeof|case|in|of|do|else|delete|void|instanceof)\s*$`)

// unterminatedStrings tokenizes src just far enough to tell a string literal
// from a comment, a regex or a division, and reports each literal still open at
// end of line.
func unterminatedStrings(src string) []badString {
	var out []badString
	line := 1
	var prev byte // last significant character, for the regex-vs-division call
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			i += 2
		case c == '/' && (strings.IndexByte("(,=:[!&|?{};+*%~^<>", prev) >= 0 || regexKeyword.MatchString(src[:i])):
			i = skipRegex(src, i)
			prev = '/'
		case c == '\'' || c == '"':
			start, quote := line, c
			i++
			closed := false
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '\n' {
					break
				}
				if src[i] == quote {
					closed = true
					i++
					break
				}
				i++
			}
			if !closed {
				out = append(out, badString{quote: rune(quote), line: start, context: tailOfLine(src[:i])})
			}
			prev = quote
		default:
			if c != ' ' && c != '\t' && c != '\r' {
				prev = c
			}
			i++
		}
	}
	return out
}

// tailOfLine returns the end of the current line, which is where the offending
// quote is.
func tailOfLine(s string) string {
	if n := strings.LastIndexByte(s, '\n'); n >= 0 {
		s = s[n+1:]
	}
	if len(s) > 90 {
		s = s[len(s)-90:]
	}
	return s
}

// skipRegex advances past a regex literal starting at i, stopping at end of
// line if it is unterminated. Character classes are tracked because a '/'
// inside one does not close the pattern.
func skipRegex(src string, i int) int {
	i++
	inClass := false
	for i < len(src) && src[i] != '\n' {
		if src[i] == '\\' {
			i += 2
			continue
		}
		switch src[i] {
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				return i + 1
			}
		}
		i++
	}
	return i
}
