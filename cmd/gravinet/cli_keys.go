package main

import (
	"flag"
	"fmt"
	"strconv"

	"gravinet/internal/config"
	"gravinet/internal/control"
)

// cmdKey manages a network's join/rotation key slots.
//
//	gravinet key list    -net NAME
//	gravinet key show    -net NAME -slot N
//	gravinet key generate -net NAME [-label L] [-notes N]
//	gravinet key set KEY -net NAME -slot N [-label L] [-notes N]
//	gravinet key notes   -net NAME -slot N -notes N
//	gravinet key enable  -net NAME -slot N
//	gravinet key disable -net NAME -slot N
//	gravinet key delete  -net NAME -slot N
func cmdKey(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet key <list|show|generate|set|notes|enable|disable|delete|distribute> [-net NAME] [-slot N] [-label L] [-notes N]")
	}
	if args[0] == "distribute" || args[0] == "dist" {
		cmdKeyDistribute(args[1:])
		return
	}
	sub := args[0]
	netName, rest := extractOpt(args[1:], "net")
	slotStr, rest := extractOpt(rest, "slot")
	label, rest := extractOpt(rest, "label")
	notes, rest := extractOpt(rest, "notes")
	noRestart, rest := hasFlag(rest, "no-restart")
	cfg, path, rest := openCfg(rest)
	n := pickNetwork(cfg, netName)

	slot := func() int {
		if slotStr == "" {
			fatal("specify -slot N (0–%d)", config.KeySlots-1)
		}
		v, err := strconv.Atoi(slotStr)
		if err != nil {
			fatal("bad -slot %q", slotStr)
		}
		return v
	}

	switch sub {
	case "list":
		fmt.Printf("network %s keys (%d slots):\n", n.Name, config.KeySlots)
		for i, k := range n.Keys {
			if k.Key == "" {
				fmt.Printf("  [%d] (empty)\n", i)
				continue
			}
			fmt.Printf("  [%d] %-14s %-8s fp=%s\n", i, k.Label, onOff(k.Enabled), config.KeyFingerprint(k.Key))
			if k.Notes != "" {
				fmt.Printf("       notes: %s\n", k.Notes)
			}
		}
		return
	case "show", "reveal":
		key, err := cfg.KeyReveal(n.Name, slot())
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(key)
		return
	case "generate", "gen":
		s, key, err := cfg.KeyGenerate(n.Name, label)
		if err != nil {
			fatal("%v", err)
		}
		if notes != "" {
			if err := cfg.KeySetNotes(n.Name, s, notes); err != nil {
				fatal("%v", err)
			}
		}
		fmt.Printf("generated key in slot %d on %s (distribute this to joiners):\n%s\n", s, n.Name, key)
	case "set", "import":
		key, _ := splitPositional(rest)
		if key == "" {
			fatal("usage: gravinet key set KEY -net NAME -slot N")
		}
		if err := cfg.KeySet(n.Name, slot(), key, label); err != nil {
			fatal("%v", err)
		}
		if notes != "" {
			if err := cfg.KeySetNotes(n.Name, slot(), notes); err != nil {
				fatal("%v", err)
			}
		}
		fmt.Printf("set key in slot %d on %s\n", slot(), n.Name)
	case "notes":
		if err := cfg.KeySetNotes(n.Name, slot(), notes); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("set notes on key slot %d on %s\n", slot(), n.Name)
	case "enable", "disable":
		if err := cfg.KeySetEnabled(n.Name, slot(), sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%sd key slot %d on %s\n", sub, slot(), n.Name)
	case "delete", "del", "remove":
		if err := cfg.KeyDelete(n.Name, slot()); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("deleted key slot %d on %s\n", slot(), n.Name)
	default:
		fatal("unknown: gravinet key %s", sub)
	}
	commitCfgStructural(cfg, path, noRestart)
}

// cmdKeyDistribute pushes a rotated key to every current member over the live
// mesh, via the running daemon's control socket, so it need not be placed on
// each node by hand. Usage: distribute the new key, let it propagate, then
// retire the old key in config (which forces peers to re-handshake onto a
// still-valid key). Do NOT use this to rotate away a key you believe is
// compromised — the mesh channel is protected by the very key you're replacing,
// so re-key those nodes out of band instead.
func cmdKeyDistribute(args []string) {
	key, rest := splitPositional(args)
	fs := flag.NewFlagSet("key distribute", flag.ExitOnError)
	sock := fs.String("sock", defaultControlSocket(), "control socket path")
	netID := fs.String("net", "", "network name or hex id; optional if only one")
	label := fs.String("label", "", "optional key label")
	expires := fs.String("expires", "", "optional expiry, RFC3339 (e.g. 2026-12-31T00:00:00Z)")
	fs.Parse(rest)
	if key == "" {
		fatal("usage: gravinet key distribute <base64-key> [-net NAME|id] [-label L] [-expires RFC3339] [-sock path]")
	}
	resp, err := control.Do(*sock, control.Request{Cmd: "keydist", Net: *netID, Key: key, Label: *label, Expires: *expires})
	ctlResult(resp, err)
}
