package config

// Human-readable audit diffs for the config history feature (history.go).
//
// Ported from parapet's src/diff.rs, adapted to gravinet's shape. Parapet's
// config is flat — a dozen independent top-level sections (policies, zones,
// NAT, ...), most of them id-keyed lists that its own PUT handlers replace
// wholesale, so diffing old-vs-new is exactly how it discovers what changed.
// gravinet's config is two-level instead: a pile of global settings plus one
// Networks list, with the actual policy-like content (keys, seeds, routes,
// NAT, QoS, firewall, hosts, DNS) nested *inside* each network. So instead of
// parapet's dozen top-level sections, gravinet gets two: "networks" (each
// network diffed as a composite of its own sub-areas, the same way parapet's
// own "routing" section composites static routes + policy routes + BGP +
// OSPF) and "settings" (everything else, field-by-field).
//
// Also unlike parapet's policies/NAT (which carry a numeric id), gravinet's
// NAT/QoS/firewall rule lists have no stable identity field at all — nothing
// to match old[i] to new[j] by if the list is reordered or edited in the
// middle. Those three are diffed as whole objects (diffObject, field-level)
// rather than item lists; the sub-areas that do have a natural stable key —
// seeds (address), routes/rejects (cidr), hosts (name), DNS (domain) — get
// the full added/removed/renamed/changed treatment via diffItems, same as
// parapet's own id-keyed lists.
//
// The comparison works on each config's generic JSON form (map[string]any /
// []any, via json.Marshal+Unmarshal into `any`) rather than the typed Go
// structs — the same choice parapet made and for the same reason: a diff
// stays correct as fields evolve without a hand-maintained field list, since
// it's driven by whatever's actually in the JSON rather than a struct
// literal that could drift from it.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// maxNamed caps how many named items are listed before collapsing to "and N
// more", so a bulk change doesn't produce a runaway summary line.
const maxNamed = 6

// toGeneric round-trips v through JSON into a generic map[string]any /
// []any / scalar tree, the same shape a browser's JSON.parse would produce.
func toGeneric(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}

func getField(v any, key string) any {
	m := asMap(v)
	if m == nil {
		return nil
	}
	return m[key]
}

// canonical renders v as a stable string for equality comparison. Go's
// encoding/json does not sort map keys by default for map[string]any during
// re-marshal in older versions, but does since Go 1.12 (map keys are sorted
// for deterministic output) — relied on here for two encodings of the same
// logical value to compare equal.
func canonical(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// item is one keyed entry extracted from a list for diffing — parapet's
// Item struct, same fields, same purpose.
type item struct {
	key     string // stable identity (the natural key field's value)
	label   string // human display (falls back to "#<key>" if unlabeled)
	content string // canonical serialization, for equality
}

// itemsFrom extracts items from a JSON array, keyed and labeled by the given
// fields (they may be the same field, e.g. seeds keyed and labeled by their
// own address).
func itemsFrom(arr []any, keyField, labelField string) []item {
	out := make([]item, 0, len(arr))
	for _, v := range arr {
		key := fmt.Sprint(getField(v, keyField))
		label := key
		if lv := getField(v, labelField); lv != nil {
			if s, ok := lv.(string); ok && s != "" {
				label = s
			}
		}
		if key == "<nil>" {
			key = ""
		}
		if label == "" {
			label = "#" + key
		}
		out = append(out, item{key: key, label: label, content: canonical(v)})
	}
	return out
}

// diffItems renders a delta phrase for two keyed item lists: parapet's
// diff_items, unchanged.
func diffItems(old, new []item) string {
	var added, removed, renamed, changed []string
	for _, n := range new {
		found := false
		for _, o := range old {
			if o.key == n.key && n.key != "" {
				found = true
				if o.label != n.label {
					renamed = append(renamed, fmt.Sprintf("'%s' \u2192 '%s'", o.label, n.label))
				} else if o.content != n.content {
					changed = append(changed, fmt.Sprintf("'%s'", n.label))
				}
				break
			}
		}
		if !found {
			added = append(added, fmt.Sprintf("'%s'", n.label))
		}
	}
	for _, o := range old {
		found := false
		for _, n := range new {
			if n.key == o.key && o.key != "" {
				found = true
				break
			}
		}
		if !found {
			removed = append(removed, fmt.Sprintf("'%s'", o.label))
		}
	}

	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+joinCapped(added))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+joinCapped(removed))
	}
	if len(renamed) > 0 {
		parts = append(parts, "renamed "+joinCapped(renamed))
	}
	if len(changed) > 0 {
		parts = append(parts, "changed "+joinCapped(changed))
	}
	if len(parts) == 0 {
		if len(old) == 0 && len(new) == 0 {
			return "no effective change"
		}
		return "reordered (no content change)"
	}
	return strings.Join(parts, "; ")
}

func joinCapped(items []string) string {
	if len(items) <= maxNamed {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:maxNamed], ", ") + fmt.Sprintf(", and %d more", len(items)-maxNamed)
}

