package webadmin

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/ddns"
	"gravinet/internal/logx"
	"gravinet/internal/service"
)

// Driving the dynamic DNS registrar from the daemon.
//
// The inputs are the host's live resolver settings rather than anything stored
// here: the hostname it actually has, the servers it actually resolves through,
// the domain it actually searches. That is the same information System >
// Resolver shows, read the same way, so the page and the registration cannot
// describe different nodes. It also means the feature configures itself — fill
// those three in and set an interval, and there is nothing else to say.

// Nothing here records what a run did.
//
// It used to: there was a ddnsState holding the last outcome, and Settings >
// General rendered it. That was a status report on a settings page, and the
// two are different jobs — a settings page says what this node is configured
// to do, and what it actually did belongs in the log with everything else that
// happens on a timer. The loop below logs every outcome already: failures at
// warn with the server's own reason, a change at info naming what moved, and
// the steady state at debug so a working node does not write a line an hour
// forever. Removed in v1000.

// ddnsParams assembles a run's inputs, or explains why there is nothing to do.
//
// The three preconditions are reported as a sentence rather than a bool,
// because "nothing happened" is the hardest state to debug on a feature whose
// whole output is somewhere else.
func ddnsParams(cfg *config.Config) (ddns.Params, error) {
	info := service.HostResolver()
	host := strings.TrimSpace(info.Hostname)
	domain := strings.TrimSpace(info.SearchDomain)
	servers := trimAll(info.DNSServers)

	var missing []string
	if host == "" {
		missing = append(missing, "a hostname")
	}
	if domain == "" {
		missing = append(missing, "a search domain")
	}
	if len(servers) == 0 {
		missing = append(missing, "at least one DNS server")
	}
	if len(missing) > 0 {
		return ddns.Params{}, fmt.Errorf("this node has no %s set under System > Resolver, so there is nothing to register", strings.Join(missing, " and no "))
	}

	key, err := ddns.ParseKey(cfg.DDNS.TSIGKey)
	if err != nil {
		return ddns.Params{}, err
	}
	return ddns.Params{
		Hostname: host,
		Domain:   domain,
		Servers:  servers,
		TTL:      uint32(cfg.DDNS.TTL),
		Key:      key,
		Reverse:  cfg.DDNS.ReverseEnabled(),
	}, nil
}

// RunDDNSOnce performs one registration pass and records the outcome for the
// page and the support bundle.
//
// Exported, with the timer loop below as its only caller today. It is the seam
// an on-demand run would attach to if one is ever wanted — but not from the
// settings page: triggering a registration is an operational action rather
// than a preference, and a button that reaches out to somebody else's DNS
// server does not belong beside a dark-mode switch. There was one there
// through v995.
func RunDDNSOnce(cfg *config.Config) (ddns.Result, error) {
	p, err := ddnsParams(cfg)
	if err != nil {
		return ddns.Result{}, err
	}
	return ddns.Register(p, logx.Debugf)
}

// ddnsJitter spreads runs out so a rack of gateways that all rebooted together
// does not arrive at the DNS server in the same millisecond every interval.
// Proportional rather than fixed, so a five-minute interval is not smeared by
// an hour's worth of jitter.
const ddnsJitter = 0.3

// StartDDNS runs the registrar on a timer until stop is closed.
//
// Reads the config on every tick rather than closing over it, so changing the
// interval or the key on the page takes effect at the next run without a
// restart — and so a node whose resolver settings are filled in an hour after
// boot starts registering without one either.
//
// The first run is immediate rather than one interval away. A node that just
// booted with a new address is exactly when its record is most wrong, and
// waiting an hour to say so would make the feature useless at the only moment
// it is urgently needed.
func StartDDNS(configPath string, stop <-chan struct{}) {
	go func() {
		for {
			cfg, err := config.Load(configPath)
			if err != nil {
				logx.Warnf("ddns: could not read the config: %v", err)
				select {
				case <-stop:
					return
				case <-time.After(time.Minute):
					continue
				}
			}
			if !cfg.DDNS.Active() {
				// Off. Re-checked on a slow timer rather than exiting, so
				// switching it on from the page starts it without a restart.
				select {
				case <-stop:
					return
				case <-time.After(time.Minute):
					continue
				}
			}
			res, err := RunDDNSOnce(cfg)
			switch {
			case err != nil:
				logx.Warnf("ddns: %v", err)
			case len(res.Errors) > 0:
				logx.Warnf("ddns: published %d name(s), %d problem(s): %s",
					len(res.Published), len(res.Errors), strings.Join(res.Errors, "; "))
			case res.Updated > 0:
				logx.Infof("ddns: updated %d of %d name(s): %s",
					res.Updated, len(res.Published), strings.Join(res.Published, ", "))
			default:
				// The steady state, and the common one. Debug so a working
				// node does not write a line every interval forever.
				logx.Debugf("ddns: %d name(s) already correct", len(res.Published))
			}

			select {
			case <-stop:
				return
			case <-time.After(withJitter(cfg.DDNS.Interval())):
			}
		}
	}()
}

