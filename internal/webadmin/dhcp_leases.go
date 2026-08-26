package webadmin

// Reading Kea's leases back, for Monitor > DHCP. The read-only counterpart to
// the DHCP editor the way Monitor > BGP Peers is to Traffic > BGP and Monitor
// > L2 Peers is to System > LLDP: gravinet configures a daemon, and this shows
// what that daemon is actually doing.
//
// # Why the CSV and not the control agent
//
// Kea can also answer lease4-get-all over its control agent, which returns
// clean JSON and needs no parsing rules at all. It also needs kea-ctrl-agent
// installed and a control socket configured, neither of which gravinet sets up
// — so it would mean adding a package and a listening socket to every DHCP
// server node in order to render a read-only table. The memfile CSV is already
// there, because renderKea points Kea at it (see keaLeasePath).
//
// # The file is a journal, not a table
//
// Every lease event appends a row. One address can appear many times, and the
// last row for an address is its current state. This was verified against a
// real Kea 2.4.1 rather than taken from the docs, and two of the findings are
// not guessable:
//
//   - A declined lease has its hwaddr, client_id and hostname *blanked* by
//     Kea. There is no way to report who declined an address, so the UI shows
//     the address and the state and nothing else. Reporting a stale client
//     from an earlier row would be worse than reporting nothing.
//   - A declined lease's valid_lifetime is the decline probation period
//     (86400 by default), not the subnet's lifetime. It is not a lease anyone
//     holds and its expiry means "when the address returns to the pool", so
//     the UI must not render it in the same column as a real lease's expiry.
//
// A release produces two rows: the lease with valid_lifetime 0 and an expiry
// in the past, then a second row in state 2 with the hostname cleared.
//
// # Agreeing with lease file cleanup
//
// Kea compacts this file periodically (LFC, hourly by default). The rules
// below — last row wins, drop expired-reclaimed, keep declined — are the same
// rules LFC applies, which was confirmed by running kea-lfc over a real
// journal and diffing: a 9-row journal and its 4-row compaction produce an
// identical view through this reader. That is the property worth having. It
// means the page does not appear to change contents at the top of the hour
// just because LFC happened to run.
//
// LFC works by renaming the live file aside and writing a fresh one, so a read
// can land in a window where the main file does not exist. That is not an
// error and must not render as one — see readKeaLeases' fallback.

import (
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/config"
)

// Kea memfile lease states, from the CSV's state column. Verified against a
// real server rather than assumed: state is the field that decides whether a
// row is a lease at all.
const (
	keaStateDefault   = 0 // a normal lease
	keaStateDeclined  = 1 // client refused the address; probation, no client info
	keaStateReclaimed = 2 // expired and reclaimed; the address is free
)

