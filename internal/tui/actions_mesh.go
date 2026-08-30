package tui

// Mesh group actions: add/edit/delete/toggle for Networks, Keys, Seeds,
// Peers, and Bans. See actions.go for the dispatch mechanism, and
// mutate.go's package comment for the two ways a mutation actually reaches
// disk here — shelling out to the real CLI leaf wherever one exists, calling
// a validated config.Config setter directly plus this package's own
// save-and-reload for the handful of fields that don't have one yet.
//
// Every runLeaf call below was built from the exact usage string the
// corresponding cmd/gravinet leaf prints on a bad invocation, read from the
// source rather than guessed, and -net/-config/-sock are always passed
// explicitly (never left to a default) so a shelled-out command can never
// land on a different node or a different network than the one the operator
// is looking at.

import (
	"strconv"

	"gravinet/internal/config"
)

func init() {
	registerActions("networks", networksActions)
	registerActions("keys", keysActions)
	registerActions("seeds", seedsActions)
	registerActions("peers", peersActions)
	registerActions("bans", bansActions)
}

// ---- networks -------------------------------------------------------------

var networksActions = sectionActionSet{
	add: func(m *model) formSpec {
		return formSpec{
			title: "add network",
			fields: []formField{
				{key: "name", label: "name", kind: fieldText, help: "a short local label — letters, digits, hyphens"},
				{key: "subnet4", label: "subnet4", kind: fieldText, help: "IPv4 CIDR for this overlay, e.g. 10.42.0.0/16 (optional)"},
				{key: "subnet6", label: "subnet6", kind: fieldText, help: "IPv6 CIDR, e.g. fd00:42::/64 (optional)"},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["name"] == "" {
					return mutationResult{ok: false, detail: "a name is required"}
				}
				args := []string{"network", "add", v["name"]}
				if v["subnet4"] != "" {
					args = append(args, "subnet", v["subnet4"])
				}
				if v["subnet6"] != "" {
					args = append(args, "subnet6", v["subnet6"])
				}
				return runLeaf(m.cliArgs(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		n := findNetwork(m, row.id)
		out := map[rune]rowAction{
			'e': {edit: networkEditForm},
			'E': {edit: networkAdvancedForm},
			'd': {confirm: "Delete network " + row.id + "? This removes it from the config; any live sessions on it drop.",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("network", "delete", row.id)...)
				}},
		}
		label := "disable"
		if n != nil && !n.Enabled {
			label = "enable"
		}
		out[' '] = rowAction{label: label, run: func(m *model, row selRow) mutationResult {
			verb := "disable"
			if n != nil && !n.Enabled {
				verb = "enable"
			}
			return runLeaf(m.cliArgs("network", verb, row.id)...)
		}}
		return out
	},
}

// findNetwork locates a network by name in the current snapshot, for the row
// actions above that need to read the row's current values (to pre-fill a
// form, or to know which direction a toggle should go).
func findNetwork(m *model, name string) *config.Network {
	if m.snap == nil || m.snap.cfg == nil {
		return nil
	}
	for i := range m.snap.cfg.Networks {
		if m.snap.cfg.Networks[i].Name == name {
			return &m.snap.cfg.Networks[i]
		}
	}
	return nil
}

// networkEditForm covers the fields that already have a CLI verb and take
// effect without a restart (rename aside — see its own comment below):
// notes, MTU, and the two subnets. Submitting issues one CLI call per field
// that actually changed, comparing against the value the form opened with —
// an operator who touched one field doesn't get three no-op commands run
// alongside it.
func networkEditForm(m *model, row selRow) formSpec {
	n := findNetwork(m, row.id)
	notes, mtu, v4, v6 := "", "1380", "", ""
	if n != nil {
		notes, v4, v6 = n.Notes, n.Subnet4, n.Subnet6
		mtu = strconv.Itoa(n.MTU)
	}
	return formSpec{
		title: "edit network " + row.id,
		fields: []formField{
			{key: "notes", label: "notes", kind: fieldText, value: notes},
			{key: "mtu", label: "mtu", kind: fieldText, value: mtu, help: "changing this restarts the service (matches the CLI's own default)"},
			{key: "subnet4", label: "subnet4", kind: fieldText, value: v4, help: "\"none\" clears it"},
			{key: "subnet6", label: "subnet6", kind: fieldText, value: v6, help: "\"none\" clears it"},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			var results []mutationResult
			if v["notes"] != notes {
				results = append(results, runLeaf(m.cliArgs("network", "notes", row.id, v["notes"])...))
			}
			if v["mtu"] != mtu {
				n, err := strconv.Atoi(v["mtu"])
				if err != nil {
					results = append(results, mutationResult{ok: false, detail: "mtu must be a number, got " + v["mtu"]})
				} else {
					results = append(results, runLeaf(m.cliArgs("network", "mtu", row.id, strconv.Itoa(n))...))
				}
			}
			if v["subnet4"] != v4 || v["subnet6"] != v6 {
				results = append(results, runLeaf(m.cliArgs("network", "subnet", row.id, "subnet", v["subnet4"], "subnet6", v["subnet6"])...))
			}
			return combineResults(results)
		},
	}
}

// networkAdvancedForm covers the fields config.Config validates and stores
// but that cmd/gravinet has never grown a verb for — this node's own
// address, mesh topology, relay willingness, and self-seeding. Each is
// called directly against the setter a web-admin edit would use (see
// mutate.go's package comment), and every one of them needs a restart to
// take effect, which the setters' own doc comments say plainly and this
// form repeats rather than attempting to trigger automatically: this
// package has no audited, cross-platform way to restart the OS service, and
// guessing at one is a worse outcome than telling the operator to run
// "gravinet system power" or their service manager.
func networkAdvancedForm(m *model, row selRow) formSpec {
	n := findNetwork(m, row.id)
	v4, v6, mesh, relay, selfSeed := "", "", "full", "false", "false"
	if n != nil {
		v4, v6 = n.Address4, n.Address6
		if n.Mesh == "partial" {
			mesh = "partial"
		}
		relay = onOffBool(n.AllowRelay)
		selfSeed = onOffBool(n.SelfSeed)
	}
	return formSpec{
		title: "advanced: " + row.id + " (no CLI verb yet — see the note after submitting)",
		fields: []formField{
			{key: "address4", label: "address4", kind: fieldText, value: v4, help: "this node's own IPv4 inside subnet4, e.g. 10.42.0.5/16"},
			{key: "address6", label: "address6", kind: fieldText, value: v6},
			{key: "mesh", label: "mesh mode", kind: fieldSelect, value: mesh, options: []string{"full", "partial"}},
			{key: "relay", label: "allow relay", kind: fieldBool, value: relay},
			{key: "self_seed", label: "self seed", kind: fieldBool, value: selfSeed},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			if m.snap == nil || m.snap.cfg == nil {
				return mutationResult{ok: false, detail: "no config loaded"}
			}
			cfg := m.snap.cfg
			changed := false
			if v["address4"] != v4 || v["address6"] != v6 {
				if err := cfg.NetworkSetAddress(row.id, v["address4"], v["address6"]); err != nil {
					return mutationResult{ok: false, detail: err.Error()}
				}
				changed = true
			}
			if v["mesh"] != mesh {
				if err := cfg.NetworkSetMesh(row.id, v["mesh"]); err != nil {
					return mutationResult{ok: false, detail: err.Error()}
				}
				changed = true
			}
			if v["relay"] != relay {
				if err := cfg.NetworkSetAllowRelay(row.id, v["relay"] == "true"); err != nil {
					return mutationResult{ok: false, detail: err.Error()}
				}
				changed = true
			}
			if v["self_seed"] != selfSeed {
				if err := cfg.NetworkSetSelfSeed(row.id, v["self_seed"] == "true"); err != nil {
					return mutationResult{ok: false, detail: err.Error()}
				}
				changed = true
			}
			if !changed {
				return mutationResult{ok: true, detail: "nothing changed"}
			}
			res := commitConfig(cfg, m.cfgPath, m.sockPath)
			if res.ok {
				res.detail += " — restart required for these fields to take effect (gravinet system power, or your service manager)"
			}
			return res
		},
	}
}

// combineResults folds several sequential mutation attempts (an edit form
// that changed more than one field) into one to show: ok only if every step
// was, and every step's own detail kept on its own line rather than only the
// last one, so a form where the second of three calls failed doesn't look
// like nothing happened.
func combineResults(results []mutationResult) mutationResult {
	if len(results) == 0 {
		return mutationResult{ok: true, detail: "nothing changed"}
	}
	ok := true
	detail := ""
	for _, r := range results {
		if !r.ok {
			ok = false
		}
		if detail != "" {
			detail += "\n"
		}
		detail += r.detail
	}
	return mutationResult{ok: ok, detail: detail}
}

// ---- keys -------------------------------------------------------------
//
// Keys' selectable rows are per-network (one card, one table, per network —
// see pageKeys in sections.go), so a row's id alone doesn't say which
// network it belongs to. It's encoded into the id as "network\x1fslot" —
// keyRowID/parseKeyRowID below are the one place that packs and unpacks it,
// so every action here reads a plain (network, slot) pair rather than
// re-parsing the separator itself.

const idSep = "\x1f"

func keyRowID(netName string, slot int) string {
	return netName + idSep + strconv.Itoa(slot)
}

func parseKeyRowID(id string) (netName string, slot int, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] == idSep[0] {
			n, err := strconv.Atoi(id[i+1:])
			if err != nil {
				return "", 0, false
			}
			return id[:i], n, true
		}
	}
	return "", 0, false
}

