package main

import (
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/service"
)

// reconcileHostSettings applies the host configuration gravinet has been told
// to own — syslog forwarding, timezone and NTP, hostname and DNS — at startup
// and on every reload.
//
// This is what makes a restored backup bring them back. The restore writes the
// configuration and reloads; the reload gets here. Without it the config would
// describe settings nothing ever applied, which is the same silent-no-op the
// interface reconciler was written to avoid.
//
// Every group is separately opt-in and an unset one is skipped entirely. A
// host whose DNS comes from DHCP, or whose clock is managed by something else,
// is left alone unless an operator has actually changed that setting through
// gravinet.
//
// Failures are logged and stepped over rather than aborting the reload: a
// reload carries far more than this, and a host setting that will not apply —
// a timezone the OS does not have, a syslog daemon that is not installed —
// must not stop a mesh from coming up.
func reconcileHostSettings(cfg *config.Config) {
	if cfg.HostSettings == nil {
		return // gravinet manages none of this on this host
	}
	h := *cfg.HostSettings

	if h.Syslog != nil {
		targets := make([]service.SyslogTarget, 0, len(h.Syslog.Targets))
		for _, t := range h.Syslog.Targets {
			targets = append(targets, service.SyslogTarget{
				Remote: t.Host, Port: t.Port, Protocol: t.Proto, Disabled: t.Disabled,
			})
		}
		if ok, note := service.SetHostSyslog(targets); !ok {
			logx.Errorf("host syslog: %s", note)
		}
	}

	if t := h.Time; t != nil {
		// The clock itself is never restored — only how the host keeps it.
		// Setting the system time from a backup would move it to whenever
		// the backup was taken, which is worse than any problem it solves.
		if t.Timezone != "" {
			if ok, note := service.SetHostTimezone(t.Timezone); !ok {
				logx.Errorf("host timezone %s: %s", t.Timezone, note)
			}
		}
		// NTP is applied whenever gravinet manages time at all, because
		// "off" is a setting an operator can mean and skipping it when the
		// list is empty would make disabling NTP unrestorable.
		if ok, note := service.SetHostNTP(t.NTPEnabled, t.NTPServers); !ok {
			logx.Errorf("host NTP: %s", note)
		}
	}

	// Console accounts are recreated but never deleted: an account on this
	// host that the configuration does not mention is somebody else's, and
	// removing it because a backup omitted it is a purge, not a restore.
	//
	// They come back with no password — locked — so a restored node has its
	// accounts named and present, and someone sets a password once from the
	// console. That is the deliberate limit of storing no credentials.
	for _, u := range h.Users {
		var exp time.Time
		if u.ExpiresUnix > 0 {
			exp = time.Unix(u.ExpiresUnix, 0)
		}
		if ok, note := service.EnsureSystemUser(u.Name, exp); !ok {
			logx.Errorf("console account %s: %s", u.Name, note)
		}
	}

	if rz := h.Resolver; rz != nil {
		if rz.Hostname != "" {
			if ok, note := service.SetHostname(rz.Hostname); !ok {
				logx.Errorf("host hostname %s: %s", rz.Hostname, note)
			}
		}
		// Unlike NTP, empty DNS is not applied: an empty resolver list is
		// far more likely to mean "gravinet only ever set the hostname" than
		// "remove every nameserver", and guessing wrong takes name
		// resolution off the host.
		if len(rz.DNSServers) > 0 {
			if ok, note := service.SetHostDNS(rz.DNSServers, rz.SearchDomain); !ok {
				logx.Errorf("host DNS: %s", note)
			}
		}
	}
}
