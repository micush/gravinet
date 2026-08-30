package tui

// Settings actions.
//
// Verified against cmd/gravinet/cli_settings.go directly: nearly every row
// on this page is one of exactly two shapes — settingsBoolSet
// ("gravinet settings X on|off") or settingsIntSet ("gravinet settings X N")
// — which that file's own comment says explicitly, and which is what makes
// generic form builders below the right call rather than one hand-written
// function per row: the shape is a genuine property of the CLI, not a
// convenience this package invented. A handful of rows don't fit either
// shape (login lockout takes two numbers, listen addresses and log level
// take a bare string, the two port lists take a comma-list-or-"-", DDNS's
// key is a secret) and each of those gets its own small form instead of
// being forced into the generic shape.
//
// Two rows on the web admin's Settings page have no CLI form at all, and
// still don't here, for the same reasons cli_settings.go's own package
// comment gives: Dark mode is a per-browser preference with nothing in
// config.json behind it (this console's counterpart is the t key), and the
// TLS certificate upload wants two pasted PEM blobs and deliberately never
// auto-restarts, since the browser it could lock out is the one asking.

import (
	"strconv"
	"strings"
)

// boolSettingForm builds the single-field on/off form every
// "gravinet settings X on|off" row uses.
func boolSettingForm(title, leaf string, current bool) formSpec {
	return verbBoolForm(title, []string{"settings", leaf}, "on", "off", current)
}

// topLevelBoolForm is boolSettingForm's counterpart for managed/manager,
// which predate the settings group and sit at the top level
// ("gravinet managed on|off", not "gravinet settings managed on|off").
func topLevelBoolForm(title, cmd string, current bool) formSpec {
	return verbBoolForm(title, []string{cmd}, "on", "off", current)
}

// verbBoolForm is the fully general form behind boolSettingForm/
// topLevelBoolForm: prefix is the command and any leading subcommands
// (e.g. []string{"nat"} or []string{"settings", "shell"}), onVerb/offVerb
// are whatever words that specific leaf wants ("on"/"off" for most of the
// settings group, "enable"/"disable" for nat/qos/lldp/snmp/discovery — each
// leaf's own usage string decides which, and it is read from there, not
// assumed).
func verbBoolForm(title string, prefix []string, onVerb, offVerb string, current bool) formSpec {
	return formSpec{
		title:  title,
		fields: []formField{{key: "on", label: "on", kind: fieldBool, value: onOffBool(current)}},
		submit: func(m *model, v map[string]string) mutationResult {
			verb := offVerb
			if v["on"] == "true" {
				verb = onVerb
			}
			return runLeaf(m.cliArgs(append(append([]string{}, prefix...), verb)...)...)
		},
	}
}

// intSettingForm builds the single-field numeric form every
// "gravinet settings X N" row uses.
func intSettingForm(title, leaf string, current int, help string) formSpec {
	return formSpec{
		title:  title,
		fields: []formField{{key: "n", label: "value", kind: fieldText, value: strconv.Itoa(current), help: help}},
		submit: func(m *model, v map[string]string) mutationResult {
			n, err := strconv.Atoi(strings.TrimSpace(v["n"]))
			if err != nil {
				return mutationResult{ok: false, detail: "must be a whole number"}
			}
			return runLeaf(m.cliArgs("settings", leaf, strconv.Itoa(n))...)
		},
	}
}

// textSettingForm builds the single-field free-text form a bare-string
// setting (log level, log size cap) uses.
func textSettingForm(title, leaf, current, help string) formSpec {
	return formSpec{
		title:  title,
		fields: []formField{{key: "v", label: "value", kind: fieldText, value: current, help: help}},
		submit: func(m *model, v map[string]string) mutationResult {
			if strings.TrimSpace(v["v"]) == "" {
				return mutationResult{ok: false, detail: "a value is required"}
			}
			return runLeaf(m.cliArgs("settings", leaf, v["v"])...)
		},
	}
}