// diffObject is a field-by-field diff of two objects, reporting which top-
// level keys changed value (added, removed, or a different value) — parapet's
// diff_object, unchanged. excluding lets a caller diff "everything except
// these composite sub-areas", which is how the per-network scalar-field
// summary works (composite areas like seeds/routes/nat are reported
// separately, so they're excluded here to avoid double-reporting).
func diffObject(old, new any, excluding ...string) string {
	skip := make(map[string]bool, len(excluding))
	for _, k := range excluding {
		skip[k] = true
	}
	om, nm := asMap(old), asMap(new)
	var changed []string
	seen := make(map[string]bool)
	for k, nv := range nm {
		if skip[k] {
			continue
		}
		seen[k] = true
		if ov, ok := om[k]; !ok || canonical(ov) != canonical(nv) {
			changed = append(changed, k)
		}
	}
	for k := range om {
		if skip[k] || seen[k] {
			continue
		}
		changed = append(changed, k)
	}
	if len(changed) == 0 {
		return "no effective change"
	}
	sort.Strings(changed)
	return "changed " + joinCapped(changed)
}

// networkKeySlotsDiff reports per-slot key changes. Keys is a fixed 8-slot
// array (config.Network.Keys), not a variable list — there's no add/remove
// in the diffItems sense, only "this slot's content changed", so it gets its
// own small comparison rather than being forced through diffItems.
func networkKeySlotsDiff(old, new any) string {
	oldArr, newArr := asArray(old), asArray(new)
	var changed []string
	for i := 0; i < 8; i++ {
		var ov, nv any
		if i < len(oldArr) {
			ov = oldArr[i]
		}
		if i < len(newArr) {
			nv = newArr[i]
		}
		if canonical(ov) == canonical(nv) {
			continue
		}
		label := fmt.Sprintf("slot %d", i)
		if lv, ok := getField(nv, "label").(string); ok && lv != "" {
			label += " (" + lv + ")"
		} else if lv, ok := getField(ov, "label").(string); ok && lv != "" {
			label += " (" + lv + ")"
		}
		changed = append(changed, label)
	}
	if len(changed) == 0 {
		return "no effective change"
	}
	return "changed " + joinCapped(changed)
}

// networkArea is one sub-area of a network's composite diff: a label plus
// how to compute its detail phrase from the two networks' generic JSON.
type networkArea struct {
	field string // the network's JSON field this area covers
	label string
	diff  func(old, new any) string
}

// networkAreas lists every sub-area a network diff checks, in display
// order — parapet's own SECTIONS list, but for one network's internals
// rather than the whole config.
var networkAreas = []networkArea{
	{"keys", "keys", networkKeySlotsDiff},
	{"seeds", "seeds", func(old, new any) string {
		return diffItems(itemsFrom(asArray(old), "address", "address"), itemsFrom(asArray(new), "address", "address"))
	}},
	{"routes", "advertised routes", func(old, new any) string {
		return diffItems(itemsFrom(asArray(old), "cidr", "cidr"), itemsFrom(asArray(new), "cidr", "cidr"))
	}},
	{"route_reject", "rejected routes", func(old, new any) string {
		return diffItems(itemsFrom(asArray(old), "cidr", "cidr"), itemsFrom(asArray(new), "cidr", "cidr"))
	}},
	{"hosts_advertise", "custom hosts", func(old, new any) string {
		return diffItems(itemsFrom(asArray(old), "name", "name"), itemsFrom(asArray(new), "name", "name"))
	}},
	{"hosts_reject", "rejected hosts", func(old, new any) string {
		return diffItems(itemsFrom(asArray(old), "name", "name"), itemsFrom(asArray(new), "name", "name"))
	}},
	{"dns_advertise", "DNS forwards", func(old, new any) string {
		return diffItems(itemsFrom(asArray(old), "domain", "domain"), itemsFrom(asArray(new), "domain", "domain"))
	}},
	{"dns_reject", "rejected DNS forwards", func(old, new any) string {
		return diffItems(itemsFrom(asArray(old), "domain", "domain"), itemsFrom(asArray(new), "domain", "domain"))
	}},
	{"nat", "NAT", func(old, new any) string { return diffObject(old, new) }},
	{"qos", "QoS", func(old, new any) string { return diffObject(old, new) }},
	{"firewall", "firewall", func(old, new any) string { return diffObject(old, new) }},
}

// networkAreaFields is every field name covered by networkAreas above, used
// to exclude them from the catch-all scalar-field diff below.
func networkAreaFields() []string {
	out := make([]string, len(networkAreas))
	for i, a := range networkAreas {
		out[i] = a.field
	}
	return out
}