var keysActions = sectionActionSet{
	// Adding a key means picking an empty slot, which the CLI's own
	// "generate"/"set" verbs do implicitly (they take the next free one) —
	// there is no "which network" to ask here beyond what the row cursor is
	// already on, so this section has no top-level 'a' add. Slots are
	// generated per network from the network the cursor is on when 'e' or
	// the row's own generate action is used instead; see row below.
	row: func(m *model, row selRow) map[rune]rowAction {
		netName, slot, ok := parseKeyRowID(row.id)
		if !ok {
			return nil
		}
		k := findKeySlot(m, netName, slot)
		out := map[rune]rowAction{
			'd': {confirm: "Delete key slot " + strconv.Itoa(slot) + " on " + netName + "? Peers still using it will fail to authenticate.",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("key", "delete", strconv.Itoa(slot), "-net", netName)...)
				}},
		}
		if k == nil || k.Key == "" {
			out['e'] = rowAction{edit: func(m *model, row selRow) formSpec { return keyGenerateForm(netName, slot) }}
			delete(out, 'd') // nothing to delete in an empty slot
		} else {
			out['e'] = rowAction{edit: func(m *model, row selRow) formSpec { return keyEditForm(m, netName, slot) }}
			label := "disable"
			if !k.Enabled {
				label = "enable"
			}
			out[' '] = rowAction{label: label, run: func(m *model, row selRow) mutationResult {
				verb := "disable"
				if !k.Enabled {
					verb = "enable"
				}
				return runLeaf(m.cliArgs("key", verb, strconv.Itoa(slot), "-net", netName)...)
			}}
		}
		return out
	},
}

