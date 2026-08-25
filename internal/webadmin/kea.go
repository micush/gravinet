package webadmin

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"gravinet/internal/config"
)

// Kea is the DHCP server gravinet drives, the same shape as the radvd and FRR
// integrations next door: gravinet owns the config file, regenerates it whole
// from its own config on every apply, and never edits in place.
//
// One difference is worth the note. Kea's config is JSON, so this renders
// through encoding/json rather than through a strings.Builder. That removes
// the entire class of bug the text renderers have to defend against by hand —
// radvd.go carries a safeToken check on every operator-supplied string because
// a stray brace or semicolon in a search domain would otherwise break out of
// the line it was written on. Here the marshaller escapes, and there is
// nothing to escape wrong.
//
// The marker that says a file is gravinet's to rewrite therefore has to live
// inside the JSON rather than on a comment line. Kea permits comments in its
// config, but a comment is not something json.Marshal will emit, and a file
// whose first line had to be written outside the marshaller would be a file
// this code could no longer round-trip.
//
// Where inside the JSON is not a free choice, and v944 got it wrong. Kea's
// grammar accepts exactly one key at the top level, Dhcp4, and rejects any
// other outright rather than ignoring it:
//
//\tsyntax error, unexpected constant string, expecting Dhcp4
//
// which is a server that will not start at all. The marker goes in Dhcp4's
// global user-context instead \u2014 the mechanism Kea documents for attaching
// data of one's own to a scope, defined as ignored by the server.

const keaConfPath = "/etc/kea/kea-dhcp4.conf"

// keaMarker is the key gravinet writes to claim the file. It lives in Dhcp4's
// user-context, which is Kea's own mechanism for carrying data the server does
// not interpret; a key beside Dhcp4 is a parse error, not an ignored field.
const keaMarker = "gravinet-generated"

// keaConf is the top-level config document. Exactly one field, because Kea's
// grammar permits exactly one key here.
type keaConf struct {
	Dhcp4 keaDhcp4 `json:"Dhcp4"`
}

// keaUserContext is where the ownership marker lives.
type keaUserContext struct {
	Marker bool `json:"gravinet-generated"`
}

type keaDhcp4 struct {
	UserContext      keaUserContext `json:"user-context"`
	InterfacesConfig keaIfaces      `json:"interfaces-config"`
	LeaseDatabase    keaLeaseDB     `json:"lease-database"`
	ValidLifetime    int            `json:"valid-lifetime"`
	Subnet4          []keaSubnet    `json:"subnet4"`
	Loggers          []keaLogger    `json:"loggers,omitempty"`
}

type keaIfaces struct {
	Interfaces []string `json:"interfaces"`
}

type keaLeaseDB struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type keaSubnet struct {
	ID         int         `json:"id"`
	Subnet     string      `json:"subnet"`
	Interface  string      `json:"interface"`
	Pools      []keaPool   `json:"pools"`
	OptionData []keaOption `json:"option-data,omitempty"`
	ValidLife  int         `json:"valid-lifetime,omitempty"`
}

type keaPool struct {
	Pool string `json:"pool"`
}

type keaOption struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

type keaLogger struct {
	Name       string             `json:"name"`
	Severity   string             `json:"severity"`
	OutputOpts []keaOutputOptions `json:"output_options"`
}

type keaOutputOptions struct {
	Output string `json:"output"`
}

// defaultLease is what a subnet gets when the operator named no lease time.
// Kea has its own default, but it is a property of the version installed
// rather than of this configuration, and a lease time that changes when a
// package is upgraded is not something to leave implicit.
const defaultLease = 3600

