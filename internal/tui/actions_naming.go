package tui

// Naming group actions. Verified against cmd/gravinet's cmdNamingDNS and
// cmdHost (cli_groups.go/cli_config.go), which are the same shape as each
// other and as the Mesh group's per-network leaves — the shape this whole
// file leans on: "add" is an upsert (config.DNSForwardAdd/HostAdd both
// replace an existing entry in place rather than duplicating one), so
// "edit" is just "add" again with a new value, confirmed by reading each
// setter rather than assumed from the naming convention.

func init() {
	registerActions("dns", dnsActions)
	registerActions("hosts", hostsActions)
}

// ---- dns --------------------------------------------------------------

var dnsActions = sectionActionSet{
	add: func(m *model) formSpec {
		net := currentNetworkHint(m)
		return formSpec{
			title: "advertise a dns forward" + netHintSuffix(net),
			fields: []formField{
				{key: "net", label: "network", kind: fieldText, value: net},
				{key: "domain", label: "domain", kind: fieldText},
				{key: "servers", label: "servers", kind: fieldText, help: "comma-separated"},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["domain"] == "" || v["servers"] == "" {
					return mutationResult{ok: false, detail: "a domain and at least one server are required"}
				}
				args := []string{"naming", "dns", "add", v["domain"], v["servers"]}
				if v["net"] != "" {
					args = append(args, "-net", v["net"])
				}
				return runLeaf(m.cliArgs(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		netName, domain, ok := splitSeedRowID(row.id)
		if !ok {
			return nil
		}
		switch row.tableKey {
		case "dns-fwd":
			f := dnsForwardFor(m, netName, domain)
			label := "disable"
			if f != nil && f.Disabled {
				label = "enable"
			}
			return map[rune]rowAction{
				'e': {label: "servers", edit: func(m *model, row selRow) formSpec { return dnsForwardEditForm(m, netName, domain) }},
				'd': {label: "remove", confirm: "Stop advertising " + domain + " on " + netName + "?",
					run: func(m *model, row selRow) mutationResult {
						return runLeaf(m.cliArgs("naming", "dns", "remove", domain, "-net", netName)...)
					}},
				' ': {label: label, run: func(m *model, row selRow) mutationResult {
					verb := "disable"
					if f != nil && f.Disabled {
						verb = "enable"
					}
					return runLeaf(m.cliArgs("naming", "dns", verb, domain, "-net", netName)...)
				}},
			}
		case "dns-reject":
			r := dnsRejectFor(m, netName, domain)
			label := "disable"
			if r != nil && r.Disabled {
				label = "enable"
			}
			return map[rune]rowAction{
				'd': {label: "remove", confirm: "Stop rejecting " + domain + " on " + netName + "?",
					run: func(m *model, row selRow) mutationResult {
						return runLeaf(m.cliArgs("naming", "dns", "reject-remove", domain, "-net", netName)...)
					}},
				' ': {label: label, run: func(m *model, row selRow) mutationResult {
					verb := "reject-disable"
					if r != nil && r.Disabled {
						verb = "reject-enable"
					}
					return runLeaf(m.cliArgs("naming", "dns", verb, domain, "-net", netName)...)
				}},
			}
		}
		return nil
	},
}

func dnsForwardFor(m *model, netName, domain string) *forwardRow {
	n := findNetwork(m, netName)
	if n == nil {
		return nil
	}
	for _, f := range n.DNSAdvertise {
		if f.Domain == domain {
			return &forwardRow{Servers: f.Servers, Disabled: f.Disabled}
		}
	}
	return nil
}

// forwardRow is a tiny local copy of the two fields dnsForwardEditForm
// needs, so this file doesn't need to know config.DNSForward's exact type
// name at this call site.
type forwardRow struct {
	Servers  []string
	Disabled bool
}

func dnsRejectFor(m *model, netName, domain string) *rejectRow {
	n := findNetwork(m, netName)
	if n == nil {
		return nil
	}
	for _, r := range n.DNSReject {
		if r.Domain == domain {
			return &rejectRow{Disabled: r.Disabled}
		}
	}
	return nil
}

type rejectRow struct{ Disabled bool }

func dnsForwardEditForm(m *model, netName, domain string) formSpec {
	f := dnsForwardFor(m, netName, domain)
	servers := ""
	if f != nil {
		servers = joinOr(f.Servers, "")
	}
	return formSpec{
		title: "edit dns forward " + domain + " (" + netName + ")",
		fields: []formField{
			{key: "servers", label: "servers", kind: fieldText, value: servers, help: "comma-separated"},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["servers"] == "" {
				return mutationResult{ok: false, detail: "at least one server is required"}
			}
			// "dns add" upserts (config.DNSForwardAdd replaces an existing
			// domain's server list rather than erroring) — confirmed by
			// reading the setter, which is why editing is just re-adding.
			return runLeaf(m.cliArgs("naming", "dns", "add", domain, v["servers"], "-net", netName)...)
		},
	}
}

// ---- hosts --------------------------------------------------------------

var hostsActions = sectionActionSet{
	add: func(m *model) formSpec {
		net := currentNetworkHint(m)
		return formSpec{
			title: "advertise a host record" + netHintSuffix(net),
			fields: []formField{
				{key: "net", label: "network", kind: fieldText, value: net},
				{key: "name", label: "name", kind: fieldText},
				{key: "ip", label: "address", kind: fieldText},
			},
			submit: func(m *model, v map[string]string) mutationResult {
				if v["name"] == "" || v["ip"] == "" {
					return mutationResult{ok: false, detail: "a name and an address are required"}
				}
				args := []string{"naming", "hosts", "add", v["name"], v["ip"]}
				if v["net"] != "" {
					args = append(args, "-net", v["net"])
				}
				return runLeaf(m.cliArgs(args...)...)
			},
		}
	},
	row: func(m *model, row selRow) map[rune]rowAction {
		netName, name, ok := splitSeedRowID(row.id)
		if !ok {
			return nil
		}
		h := hostRecordFor(m, netName, name)
		label := "disable"
		if h != nil && h.Disabled {
			label = "enable"
		}
		return map[rune]rowAction{
			'e': {label: "address", edit: func(m *model, row selRow) formSpec { return hostEditForm(m, netName, name) }},
			'd': {label: "remove", confirm: "Stop advertising " + name + " on " + netName + "?",
				run: func(m *model, row selRow) mutationResult {
					return runLeaf(m.cliArgs("naming", "hosts", "remove", name, "-net", netName)...)
				}},
			' ': {label: label, run: func(m *model, row selRow) mutationResult {
				verb := "disable"
				if h != nil && h.Disabled {
					verb = "enable"
				}
				return runLeaf(m.cliArgs("naming", "hosts", verb, name, "-net", netName)...)
			}},
		}
	},
}

type hostRow struct {
	IP       string
	Disabled bool
}

func hostRecordFor(m *model, netName, name string) *hostRow {
	n := findNetwork(m, netName)
	if n == nil {
		return nil
	}
	for _, h := range n.HostsAdvertise {
		if h.Name == name {
			return &hostRow{IP: h.IP, Disabled: h.Disabled}
		}
	}
	return nil
}

func hostEditForm(m *model, netName, name string) formSpec {
	h := hostRecordFor(m, netName, name)
	ip := ""
	if h != nil {
		ip = h.IP
	}
	return formSpec{
		title: "edit host record " + name + " (" + netName + ")",
		fields: []formField{
			{key: "ip", label: "address", kind: fieldText, value: ip},
		},
		submit: func(m *model, v map[string]string) mutationResult {
			if v["ip"] == "" {
				return mutationResult{ok: false, detail: "an address is required"}
			}
			// "host add" upserts (config.HostAdd replaces an existing
			// name's address rather than erroring) — same confirmed pattern
			// as DNS forwards above.
			return runLeaf(m.cliArgs("naming", "hosts", "add", name, v["ip"], "-net", netName)...)
		},
	}
}
