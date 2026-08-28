package webadmin

import (
	"strings"
	"testing"
)

// Every page in the Traffic group carries its enabled/disabled switch as a
// pill beside the page's own <h2>, and none of them restates the section name
// on a card heading one line below it (v968). These lock in both halves,
// because the two are easy to half-do: a pill added without dropping the card
// title leaves the switch duplicated, and a title dropped without adding a
// pill leaves the feature with no way to be switched at all.

// trafficSections maps each section's render function to the endpoint its
// title pill posts to.
var trafficSections = map[string]string{
	"function secFirewall(c) {":  "/api/firewall",
	"function secNAT(c) {":       "/api/nat",
	"function secQoS(c) {":       "/api/qos",
	"function secBandwidth(c) {": "/api/bandwidth",
	"function secRadvd(c){":      "/api/radvd",
}

func TestTrafficSectionsPutTheirSwitchOnTheTitle(t *testing.T) {
	for fn, endpoint := range trafficSections {
		sec := between(t, indexHTML, fn, "\n}")
		if !strings.Contains(sec, "sectionTitlePill(c,") {
			t.Errorf("%s has no title pill", fn)
			continue
		}
		if !strings.Contains(sec, endpoint) {
			t.Errorf("%s's title pill does not post to %s", fn, endpoint)
		}
	}
}

// The card heading that used to restate the section name is gone from all
// five. sectionCardHead itself stays — DHCP has two cards on one page and
// genuinely needs a label on each.
func TestTrafficSectionsDoNotRestateTheirName(t *testing.T) {
	for fn := range trafficSections {
		sec := between(t, indexHTML, fn, "\n}")
		if strings.Contains(sec, "sectionCardHead(") {
			t.Errorf("%s still labels a card with the section name", fn)
		}
		for _, word := range []string{"FIREWALL", "'NAT'", "QOS", "BANDWIDTH"} {
			if strings.Contains(sec, ">"+word+"<") {
				t.Errorf("%s still renders %s as a card title", fn, word)
			}
		}
	}
}

// DHCP was the deliberate exception through v987: a server card and a relay
// card on one page, where a single title pill could not have said which half
// it governed. v988 removed the server, and with it the reason — so the page
// now follows the same rule as the five above, and the guard is that it does
// rather than that it does not.
//
// sectionCardHead itself stays. Nothing on this page uses it now, but the
// per-network cards elsewhere still do, and it is their helper too.
func TestDHCPNoLongerNeedsPerCardHeads(t *testing.T) {
	sec := between(t, indexHTML, "function secDHCP(c)", "\nfunction ")
	if strings.Contains(sec, "sectionCardHead(") {
		t.Error("DHCP labels a card with a heading again; with one card the switch belongs on the title")
	}
	if !strings.Contains(sec, "sectionTitlePill(c,") {
		t.Error("DHCP has no title pill, so the relay cannot be switched on at all")
	}
}

// The helper removes any pill already on the <h2> before adding one. Sections
// that redraw on save (Radvd rewrites its body on every load) would otherwise
// stack a new pill on the title each time.
func TestTitlePillDoesNotStackOnRedraw(t *testing.T) {
	src := uiFuncSrc(t, "sectionTitlePill")
	if !strings.Contains(src, "querySelector('.tag-toggle')") || !strings.Contains(src, "remove()") {
		t.Error("sectionTitlePill does not clear a previous pill, so a redrawing section would accumulate them")
	}
}

// The pill flips immediately and posts in the background, matching
// netCardHead and sectionCardHead. A pill that waited on the round trip would
// feel stuck on a slow node — and would behave differently from the per-row
// pills on the same page.
func TestTitlePillFlipsBeforeItPosts(t *testing.T) {
	src := uiFuncSrc(t, "sectionTitlePill")
	paintAt := strings.Index(src, "paint(on);")
	postAt := strings.Index(src, "api(apiPath")
	if paintAt < 0 || postAt < 0 {
		t.Fatal("sectionTitlePill no longer both paints and posts")
	}
	if paintAt > postAt {
		t.Error("sectionTitlePill posts before it repaints; the toggle will feel stuck on a slow node")
	}
}

// Shaping's switch reads as on when the node never sent the field, matching
// the inverted flag on the config side. Reading an absent field as "off" would
// show every upgraded node as unshaped.
func TestShapingPillDefaultsOnWhenTheNodeIsSilent(t *testing.T) {
	if !strings.Contains(indexHTML, "state.shapingEnabled = !(c.body && c.body.shaping_enabled === false)") {
		t.Error("shapingEnabled is not derived so that an absent field reads as enabled")
	}
	sec := between(t, indexHTML, "function secBandwidth(c) {", "\n}")
	if !strings.Contains(sec, "state.shapingEnabled !== false") {
		t.Error("the shaping pill does not treat an unset state as enabled")
	}
}

// IPv6 RA's pill drives the "feature" op, which is the flag the API already
// had — this page was simply missing a way to reach it.
func TestRadvdPillUsesTheExistingFeatureOp(t *testing.T) {
	sec := between(t, indexHTML, "function secRadvd(c){", "\n}")
	if !strings.Contains(sec, "op:'feature'") {
		t.Error("the IPv6 RA pill does not post the feature op")
	}
	if !strings.Contains(sec, "!!ra.enabled") {
		t.Error("the IPv6 RA pill is not backed by RAConfig.Enabled")
	}
}
