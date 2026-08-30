package tui

// System group row actions. Verified against cmd/gravinet's cli_system.go
// leaf by leaf before any of this was written — see each function's own
// comment for the specific usage string it was built from, and mutate.go's
// package comment for why cliArgs/cliArgsSock/cliArgsBare are three
// different functions rather than one: System is the group where all three
// shapes actually appear side by side (lldp/config-history are config-file
// leaves, upgrade is control-socket-only, resolver/time/syslog/users/power
// are bare host-OS operations with no flag for either).

func init() {
	registerActions("lldp", lldpActions)
	registerActions("syslog", syslogActions)
	registerActions("users", usersActions)
	registerActions("config-history", configHistoryActions)
}

// ---- lldp interfaces ------------------------------------------------------
//
// "system lldp iface add|del NAME" — confirmed config-file (openCfg), no
// -sock. Adding always turns both LLDP and CDP on together (there is no
// finer-grained verb); see pageLLDP's own note for why that's stated rather
// than worked around.

var lldpActions = sectionActionSet{
	add: func(m *model) formSpec {
		return formSpec{
			title:  "add an lldp/cdp interface",
			fields: []formField{{key: "iface", label: "iface", kind: fieldText}},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["iface"] == "" {
					return mutationResult{ok: false, detail: "an interface name is required"}
				}
				return runLeaf(m.cliArgs("system", "lldp", "iface", "add", v["iface"])...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		return map[rune]rowAction{
			'd': {confirm: "Stop discovery on " + row.id + "?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("system", "lldp", "iface", "del", row.id)...)
				}},
		}
	},
}

// ---- syslog targets ---------------------------------------------------
//
// "system syslog add HOST [-proto udp|tcp] [-port N]" / "del HOST" —
// confirmed bare host-OS (service.SetHostSyslog), no -config, no -sock.

var syslogActions = sectionActionSet{
	add: func(m *model) formSpec {
		return formSpec{
			title: "add a syslog collector",
			fields: []formField{
				{key: "host", label: "host", kind: fieldText},
				{key: "proto", label: "protocol", kind: fieldSelect, value: "udp", options: []string{"udp", "tcp"}},
				{key: "port", label: "port", kind: fieldText, value: "514"},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["host"] == "" {
					return mutationResult{ok: false, detail: "a host is required"}
				}
				args := []string{"system", "syslog", "add", v["host"], "-proto", v["proto"]}
				if v["port"] != "" {
					args = append(args, "-port", v["port"])
				}
				return runLeaf(m.cliArgsBare(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		return map[rune]rowAction{
			'd': {confirm: "Remove syslog collector " + row.id + "?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgsBare("system", "syslog", "del", row.id)...)
				}},
		}
	},
}

// ---- users ----------------------------------------------------------------
//
// "system users add NAME [-pass PW] [-expires YYYY-MM-DD]" / "passwd NAME
// [-pass PW]" / "expiry NAME [DATE]" / "del NAME" — confirmed bare host-OS
// (service.AddSystemUser and friends), no -config, no -sock. -pass is
// always passed explicitly here, never omitted: cmdSystemUsers prompts on
// stdin when it's missing (readPassword's fmt.Scanln), and this console's
// subprocess has no stdin for a human to answer that prompt on — an empty
// field is refused before ever shelling out, rather than left to become a
// hung or failed subprocess.

var usersActions = sectionActionSet{
	add: func(m *model) formSpec {
		return formSpec{
			title: "add a console user",
			fields: []formField{
				{key: "name", label: "name", kind: fieldText},
				{key: "pass", label: "password", kind: fieldText},
				{key: "expires", label: "expires (YYYY-MM-DD)", kind: fieldText, help: "blank = never"},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["name"] == "" || v["pass"] == "" {
					return mutationResult{ok: false, detail: "a name and a password are required"}
				}
				args := []string{"system", "users", "add", v["name"], "-pass", v["pass"]}
				if v["expires"] != "" {
					args = append(args, "-expires", v["expires"])
				}
				return runLeaf(m.cliArgsBare(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		return map[rune]rowAction{
			'e': {edit: func(m *model, row selRow) formSpec { return userExpiryForm(row.id) }},
			'd': {confirm: "Delete console user " + row.id + "?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgsBare("system", "users", "del", row.id)...)
				}},
		}
	},
}

func userExpiryForm(name string) formSpec {
	return formSpec{
		title: "expiry for " + name,
		fields: []formField{
			{key: "date", label: "expires (YYYY-MM-DD)", kind: fieldText, help: "blank clears it (never expires)"},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			args := []string{"system", "users", "expiry", name}
			if v["date"] != "" {
				args = append(args, v["date"])
			}
			return runLeaf(m.cliArgsBare(args...)...)
		},
	}
}

// ---- config history ---------------------------------------------------
//
// "system config-history diff ID" / "restore ID" / "snapshot" — confirmed
// config-file (openCfg), no -sock. diff is read-only but routed through the
// same runLeaf path as everything else: its own stdout (a section-by-section
// summary of what differs) is exactly what a result screen is for, and
// reusing that machinery beats inventing a second way to show text.

var configHistoryActions = sectionActionSet{
	add: func(m *model) formSpec {
		return formSpec{
			title:  "take a config snapshot now",
			fields: []formField{{key: "confirm", label: "snapshot now", kind: fieldBool, value: "true"}},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["confirm"] != "true" {
					return mutationResult{ok: true, detail: "nothing changed"}
				}
				return runLeaf(m.cliArgs("system", "config-history", "snapshot")...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		return map[rune]rowAction{
			'e': {run: func(m *model, row selRow) mutationResult {
				return runLeaf(m.cliArgs("system", "config-history", "diff", row.id)...)
			}},
			'd': {confirm: "Restore snapshot " + row.id + "? This overwrites the current config.",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("system", "config-history", "restore", row.id)...)
				}},
		}
	},
}
