package webadmin

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// parseTopIdlePercent parses the idle share off macOS top(1)'s summary line,
// e.g. "CPU usage: 8.23% user, 15.38% sys, 76.38% idle" -> 76.38, true. It
// looks for the token immediately before the literal "idle" rather than a
// fixed column position, matching this package's general approach elsewhere
// (see readNetDev in metrics_darwin.go) of parsing by label rather than
// position, so a minor format change degrades to ok=false instead of a silent
// mis-parse.
//
// This file deliberately has no _darwin.go suffix and no darwin build tag,
// unlike the rest of the macOS metrics readers: the parsing itself has no OS
// dependency, and keeping it free of the platform restriction is what lets it
// be unit-tested here without an actual Mac to run top on. Only
// metrics_darwin.go's topIdlePercent (which does the actual shell-out) calls
// this.
func parseTopIdlePercent(output []byte) (float64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(output))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "CPU usage:") {
			continue
		}
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i+1] != "idle" {
				continue
			}
			v := strings.TrimSuffix(fields[i], "%")
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return 0, false
			}
			return n, true
		}
		return 0, false
	}
	return 0, false
}
