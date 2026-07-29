package webadmin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Troubleshooting bundle: one download containing everything needed to
// diagnose this node without a back-and-forth.
//
// The motivating experience was a day spent reconstructing a mesh's state one
// screenshot at a time — peer tables cropped above the row that mattered,
// routes from one node but not its peer, drop counters never checked because
// nobody knew they existed. Every question asked in that session is answered
// here in a single file, from every network, with the fields that actually
// discriminate rather than the ones that fit in a table column.
//
// Two rules govern what goes in:
//
//   - Structured sections are emitted as indented JSON of the source structs
//     rather than hand-formatted. Hand-formatting silently omits fields added
//     later, which is exactly how a diagnostic goes stale; marshalling the
//     struct means a new field appears in the bundle the day it is added.
//   - Secrets never appear. The config file is included because misconfiguration
//     is a leading cause of the problems this exists to diagnose, but every
//     key-bearing line is redacted on the way out, and the redaction is
//     allow-list-free: anything whose key name looks secret is replaced, so a
//     newly added secret field is redacted by default rather than leaked by
//     default.
const tshootMaxLogBytes = 4 << 20 // tail of the log; enough for hours of context

// secretish matches configuration keys whose values must never leave the node.
// Deliberately broad: a false positive costs a redacted line in a diagnostic,
// a false negative leaks a private key.
var secretish = regexp.MustCompile(`(?i)(key|secret|password|passwd|token|credential|psk|private|seed_?phrase|hash|salt)`)

func (s *Server) handleTshoot(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	now := time.Now()

	sec := func(title string) {
		fmt.Fprintf(&b, "\n\n========== %s ==========\n", title)
	}
	dump := func(label string, v any) {
		fmt.Fprintf(&b, "\n--- %s ---\n", label)
		enc, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintf(&b, "(could not encode: %v)\n", err)
			return
		}
		b.Write(enc)
		b.WriteByte('\n')
	}

	fmt.Fprintf(&b, "gravinet troubleshooting bundle\n")
	fmt.Fprintf(&b, "generated:  %s (%s)\n", now.Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "version:    %s (commit %s)\n", s.version, s.commit)
	fmt.Fprintf(&b, "go:         %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if h, err := os.Hostname(); err == nil {
		fmt.Fprintf(&b, "hostname:   %s\n", h)
	}
	fmt.Fprintf(&b, "\nCollected from ONE node. Peers hold their own view and the two can\n")
	fmt.Fprintf(&b, "legitimately disagree — a session torn down on one side is not always\n")
	fmt.Fprintf(&b, "torn down on the other. For any problem involving a specific peer,\n")
	fmt.Fprintf(&b, "collect this from BOTH ends: the disagreement is often the diagnosis.\n")

	sec("NODE")
	nat4, nat6 := s.be.NATStatusStrings()
	fmt.Fprintf(&b, "NAT (v4): %s\nNAT (v6): %s\n", nat4, nat6)
	dump("interfaces", s.be.Interfaces())

	sec("NETWORKS")
	ids := s.be.NetworkIDs()
	fmt.Fprintf(&b, "networks: %d\n", len(ids))
	for _, id := range ids {
		fmt.Fprintf(&b, "\n\n---------- network %x ----------\n", id)

		// Peers first and in full. Every field matters somewhere: reach and
		// relay identify the path, time distinguishes a stable session from
		// one being rebuilt on a loop, path_mtu and the fragment counters
		// catch amplification, and the drop counters name a cause instead of
		// leaving a lost packet anonymous.
		peers := s.be.ListPeers(id)
		fmt.Fprintf(&b, "peers: %d\n", len(peers))
		dump("peers", peers)

		dump("routes", s.be.Routes(id))
		dump("bans", s.be.ListBans(id))
		dump("disabled peers", s.be.DisabledPeers(id))

		if rules, err := s.be.FirewallRules(id); err != nil {
			fmt.Fprintf(&b, "\n--- firewall rules ---\n(unavailable: %v)\n", err)
		} else {
			dump("firewall rules", rules)
		}
		dump("firewall exemptions", s.be.FirewallExemptsFor(id))
	}

	sec("CONFIG (secrets redacted)")
	if s.configPath == "" {
		b.WriteString("(config view not enabled on this node)\n")
	} else if raw, err := os.ReadFile(s.configPath); err != nil {
		fmt.Fprintf(&b, "(could not read %s: %v)\n", s.configPath, err)
	} else {
		fmt.Fprintf(&b, "path: %s\n\n", s.configPath)
		b.WriteString(redactConfig(string(raw)))
	}

	sec("LOG (tail)")
	if s.logPath == "" {
		b.WriteString("(file logging disabled; set \"log_file\" in the config to enable it)\n")
	} else {
		fmt.Fprintf(&b, "path: %s\n", s.logPath)
		tail, err := tailFile(s.logPath, tshootMaxLogBytes)
		if err != nil {
			fmt.Fprintf(&b, "(could not read: %v)\n", err)
		} else {
			fmt.Fprintf(&b, "(last %d bytes)\n\n", len(tail))
			b.Write(tail)
		}
	}

	b.WriteString("\n\n========== END ==========\n")

	name := fmt.Sprintf("gravinet-tshoot-%s.txt", now.Format("20060102-150405"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// redactConfig blanks the value of any line whose key looks secret, for both
// "key: value" and "key = value" shapes, leaving structure and comments intact
// so the file is still readable as configuration.
func redactConfig(src string) string {
	lines := strings.Split(src, "\n")
	secretIndent := -1 // indent of an open secret-looking block, or -1

	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		indent := len(ln) - len(strings.TrimLeft(ln, " \t"))

		// Anything nested under a secret-looking key is itself secret — a list
		// of keys has no per-item key name to match on, so the block has to be
		// tracked rather than each line judged alone.
		if secretIndent >= 0 {
			if indent > secretIndent || strings.HasPrefix(trimmed, "-") {
				lines[i] = redactKeepingIndent(ln, "<redacted>")
				continue
			}
			secretIndent = -1
		}

		sepIdx := -1
		for _, sep := range []string{":", "="} {
			if j := strings.Index(ln, sep); j >= 0 && (sepIdx < 0 || j < sepIdx) {
				sepIdx = j
			}
		}
		if sepIdx < 0 {
			if len(trimmed) > 24 && !strings.ContainsAny(trimmed, " \t") {
				lines[i] = redactKeepingIndent(ln, "<redacted>")
			}
			continue
		}
		if !secretish.MatchString(ln[:sepIdx]) {
			continue
		}
		if strings.TrimSpace(ln[sepIdx+1:]) == "" {
			// "keys:" with the values on following lines.
			secretIndent = indent
			continue
		}
		lines[i] = ln[:sepIdx+1] + " <redacted>"
	}
	return strings.Join(lines, "\n")
}

func redactKeepingIndent(ln, with string) string {
	i := 0
	for i < len(ln) && (ln[i] == ' ' || ln[i] == '\t' || ln[i] == '-') {
		i++
	}
	return ln[:i] + with
}

// tailFile returns the last max bytes of path, starting at a line boundary so
// the bundle never opens mid-record.
func tailFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if st.Size() > max {
		start = st.Size() - max
	}
	if _, err := f.Seek(start, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, st.Size()-start)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, err
	}
	buf = buf[:n]
	if start > 0 {
		if j := strings.IndexByte(string(buf), '\n'); j >= 0 && j+1 < len(buf) {
			buf = buf[j+1:]
		}
	}
	return buf, nil
}