// dhcpLease is one current lease, as Monitor > DHCP shows it.
type dhcpLease struct {
	Address  string `json:"address"`
	HWAddr   string `json:"hwaddr,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	SubnetID int    `json:"subnet_id"`
	// Expire is the unix time the lease runs out, and ValidLifetime the
	// lifetime it was granted with. Both are reported raw so the UI can show
	// a countdown without this package deciding on a format.
	Expire        int64 `json:"expire"`
	ValidLifetime int   `json:"valid_lifetime"`
	// State is the raw Kea state; Declined is the one callers act on, since
	// such a row carries no client identity and is not a lease anybody holds.
	State    int  `json:"state"`
	Declined bool `json:"declined"`
}

// keaLeaseColumns is the header renderKea's Kea version writes. Checked rather
// than assumed: the reader indexes by name resolved from the header, so a Kea
// that adds a column in the middle (pool_id was added in 2.3) does not silently
// shift every field by one.
const (
	colAddress       = "address"
	colHWAddr        = "hwaddr"
	colClientID      = "client_id"
	colValidLifetime = "valid_lifetime"
	colExpire        = "expire"
	colSubnetID      = "subnet_id"
	colHostname      = "hostname"
	colState         = "state"
)

// readKeaLeases returns the current leases from Kea's memfile database at
// path, newest state per address, as of now.
//
// A missing file is not an error: it is what a node that has never run Kea
// looks like, and what a node looks like for the moment LFC has the live file
// renamed aside. In the latter case the compacted copy Kea leaves at
// "<path>.2" is read instead, which is the same data one compaction older —
// strictly better than reporting an empty lease table to an operator whose
// LAN is fully leased.
func readKeaLeases(path string, now time.Time) ([]dhcpLease, error) {
	rows, err := readKeaLeaseFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// LFC window, or Kea never started. Try the file LFC renames aside
		// before concluding there are no leases.
		if rows, err = readKeaLeaseFile(path + ".2"); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return currentLeases(rows, now), nil
}

// readKeaLeaseFile parses every row of a memfile CSV in file order.
//
// Kea appends while this reads, and a row can be torn — the last line may be
// half-written at the moment of the read. csv.Reader with FieldsPerRecord
// disabled tolerates that by yielding a short record, which parseLeaseRow then
// rejects; a torn final row is dropped rather than failing the whole read. A
// lease table missing its newest row for a few hundred milliseconds is a
// non-event; an error page because a client happened to renew mid-request is
// not.
func readKeaLeaseFile(path string) ([]dhcpLease, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1 // tolerate a torn trailing row
	cr.ReuseRecord = true

	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil // header-only: Kea started, nobody has leased yet
	}
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i, name := range header {
		idx[strings.TrimSpace(name)] = i
	}
	if _, ok := idx[colAddress]; !ok {
		return nil, errors.New("not a Kea lease file: no address column")
	}

	var out []dhcpLease
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A parse error part-way through is the same torn-write case as a
			// short record; keep what was read rather than discarding a whole
			// good table for one bad line.
			break
		}
		if l, ok := parseLeaseRow(rec, idx); ok {
			out = append(out, l)
		}
	}
	return out, nil
}

func parseLeaseRow(rec []string, idx map[string]int) (dhcpLease, bool) {
	get := func(name string) string {
		i, ok := idx[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	addr := get(colAddress)
	if addr == "" {
		return dhcpLease{}, false
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	state := atoi(get(colState))
	return dhcpLease{
		Address:       addr,
		HWAddr:        get(colHWAddr),
		ClientID:      get(colClientID),
		Hostname:      get(colHostname),
		SubnetID:      atoi(get(colSubnetID)),
		ValidLifetime: atoi(get(colValidLifetime)),
		Expire:        int64(atoi(get(colExpire))),
		State:         state,
		Declined:      state == keaStateDeclined,
	}, true
}

// currentLeases collapses a journal to the current state of each address, in
// stable address order, applying the same rules Kea's own LFC applies.
//
// Rows arrive in file order and the last one for an address wins, so this
// walks forward and overwrites. Order out is by first appearance rather than
// by the map, so two reads of an unchanged file produce the same table and the
// UI does not reshuffle rows under an operator between refreshes.
func currentLeases(rows []dhcpLease, now time.Time) []dhcpLease {
	latest := make(map[string]dhcpLease, len(rows))
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		if _, seen := latest[r.Address]; !seen {
			order = append(order, r.Address)
		}
		latest[r.Address] = r
	}
	unix := now.Unix()
	out := make([]dhcpLease, 0, len(order))
	for _, addr := range order {
		l := latest[addr]
		switch {
		case l.State == keaStateReclaimed:
			// The address went back to the pool. Not a lease.
			continue
		case l.Declined:
			// Kept, because the address is genuinely unavailable and an
			// operator looking for a missing address needs to see why. Kea
			// blanked the client fields on this row; blank them explicitly
			// rather than passing through whatever happened to be there, so
			// no caller is tempted to render a half-identity.
			l.HWAddr, l.ClientID, l.Hostname = "", "", ""
			out = append(out, l)
		case l.Expire > 0 && l.Expire <= unix:
			// Lapsed but not yet reclaimed by Kea. Not a current lease.
			continue
		default:
			out = append(out, l)
		}
	}
	return out
}

// dhcpLeasesJSON is the Monitor > DHCP payload.
//
// Mode is carried alongside the leases because an empty table means three
// different things and the page must not render them identically: a server
// with nobody leasing yet, a relay (whose leases live on the upstream server
// and are not this node's to show), and DHCP switched off entirely. Only the
// first is "no leases"; the other two are "no leases here, and here is why".
type dhcpLeasesJSON struct {
	Supported bool        `json:"supported"`
	Mode      string      `json:"mode"`
	Running   bool        `json:"running"`
	Leases    []dhcpLease `json:"leases"`
	Hint      string      `json:"hint,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// handleDHCPLeases serves the current DHCP leases (GET /api/dhcp-leases).
//
// Read-only, like every other Monitor endpoint: it opens a file Kea wrote and
// never touches Kea, the config, or the service. In particular it does not
// install or start anything — the editor under Network > DHCP is the only
// place that does, and a monitoring page causing a package install as a side
// effect of being opened is exactly the trap installKea's doc comment warns
// about.
func (s *Server) handleDHCPLeases(w http.ResponseWriter, r *http.Request) {
	out := dhcpLeasesJSON{Supported: dhcpSupported(), Leases: []dhcpLease{}}
	if !out.Supported {
		out.Hint = "gravinet drives a DHCP server on Linux only"
		writeJSON(w, http.StatusOK, out)
		return
	}
	cfg, err := config.Load(s.configPath)
	if err != nil {
		out.Error = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Mode = string(cfg.DHCP.Mode)
	out.Running = keaActive()

	switch cfg.DHCP.Mode {
	case config.DHCPRelay:
		// A relay allocates nothing. The leases its clients hold were issued
		// by the upstream server and are recorded there, so there is no local
		// file to read and an empty table here is the correct, complete
		// answer rather than a missing one.
		out.Hint = "this node relays DHCP rather than serving it — leases are held by the upstream server, not here"
		writeJSON(w, http.StatusOK, out)
		return
	case config.DHCPOff:
		// Deliberately still reads the file below if Kea is running: the
		// preflight already warns that a Kea left running by an earlier apply
		// is "handing out leases this page does not manage", and those are
		// exactly the leases an operator needs to see to act on that warning.
		if !out.Running {
			out.Hint = "DHCP is off on this node"
			writeJSON(w, http.StatusOK, out)
			return
		}
		out.Hint = "DHCP is off in gravinet's config, but a Kea server is still running on this host — these are the leases it is handing out"
	}

	leases, err := readKeaLeases(keaLeasePath, time.Now())
	if err != nil {
		out.Error = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	if leases != nil {
		out.Leases = leases
	}
	if len(out.Leases) == 0 && out.Hint == "" && cfg.DHCP.Mode == config.DHCPServer && !out.Running {
		out.Hint = "the DHCP server is configured but not running — check `journalctl -u " + keaUnit() + "`"
	}
	writeJSON(w, http.StatusOK, out)
}

// DHCPLease is one current lease, exported for the CLI. Same data the web
// page renders — "gravinet monitor dhcp-leases" and Monitor > DHCP Leases are
// the same view in two shells, and both go through readKeaLeases so the
// journal rules (last row wins, declined kept without identity, reclaimed
// dropped) cannot diverge between them.
type DHCPLease struct {
	Address       string
	HWAddr        string
	Hostname      string
	SubnetID      int
	Expire        int64
	ValidLifetime int
	Declined      bool
}

// DHCPLeases returns the leases Kea currently holds on this host, plus a hint
// explaining an empty result when the reason is not simply "nobody has leased
// yet". Mode is the caller's to supply because the config lives on their side;
// pass config.DHCPRelay's string to get the relay explanation.
//
// Read-only, like the handler: it opens a file and touches nothing.
func DHCPLeases(mode string) (leases []DHCPLease, hint string, err error) {
	if !dhcpSupported() {
		return nil, "gravinet drives a DHCP server on Linux only", nil
	}
	if mode == "relay" {
		return nil, "this node relays DHCP rather than serving it — leases are held by the upstream server, not here", nil
	}
	ls, err := readKeaLeases(keaLeasePath, time.Now())
	if err != nil {
		return nil, "", err
	}
	if mode == "" && len(ls) == 0 {
		hint = "DHCP is off on this node"
	}
	out := make([]DHCPLease, 0, len(ls))
	for _, l := range ls {
		out = append(out, DHCPLease{
			Address: l.Address, HWAddr: l.HWAddr, Hostname: l.Hostname,
			SubnetID: l.SubnetID, Expire: l.Expire,
			ValidLifetime: l.ValidLifetime, Declined: l.Declined,
		})
	}
	return out, hint, nil
}