// diffNetwork renders a composite delta phrase for one network, covering
// every sub-area plus a catch-all for scalar fields (name, notes, subnet4/6,
// address4/6, mtu, tun_name, ...) that don't have their own area above —
// parapet's diff_routing, generalized into a data-driven loop instead of one
// hand-written function per area.
func diffNetwork(old, new any) string {
	var areas []string
	for _, a := range networkAreas {
		ov, nv := getField(old, a.field), getField(new, a.field)
		if canonical(ov) == canonical(nv) {
			continue
		}
		areas = append(areas, a.label+": "+a.diff(ov, nv))
	}
	if scalar := diffObject(old, new, networkAreaFields()...); scalar != "no effective change" {
		areas = append(areas, scalar)
	}
	if len(areas) == 0 {
		return "no effective change"
	}
	return strings.Join(areas, "; ")
}

// detail computes the audit phrase for one of gravinet's two top-level
// sections. "networks" gets the full added/removed/renamed/changed
// treatment (matched by network id, labeled by name) with each changed
// network's own detail coming from diffNetwork; "settings" is everything
// else at the top level, field-by-field.
func detail(section string, old, new *Config) string {
	oldJSON, newJSON := toGeneric(old), toGeneric(new)
	switch section {
	case "networks":
		oldNets := itemsFromNetworks(asArray(getField(oldJSON, "networks")))
		newNets := itemsFromNetworks(asArray(getField(newJSON, "networks")))
		return diffItemsWithSubdetail(oldNets, newNets, asArray(getField(oldJSON, "networks")), asArray(getField(newJSON, "networks")))
	case "settings":
		return diffObject(oldJSON, newJSON, "networks")
	default:
		return ""
	}
}

func itemsFromNetworks(arr []any) []item {
	return itemsFrom(arr, "id", "name")
}

// diffItemsWithSubdetail is diffItems, but a same-key/different-content match
// gets its detail from diffNetwork (the composite sub-area breakdown) rather
// than just "changed 'name'" — the one place gravinet's version needs to go
// one level deeper than parapet's own diff_items, since a network is a much
// larger, more heterogeneous object than a parapet policy or NAT rule.
func diffItemsWithSubdetail(old, new []item, oldArr, newArr []any) string {
	var added, removed, renamed, changed []string
	findRaw := func(arr []any, id string) any {
		for _, v := range arr {
			if fmt.Sprint(getField(v, "id")) == id {
				return v
			}
		}
		return nil
	}
	for _, n := range new {
		found := false
		for _, o := range old {
			if o.key == n.key && n.key != "" {
				found = true
				if o.label != n.label {
					renamed = append(renamed, fmt.Sprintf("'%s' \u2192 '%s'", o.label, n.label))
				} else if o.content != n.content {
					sub := diffNetwork(findRaw(oldArr, o.key), findRaw(newArr, n.key))
					changed = append(changed, fmt.Sprintf("'%s' (%s)", n.label, sub))
				}
				break
			}
		}
		if !found {
			added = append(added, fmt.Sprintf("'%s'", n.label))
		}
	}
	for _, o := range old {
		found := false
		for _, n := range new {
			if n.key == o.key && o.key != "" {
				found = true
				break
			}
		}
		if !found {
			removed = append(removed, fmt.Sprintf("'%s'", o.label))
		}
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+joinCapped(added))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+joinCapped(removed))
	}
	if len(renamed) > 0 {
		parts = append(parts, "renamed "+joinCapped(renamed))
	}
	if len(changed) > 0 {
		parts = append(parts, "changed "+strings.Join(changed, "; "))
	}
	if len(parts) == 0 {
		return "reordered (no content change)"
	}
	return strings.Join(parts, "; ")
}

// sections are the two top-level areas config history tracks, in display
// order — gravinet's version of parapet's own SECTIONS const.
var historySections = []struct{ key, label string }{
	{"networks", "networks"},
	{"settings", "settings"},
}

func sectionChanged(section string, old, new *Config) bool {
	oldJSON, newJSON := toGeneric(old), toGeneric(new)
	if section == "settings" {
		// "settings" is everything except networks; compare the whole
		// object minus that one field.
		return diffObject(oldJSON, newJSON, "networks") != "no effective change"
	}
	return canonical(getField(oldJSON, section)) != canonical(getField(newJSON, section))
}

// ChangedSections is the list of section labels that differ between two
// configs, in display order. Empty means the two configs are equivalent —
// used by history.go's OnCommit to decide whether a change is worth
// snapshotting at all.
func ChangedSections(old, new *Config) []string {
	var out []string
	for _, s := range historySections {
		if sectionChanged(s.key, old, new) {
			out = append(out, s.label)
		}
	}
	return out
}

// SectionDiff is one section's audit detail, for a whole-config comparison.
type SectionDiff struct {
	Section string
	Detail  string
}

// FullSummary is a whole-config semantic diff: for every section that
// changed, its label and a human-readable detail phrase. Sections that are
// equivalent are omitted.
func FullSummary(old, new *Config) []SectionDiff {
	var out []SectionDiff
	for _, s := range historySections {
		if sectionChanged(s.key, old, new) {
			out = append(out, SectionDiff{Section: s.label, Detail: detail(s.key, old, new)})
		}
	}
	return out
}