func findKeySlot(m *model, netName string, slot int) *config.KeySlot {
	n := findNetwork(m, netName)
	if n == nil || slot < 0 || slot >= len(n.Keys) {
		return nil
	}
	return &n.Keys[slot]
}

// keyGenerateForm covers an empty slot: generate a fresh key (the CLI does
// the random generation; this collects only what a human would type
// alongside it) or paste one being imported from elsewhere.
func keyGenerateForm(netName string, slot int) formSpec {
	return formSpec{
		title: "key slot " + strconv.Itoa(slot) + " on " + netName + " (empty)",
		fields: []formField{
			{key: "mode", label: "mode", kind: fieldSelect, value: "generate", options: []string{"generate", "import"},
				help: "generate: gravinet creates a random key. import: paste one distributed to you."},
			{key: "keyval", label: "key (import only)", kind: fieldText},
			{key: "label", label: "label", kind: fieldText},
			{key: "notes", label: "notes", kind: fieldText},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			args := []string{"key"}
			if v["mode"] == "import" {
				if v["keyval"] == "" {
					return mutationResult{ok: false, detail: "paste the key to import, or switch mode to generate"}
				}
				args = append(args, "set", strconv.Itoa(slot), v["keyval"])
			} else {
				args = append(args, "generate", strconv.Itoa(slot))
			}
			args = append(args, "-net", netName)
			if v["label"] != "" {
				args = append(args, "-label", v["label"])
			}
			if v["notes"] != "" {
				args = append(args, "-notes", v["notes"])
			}
			return runLeaf(m.cliArgs(args...)...)
		},
	}
}

