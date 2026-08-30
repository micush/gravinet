package tui

// Renderers for internal/service's three host-state structs. Split out of the
// page builders because each of them has a "what could not be determined"
// axis that is easy to render wrong: TimeInfo carries NTPKnown and SyncKnown
// alongside NTPEnabled and Synchronized, and a reader that ignores the
// *Known flags reports "not synchronized" for a host where synchronization
// simply could not be read. Those are different findings and only one of them
// is a problem.

import (
	"fmt"
	"strconv"

	"gravinet/internal/service"
)

// timeRows renders TimeInfo, honoring its two "known" flags.
func timeRows(t service.TimeInfo) []kvRow {
	tz := dash(t.Timezone)
	if t.Abbrev != "" {
		tz = fmt.Sprintf("%s (%s, UTC%s)", dash(t.Timezone), t.Abbrev, utcOffset(t.OffsetSeconds))
	}
	ntp, ntpTone := "unknown", "mut"
	if t.NTPKnown {
		ntp, ntpTone = onOff(t.NTPEnabled), enabledTone(t.NTPEnabled)
	}
	sync, syncTone := "unknown", "mut"
	if t.SyncKnown {
		sync = yesNo(t.Synchronized)
		syncTone = "ok"
		if !t.Synchronized {
			syncTone = "warn"
		}
	}
	return []kvRow{
		{"clock", t.Now.Format("2006-01-02 15:04:05 MST"), ""},
		{"timezone", tz, ""},
		{"ntp", ntp, ntpTone},
		{"synchronized", sync, syncTone},
		{"servers", joinOr(t.Servers, "\u2014"), ""},
		{"managed by", dash(t.Manager), "mut"},
	}
}

// utcOffset renders a seconds-east-of-UTC offset as +HH:MM.
func utcOffset(secs int) string {
	sign := "+"
	if secs < 0 {
		sign, secs = "-", -secs
	}
	return fmt.Sprintf("%s%02d:%02d", sign, secs/3600, (secs%3600)/60)
}

// syslogTable renders SyslogInfo's targets, or nil when there are none, so
// the caller can substitute an empty state.
func syslogTable(info service.SyslogInfo) item {
	if len(info.Targets) == 0 {
		return nil
	}
	t := table{selectKey: "syslog", head: []string{"collector", "port", "protocol", "state"}}
	for _, tg := range info.Targets {
		row := tableRow{cells: []string{
			tg.Remote, strconv.Itoa(tg.Port), dash(tg.Protocol), onOff(!tg.Disabled),
		}, cellTone: map[int]string{3: enabledTone(!tg.Disabled)}}
		if tg.Disabled {
			row.tone = "dim"
		}
		t.rows = append(t.rows, row)
		t.ids = append(t.ids, tg.Remote)
	}
	return t
}