// withJitter spreads a period by up to ddnsJitter either side.
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Minute
	}
	spread := float64(d) * ddnsJitter
	// time.Now's nanoseconds are enough of a source here: this is anti-herding,
	// not anything anyone should be predicting or not predicting.
	frac := float64(time.Now().UnixNano()%1000)/1000.0*2 - 1
	out := time.Duration(float64(d) + spread*frac)
	if out < time.Second {
		return time.Second
	}
	return out
}

// handleDDNS reads and changes the dynamic DNS registration settings, and
// reports what the last run did.
//
// The GET carries both the configuration and the live resolver values the run
// depends on, so the page can say "this is off because there is no search
// domain" rather than showing an interval and leaving the operator to work out
// why nothing appears in DNS.
func (s *Server) handleDDNS(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		info := service.HostResolver()
		body := map[string]any{
			"interval_minutes": cfg.DDNS.IntervalMinutes,
			"ttl":              cfg.DDNS.TTL,
			"reverse":          cfg.DDNS.ReverseEnabled(),
			// Whether a key is set, not the key. The page draws dots from
			// this and fetches the value only if somebody clicks them — see
			// the reveal_tsig op below.
			"tsig_configured": strings.TrimSpace(cfg.DDNS.TSIGKey) != "",
			"hostname":        info.Hostname,
			"search_domain":   info.SearchDomain,
			"servers":         info.DNSServers,
		}
		if _, err := ddnsParams(cfg); err != nil {
			body["blocked"] = err.Error()
		}
		writeJSON(w, http.StatusOK, body)
		return
	}

	var req struct {
		Op       string  `json:"op"`
		Interval *int    `json:"interval_minutes"`
		TTL      *int    `json:"ttl"`
		Reverse  *bool   `json:"reverse"`
		TSIGKey  *string `json:"tsig_key"`
	}
	if !decode(w, r, &req) {
		return
	}
	// "reveal_tsig" returns the key as configured — the inline secret, or the
	// path, whichever is in the field. Read-only, and handled before anything
	// below touches config.
	//
	// The settings page masks the key to a row of dots and calls this when the
	// operator clicks them. It is not much of a gate, and is not meant to be:
	// the same session can already read the same secret out of a config
	// snapshot through /api/history/get, which redacts nothing. What it does
	// buy is that the key is not in the page every time somebody opens
	// Settings for an unrelated reason, and that a reveal is a request in the
	// log rather than a side effect of navigation.
	if req.Op == "reveal_tsig" {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		logx.Infof("ddns: TSIG key revealed to the web admin")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tsig_key": cfg.DDNS.TSIGKey})
		return
	}
	err := s.mutateConfig(r, func(cfg *config.Config) error {
		d := &cfg.DDNS
		if req.Interval != nil {
			d.IntervalMinutes = *req.Interval
		}
		if req.TTL != nil {
			d.TTL = *req.TTL
		}
		if req.Reverse != nil {
			v := *req.Reverse
			d.Reverse = &v
		}
		if req.TSIGKey != nil {
			key := strings.TrimSpace(*req.TSIGKey)
			// Parsed before it is stored. A secret that is not base64, or an
			// algorithm nothing implements, is refused while the operator is
			// looking at the field rather than once an interval in a log they
			// are not reading.
			if key != "" {
				if _, perr := ddns.ParseInlineKey(key); perr != nil {
					return perr
				}
			}
			d.TSIGKey = key
		}
		return d.Validate()
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