// keyEditForm covers a populated slot: label and notes (key material itself
// is never shown or re-enterable here — the only ways this console touches
// key material at all are the explicit, one-shot generate/import above,
// never something a form silently carries as a pre-filled value).
func keyEditForm(m *model, netName string, slot int) formSpec {
	k := findKeySlot(m, netName, slot)
	label, notes := "", ""
	if k != nil {
		label, notes = k.Label, k.Notes
	}
	return formSpec{
		title: "edit key slot " + strconv.Itoa(slot) + " on " + netName,
		fields: []formField{
			{key: "label", label: "label", kind: fieldText, value: label,
				help: "no CLI verb sets label alone yet — see the note if you change this"},
			{key: "notes", label: "notes", kind: fieldText, value: notes},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			var results []mutationResult
			if v["notes"] != notes {
				results = append(results, runLeaf(m.cliArgs("key", "notes", strconv.Itoa(slot), v["notes"], "-net", netName)...))
			}
			if v["label"] != label {
				results = append(results, mutationResult{ok: false,
					detail: "label can't be changed on its own yet — cmd/gravinet has no verb for it independent of " +
						"setting the key itself (\"gravinet key set\" takes -label alongside a new key value). " +
						"Notes above were still applied if you changed them."})
			}
			return combineResults(results)
		},
	}
}

// ---- seeds --------------------------------------------------------------

