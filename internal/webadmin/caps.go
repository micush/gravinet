package webadmin

import "gravinet/internal/service"

// Caps is what this host can actually back, as opposed to what it is
// configured to do. Six of the web admin's pages are gated on one of these:
// without FRR there is nothing for the BGP editor to write to, without
// snmpd/lldpd/a syslog daemon those pages are forms with no agent behind
// them, radvd is Linux-and-installed, and the DHCP relay needs the arrival
// interface of a broadcast in a way only Linux offers here.
//
// The web admin sends this to the browser as the *_supported flags in
// /api/config, where ui.go's sectionVisible() hides the rail entries; the
// TUI reads it directly for the same purpose (internal/tui's own
// sectionVisible). One reader, so the rail can't show a page in one client
// that the other correctly hides.
type Caps struct {
	BGP    bool // FRR's vtysh is installed
	IPv6RA bool // Linux, and radvd is installed
	DHCP   bool // Linux (the relay needs the arrival interface of a broadcast)
	SNMP   bool // snmpd is present
	LLDP   bool // lldpd is present
	Syslog bool // a syslog daemon this package can drive is present
}

// Capabilities probes the host. Each field delegates to the same function
// that already decided it — bgpSupported/ipv6RASupported/dhcpSupported here,
// service.SNMPSupported/LLDPSupported/SyslogSupported next door — so this is
// a composition of existing answers, not a second opinion on any of them.
//
// Every probe is a filesystem or PATH lookup, so this is cheap enough to call
// per request (handleConfig does) and per refresh (the TUI does). It is not
// cached: a host where lldpd was installed five minutes ago should show the
// page five minutes ago, not after a restart.
func Capabilities() Caps {
	snmp, _ := service.SNMPSupported()
	lldp, _ := service.LLDPSupported()
	syslog, _ := service.SyslogSupported()
	return Caps{
		BGP:    bgpSupported(),
		IPv6RA: ipv6RASupported(),
		DHCP:   dhcpSupported(),
		SNMP:   snmp,
		LLDP:   lldp,
		Syslog: syslog,
	}
}