// portListForm builds the udp-port/tcp-port form: a comma-separated list,
// or "-" to turn that transport off.
func portListForm(title, leaf string, current []int) formSpec {
	return formSpec{
		title: title,
		fields: []formField{
			{key: "v", label: "ports", kind: fieldText, value: portsOrOff(current),
				help: "comma-separated, e.g. 65432,443 — or \"-\" to turn this transport off (the other must stay on)"},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			val := strings.TrimSpace(v["v"])
			if val == "off" {
				val = "-"
			}
			if val == "" {
				return mutationResult{ok: false, detail: "a port list or \"-\" is required"}
			}
			return runLeaf(m.cliArgs("settings", leaf, val)...)
		},
	}
}

func loginBanForm(m *model) formSpec {
	wa := m.snap.cfg.WebAdmin
	return formSpec{
		title: "login lockout",
		fields: []formField{
			{key: "attempts", label: "attempts", kind: fieldText, value: strconv.Itoa(wa.LoginBan.EffectiveMaxFailures())},
			{key: "seconds", label: "seconds", kind: fieldText, value: strconv.Itoa(wa.LoginBan.EffectiveBanSeconds()),
				help: "0 for either restores its default. Needs a restart to take effect."},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			attempts, err1 := strconv.Atoi(strings.TrimSpace(v["attempts"]))
			secs, err2 := strconv.Atoi(strings.TrimSpace(v["seconds"]))
			if err1 != nil || err2 != nil {
				return mutationResult{ok: false, detail: "both attempts and seconds must be whole numbers"}
			}
			return runLeaf(m.cliArgs("settings", "login-ban", strconv.Itoa(attempts), strconv.Itoa(secs))...)
		},
	}
}

func listenAddrsForm(m *model) formSpec {
	current := "default"
	if addrs := m.snap.cfg.ListenAddrsRaw(); len(addrs) > 0 {
		current = strings.Join(addrs, ",")
	}
	return formSpec{
		title: "web admin listen addresses",
		fields: []formField{
			{key: "v", label: "addresses", kind: fieldText, value: current,
				help: "comma-separated, or \"default\" for loopback + this node's mesh addresses. " +
					"This is the setting that can take the web admin away from you — this console is then the only way back."},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			val := strings.TrimSpace(v["v"])
			if val == "" {
				return mutationResult{ok: false, detail: "a value is required"}
			}
			return runLeaf(m.cliArgs("settings", "listen-addrs", val)...)
		},
	}
}

// ddnsForm covers interval/ttl/reverse in one form — three fields, one
// commit each on submit (only the ones that changed), the same
// changed-fields-only pattern the Mesh group's multi-field edit forms use.
// TSIG key is deliberately not a field here: it is a secret, shown nowhere
// (same posture as Mesh key material), and gets its own explicit
// set-a-new-one form (ddnsKeyForm) instead of ever appearing pre-filled.
func ddnsForm(m *model) formSpec {
	d := m.snap.cfg.DDNS
	interval, ttl, reverse := strconv.Itoa(d.IntervalMinutes), strconv.Itoa(d.TTL), onOffBool(d.ReverseEnabled())
	return formSpec{
		title: "dynamic dns",
		fields: []formField{
			{key: "interval", label: "interval (minutes)", kind: fieldText, value: interval, help: "0 turns registration off"},
			{key: "ttl", label: "ttl (seconds)", kind: fieldText, value: ttl},
			{key: "reverse", label: "reverse records", kind: fieldBool, value: reverse},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			var results []mutationResult
			if v["interval"] != interval {
				results = append(results, runLeaf(m.cliArgs("settings", "ddns", "interval", v["interval"])...))
			}
			if v["ttl"] != ttl {
				results = append(results, runLeaf(m.cliArgs("settings", "ddns", "ttl", v["ttl"])...))
			}
			if v["reverse"] != reverse {
				verb := "off"
				if v["reverse"] == "true" {
					verb = "on"
				}
				results = append(results, runLeaf(m.cliArgs("settings", "ddns", "reverse", verb)...))
			}
			return combineResults(results)
		},
	}
}

func ddnsKeyForm(m *model) formSpec {
	return formSpec{
		title: "dynamic dns TSIG key",
		fields: []formField{
			{key: "v", label: "key", kind: fieldText, help: "name:base64secret[:algorithm] — or leave blank and submit to clear it"},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			val := strings.TrimSpace(v["v"])
			if val == "" {
				val = "-"
			}
			return runLeaf(m.cliArgs("settings", "ddns", "key", val)...)
		},
	}
}