var seedsActions = sectionActionSet{
	add: func(m *model) formSpec {
		net := currentNetworkHint(m)
		return formSpec{
			title: "add seed" + netHintSuffix(net),
			fields: []formField{
				{key: "net", label: "network", kind: fieldText, value: net, help: "leave as shown if this node has only one network"},
				{key: "addr", label: "address", kind: fieldText, help: "host:port this node dials to find/reconnect to peers"},
				{key: "notes", label: "notes", kind: fieldText},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["addr"] == "" {
					return mutationResult{ok: false, detail: "an address is required"}
				}
				args := []string{"seed", "add", v["addr"]}
				if v["net"] != "" {
					args = append(args, "-net", v["net"])
				}
				if v["notes"] != "" {
					args = append(args, "-notes", v["notes"])
				}
				return runLeaf(m.cliArgs(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		netName, addr, ok := splitSeedRowID(row.id)
		if !ok {
			return nil
		}
		sd := findSeed(m, netName, addr)
		out := map[rune]rowAction{
			'e': {edit: func(m *model, row selRow) formSpec { return seedEditForm(m, netName, addr) }},
			'd': {confirm: "Remove seed " + addr + " from " + netName + "?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("seed", "remove", addr, "-net", netName)...)
				}},
		}
		label := "disable"
		if sd != nil && sd.Disabled {
			label = "enable"
		}
		out[' '] = rowAction{label: label, run: func(m *model, row selRow) mutationResult {
			verb := "disable"
			if sd != nil && sd.Disabled {
				verb = "enable"
			}
			return runLeaf(m.cliArgs("seed", verb, addr, "-net", netName)...)
		}}
		return out
	},
}

func splitSeedRowID(id string) (netName, addr string, ok bool) {
	for i := 0; i < len(id); i++ {
		if id[i] == idSep[0] {
			return id[:i], id[i+1:], true
		}
	}
	return "", "", false
}

func findSeed(m *model, netName, addr string) *config.Seed {
	n := findNetwork(m, netName)
	if n == nil {
		return nil
	}
	for i := range n.Seeds {
		if n.Seeds[i].Address == addr {
			return &n.Seeds[i]
		}
	}
	return nil
}

func seedEditForm(m *model, netName, addr string) formSpec {
	sd := findSeed(m, netName, addr)
	notes := ""
	if sd != nil {
		notes = sd.Notes
	}
	return formSpec{
		title: "edit seed " + addr + " on " + netName,
		fields: []formField{
			{key: "notes", label: "notes", kind: fieldText, value: notes},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["notes"] == notes {
				return mutationResult{ok: true, detail: "nothing changed"}
			}
			return runLeaf(m.cliArgs("seed", "notes", addr, v["notes"], "-net", netName)...)
		},
	}
}

// currentNetworkHint picks a default network name for an add form — the one
// the operator is already looking at wherever that's unambiguous, so the
// common single-network node never has to type it.
func currentNetworkHint(m *model) string {
	if m.snap == nil || m.snap.cfg == nil || len(m.snap.cfg.Networks) != 1 {
		return ""
	}
	return m.snap.cfg.Networks[0].Name
}

func netHintSuffix(net string) string {
	if net == "" {
		return ""
	}
	return " on " + net
}

// ---- peers ----------------------------------------------------------------
//
// Peers has no CLI verb for enable/disable/notes at all — see mutate.go's
// package comment and the PeerSetEnabled/PeerSetNotes grep that confirmed
// it. Both go through the direct-config path.

var peersActions = sectionActionSet{
	row: func(m *model, row selRow) map[rune]rowAction {
		netName, nodeID, ok := splitSeedRowID(row.id) // same "net\x1fid" shape as a seed row
		if !ok {
			return nil
		}
		disabled := isPeerDisabled(m, netName, nodeID)
		label := "disable"
		if disabled {
			label = "enable"
		}
		return map[rune]rowAction{
			'e': {edit: func(m *model, row selRow) formSpec { return peerNotesForm(m, netName, nodeID) }},
			' ': {label: label, run: func(m *model, row selRow) mutationResult {
				return setPeerEnabled(m, netName, nodeID, isPeerDisabled(m, netName, nodeID))
			}},
			'd': {confirm: "Ban " + shortID(nodeID) + "? This blocks it from joining or reconnecting on any network, not just " + netName + ".",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgsSock("ban", nodeID, "-net", netName)...)
				}},
		}
	},
}

func isPeerDisabled(m *model, netName, nodeID string) bool {
	n := findNetwork(m, netName)
	if n == nil {
		return false
	}
	for _, id := range n.DisabledPeers {
		if id == nodeID {
			return true
		}
	}
	return false
}

// setPeerEnabled calls the config setter directly (no CLI verb exists) and
// commits. wasDisabled is passed in rather than re-read, so the toggle acts
// on the state the operator saw when they pressed space, not a state that
// might have changed by the time this runs.
func setPeerEnabled(m *model, netName, nodeID string, wasDisabled bool) mutationResult {
	if m.snap == nil || m.snap.cfg == nil {
		return mutationResult{ok: false, detail: "no config loaded"}
	}
	if err := m.snap.cfg.PeerSetEnabled(netName, nodeID, wasDisabled); err != nil {
		return mutationResult{ok: false, detail: err.Error()}
	}
	return commitConfig(m.snap.cfg, m.cfgPath, m.sockPath)
}

func peerNotesForm(m *model, netName, nodeID string) formSpec {
	n := findNetwork(m, netName)
	notes := ""
	if n != nil {
		notes = n.PeerNotes[nodeID]
	}
	return formSpec{
		title: "notes on " + shortID(nodeID) + " (" + netName + ")",
		fields: []formField{
			{key: "notes", label: "notes", kind: fieldText, value: notes},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			if m.snap == nil || m.snap.cfg == nil {
				return mutationResult{ok: false, detail: "no config loaded"}
			}
			if err := m.snap.cfg.PeerSetNotes(netName, nodeID, v["notes"]); err != nil {
				return mutationResult{ok: false, detail: err.Error()}
			}
			return commitConfig(m.snap.cfg, m.cfgPath, m.sockPath)
		},
	}
}

// ---- bans -----------------------------------------------------------------

var bansActions = sectionActionSet{
	add: func(m *model) formSpec {
		net := currentNetworkHint(m)
		return formSpec{
			title: "ban a node" + netHintSuffix(net),
			fields: []formField{
				{key: "net", label: "network", kind: fieldText, value: net},
				{key: "node", label: "node id", kind: fieldText, help: "full id or the prefix shown on Mesh \u203a Peers"},
				{key: "notes", label: "notes", kind: fieldText},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["node"] == "" {
					return mutationResult{ok: false, detail: "a node id is required"}
				}
				args := []string{"ban", v["node"]}
				if v["net"] != "" {
					args = append(args, "-net", v["net"])
				}
				if v["notes"] != "" {
					args = append(args, "-notes", v["notes"])
				}
				return runLeaf(m.cliArgsSock(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		if m.snap == nil {
			return nil
		}
		netName, target, ok := splitSeedRowID(row.id) // same "net\x1fid" shape as a seed/peer row
		if !ok {
			return nil
		}
		var mine bool
		for _, b := range m.snap.bans {
			if b.net == netName && b.Target == target {
				mine = b.Mine
				break
			}
		}
		if !mine {
			// Only this node's own bans can be lifted from here — same
			// restriction pageBans already notes. No 'd' action at all
			// rather than one that would just fail: pressing it here
			// should tell the operator why nothing happened, and "no such
			// action" (dispatchRowAction's own message when a key isn't
			// registered) says exactly that.
			return nil
		}
		return map[rune]rowAction{
			'd': {confirm: "Unban " + shortID(target) + " on " + netName + "?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgsSock("unban", target, "-net", netName)...)
				}},
		}
	},
}
