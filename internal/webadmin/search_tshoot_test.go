package webadmin

import (
	"regexp"
	"strings"
	"testing"
)

// Typing "tshoot" or "troubleshoot" in the header search box lands on the
// tshoot button.
//
// The button is on the Logs page, which is not where anyone would think to
// look for a mesh-wide diagnostic bundle — v984 moved it to the far end of
// that toolbar for exactly that reason. Search is how someone who has been
// told to send a bundle, and has a word rather than a page, gets to it.
//
// The chain has four links and no visible symptom when any one of them
// breaks: the index entry, the btn/key pair that has to agree across two
// files' worth of distance, the branch in navigateToSearchResult that knows
// what to do with an 'action' match, and the attribute enhanceTable writes
// out. A broken link anywhere gives a search box that finds nothing, or finds
// the entry and lands on the page without ever pointing at the button — which
// looks enough like working that it would not be noticed.

// The index entry exists, is an 'action' match, and matches both words.
//
// searchIndexQuery is a plain substring test against label+extraHay
// lowercased, so this reconstructs that haystack from the source and asks it
// the same question the search box would. "troubleshoot" does not contain
// "tshoot" and "tshoot" does not contain "troubleshoot" — neither word is
// reachable from the other, which is the whole reason the second one has to
// be written into the haystack by hand.
func TestSearchIndexHasTshootAction(t *testing.T) {
	call := tshootIndexEntry(t)

	if !strings.Contains(call, "kind:'action'") {
		t.Error("the tshoot search entry is not an 'action' match, so navigateToSearchResult will treat it as a plain section hit and never point at the button")
	}

	lits := jsStringLiterals(call)
	if len(lits) < 2 {
		t.Fatalf("could not read the label and haystack out of the tshoot index entry: %q", call)
	}
	hay := strings.ToLower(lits[0] + " " + lits[len(lits)-1])
	for _, q := range []string{"tshoot", "troubleshoot"} {
		if !strings.Contains(hay, q) {
			t.Errorf("searching %q would not find the tshoot button: it is not a substring of the entry's haystack %q", q, hay)
		}
	}
}

// The name the index asks for is the name the button is rendered with.
//
// These are ~13,000 lines apart, they are both bare strings, and nothing
// brings them together until a search result tries to find a button that is
// not there. A typo in either is silent.
func TestTshootSearchTargetMatchesButtonKey(t *testing.T) {
	want := jsFieldValue(t, tshootIndexEntry(t), "btn")
	got := jsFieldValue(t, logsRowButtons(t), "key")
	if want != got {
		t.Errorf("the search index looks for the toolbar button keyed %q, but the logs toolbar renders it as %q — the search result would land on the Logs page and point at nothing", want, got)
	}
}

// enhanceTable turns spec.key into the attribute the search selector queries,
// and navigateToSearchResult queries the attribute enhanceTable writes. All
// three have to name it identically; two out of three is a no-op.
func TestToolbarButtonKeyIsRenderedAsDataAttribute(t *testing.T) {
	const attr = "data-tbar-btn"

	bar := jsFunc(t, "function enhanceTable(")
	if !strings.Contains(bar, "spec.key?' "+attr+"=\"'+esc(spec.key)+'\"'") {
		t.Errorf("enhanceTable no longer renders spec.key as %s — a keyed toolbar button becomes unfindable in the DOM", attr)
	}

	nav := jsFunc(t, "async function navigateToSearchResult(")
	if !strings.Contains(nav, "r.match.kind === 'action'") {
		t.Fatal("navigateToSearchResult has no branch for 'action' matches — the tshoot hit would fall through to the per-network card lookup below, which has no netId to find")
	}
	if !strings.Contains(nav, "'#content ["+attr+"=\"'") {
		t.Errorf("navigateToSearchResult's 'action' branch does not look the button up by %s", attr)
	}
}

// A button gets the ring flash, not the background one.
//
// search-hit fades a background from --acc down to transparent. On a table
// row, which is transparent to begin with, that reads as a highlight. On an
// ordinary button, which is already --acc, it reads as the button fading out
// — landing on tshoot would animate it away rather than point at it.
func TestButtonSearchHitUsesRingFlash(t *testing.T) {
	fn := jsFunc(t, "function flashAndScroll(")
	if !strings.Contains(fn, "target.tagName === 'BUTTON' ? 'search-hit-ring' : 'search-hit'") {
		t.Error("flashAndScroll no longer picks the ring flash for buttons; an accent-coloured button flashed with search-hit fades out instead of lighting up")
	}
	for _, want := range []string{".search-hit-ring {", "@keyframes search-hit-ring-flash"} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("%s is missing from the stylesheet, so the class flashAndScroll adds to a button animates nothing", want)
		}
	}
}

// tshootIndexEntry returns the buildSearchIndex add() call for the tshoot
// button, comment lines stripped — the comment above it discusses
// 'troubleshoot' and 'action' by name, and a test scanning for those would be
// reading the explanation rather than the thing explained.
func tshootIndexEntry(t *testing.T) string {
	t.Helper()
	idx := jsFunc(t, "function buildSearchIndex(")
	i := strings.Index(idx, "add('tshoot'")
	if i < 0 {
		t.Fatal("buildSearchIndex has no entry for the tshoot button — searching 'tshoot' or 'troubleshoot' finds nothing at all")
	}
	call := idx[i:]
	j := strings.Index(call, ");")
	if j < 0 {
		t.Fatal("could not find the end of the tshoot add() call")
	}
	return stripJSComments(call[:j])
}

// jsFunc returns the source of the function whose declaration starts with
// decl, up to the next top-level declaration.
func jsFunc(t *testing.T, decl string) string {
	t.Helper()
	i := strings.Index(indexHTML, decl)
	if i < 0 {
		t.Fatalf("%s not found in the served page", decl)
	}
	body := indexHTML[i+len(decl):]
	if j := strings.Index(body, "\n}\n"); j >= 0 {
		body = body[:j]
	}
	return body
}

// jsFieldValue pulls the single-quoted value of a `name:'value'` field out of
// a fragment of JS source.
func jsFieldValue(t *testing.T, src, name string) string {
	t.Helper()
	m := regexp.MustCompile(name + `:'([^']*)'`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no %s:'...' field in:\n%s", name, src)
	}
	return m[1]
}

// jsStringLiterals returns the single-quoted string literals in src, in order.
func jsStringLiterals(src string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`'((?:[^'\\]|\\.)*)'`).FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

// stripJSComments drops whole-line // comments.
func stripJSComments(src string) string {
	var kept []string
	for _, ln := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}