// renderKea renders /etc/kea/kea-dhcp4.conf. Pure: no I/O, so it is testable
// without a daemon or a host.
//
// A subnet that resolves to nothing is skipped rather than emitted empty, the
// same rule renderRadvd follows and for the same reason: Kea refuses to start
// on a malformed scope, and one bad subnet taking down the leases for every
// other LAN is a far worse failure than that one LAN going unserved.
func renderKea(c config.DHCPConfig) ([]byte, error) {
	conf := keaConf{
		Dhcp4: keaDhcp4{
			UserContext: keaUserContext{Marker: true},
			// Kea listens only on the interfaces it is told about. Naming
			// them explicitly, rather than using its "*" wildcard, is what
			// keeps a DHCP server off every other link on the host — most
			// importantly off the mesh devices, where answering would hand
			// overlay peers a lease.
			InterfacesConfig: keaIfaces{Interfaces: []string{}},
			LeaseDatabase:    keaLeaseDB{Type: "memfile", Name: "/var/lib/kea/kea-leases4.csv"},
			ValidLifetime:    defaultLease,
			Subnet4:          []keaSubnet{},
			Loggers: []keaLogger{{
				Name:       "kea-dhcp4",
				Severity:   "INFO",
				OutputOpts: []keaOutputOptions{{Output: "syslog"}},
			}},
		},
	}
	// Subnet ids are assigned by position and are stable for a given config,
	// which is what keeps leases attached to the scope they were issued from
	// across an apply. They are 1-based because Kea reserves 0.
	id := 0
	for _, s := range c.EnabledSubnets() {
		if err := s.Validate(); err != nil {
			// Already checked on save, so reaching here means a config edited
			// by hand. Refusing the whole render is right: a partial file
			// would silently drop a scope an operator believes is served.
			return nil, fmt.Errorf("subnet on %s: %w", s.Iface, err)
		}
		id++
		net := netip.MustParsePrefix(strings.TrimSpace(s.Subnet)).Masked()
		sub := keaSubnet{
			ID:        id,
			Subnet:    net.String(),
			Interface: strings.TrimSpace(s.Iface),
			Pools:     []keaPool{{Pool: strings.TrimSpace(s.PoolStart) + " - " + strings.TrimSpace(s.PoolEnd)}},
			ValidLife: s.LeaseSeconds,
		}
		if r := strings.TrimSpace(s.Router); r != "" {
			sub.OptionData = append(sub.OptionData, keaOption{Name: "routers", Data: r})
		}
		if dns := trimAll(s.DNS); len(dns) > 0 {
			sub.OptionData = append(sub.OptionData, keaOption{Name: "domain-name-servers", Data: strings.Join(dns, ", ")})
		}
		if search := trimAll(s.Search); len(search) > 0 {
			// Option 119. Kea takes a comma-separated list and encodes it;
			// the operator's strings go through the marshaller untouched.
			sub.OptionData = append(sub.OptionData, keaOption{Name: "domain-search", Data: strings.Join(search, ", ")})
		}
		conf.Dhcp4.Subnet4 = append(conf.Dhcp4.Subnet4, sub)
		conf.Dhcp4.InterfacesConfig.Interfaces = append(conf.Dhcp4.InterfacesConfig.Interfaces, sub.Interface)
	}
	b, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// keaOwned reports whether the file at path is gravinet's to rewrite: either
// absent, or carrying the marker key. An unreadable or unparseable file is
// treated as not ours, which fails safe — the same rule radvdOwned follows,
// and for the same reason. A host may already be serving DHCP from a
// hand-maintained config, and silently replacing it would take a working LAN
// down the moment somebody enabled this to try it.
func keaOwned(path string) bool {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	var probe struct {
		Dhcp4 struct {
			UserContext map[string]json.RawMessage `json:"user-context"`
		} `json:"Dhcp4"`
		Legacy json.RawMessage `json:"gravinet-generated"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return false
	}
	// The v944 location, beside Dhcp4 rather than inside it. Still recognised
	// on read, because a node upgrading from v944 has one of these on disk and
	// the alternative is gravinet setting aside a file it wrote itself and
	// telling the operator it preserved their configuration.
	if marked(probe.Legacy) {
		return true
	}
	return marked(probe.Dhcp4.UserContext[keaMarker])
}

// marked reports whether a raw JSON value is the boolean true.
func marked(raw json.RawMessage) bool {
	var b bool
	return len(raw) > 0 && json.Unmarshal(raw, &b) == nil && b
}

// setAsideKeaConf renames an existing config out of the way and returns where
// it went, so a takeover destroys nothing an operator wrote. Same shape as
// setAsideRadvdConf.
func setAsideKeaConf(path string) (string, error) {
	to := path + ".pre-gravinet"
	if _, err := os.Stat(to); err == nil {
		to = fmt.Sprintf("%s.pre-gravinet.%d", path, time.Now().Unix())
	}
	if err := os.Rename(path, to); err != nil {
		return "", err
	}
	return to, nil
}

// keaInstalled reports whether the Kea DHCPv4 server is present.
func keaInstalled() bool {
	for _, bin := range []string{"kea-dhcp4"} {
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	// Kea installs to sbin, which is not always on a non-login root PATH —
	// the same trap radvd and lldpd have.
	for _, p := range []string{"/usr/sbin/kea-dhcp4", "/usr/local/sbin/kea-dhcp4"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// installKea installs Kea through pkgman, the package-manager wrapper gravinet
// already ships beside its own binary, so apt/dnf/pacman are handled without a
// per-distro branch here.
//
// Only ever called from an explicit apply. Enabling the server is the operator
// asking to serve DHCP, and refusing to work until they install a package by
// hand is a worse answer than doing it — the argument v904 made for radvd.
// Never called from a GET or a background reconciler: installing a package is
// not something that should happen as a side effect of opening a page.
func installKea() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("gravinet drives Kea on Linux only")
	}
	bin, err := exec.LookPath("pkgman")
	if err != nil {
		return fmt.Errorf("pkgman not found; install the Kea DHCPv4 server with your package manager")
	}
	// The package is named differently on every distro family, which is why
	// this cannot go through a single pkg_install the way radvd does: Debian
	// splits the server into its own package, while Fedora and Arch ship one.
	var last string
	for _, name := range []string{"kea-dhcp4-server", "kea"} {
		out, err := exec.Command(bin, "install", name).CombinedOutput()
		if err == nil && keaInstalled() {
			return nil
		}
		last = strings.TrimSpace(lastLine(string(out)))
	}
	return fmt.Errorf("installing Kea: %s", last)
}

// dhcpSupported gates the System > DHCP nav item, the way ipv6RASupported
// gates Traffic > IPv6 RA.
//
// Deliberately *not* gated on Kea being installed, which is the difference
// from the RA page. Half of this page is the relay, which gravinet implements
// itself and which needs no package at all — so gating the whole section on a
// daemon would hide a working feature because an unrelated one was missing.
// The server half reports the absent daemon itself, and installs it on apply.
func dhcpSupported() bool { return runtime.GOOS == "linux" }
