package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"gravinet/internal/config"
	"gravinet/internal/control"
)

// cmdKey manages a network's join/rotation key slots.
//
//	gravinet key list                 [-net NAME]
//	gravinet key show     SLOT        [-net NAME]
//	gravinet key generate             [-net NAME] [-label L] [-notes N]
//	gravinet key set      SLOT KEY    [-net NAME] [-label L] [-notes N]
//	gravinet key notes    SLOT [TEXT...]  [-net NAME]
//	gravinet key enable   SLOT        [-net NAME]
//	gravinet key disable  SLOT        [-net NAME]
//	gravinet key delete   SLOT        [-net NAME]
//
// The slot is the thing every one of those verbs acts on, and it used to be
// mandatory *and* spelled as a flag: "gravinet key delete -net corp -slot 0".
// Nothing else in this CLI does that — nat, fw exempt and route all take the
// index or CIDR they operate on as an argument — so the slot is positional
// now, and -slot is accepted silently for anything already scripted against
// it. Note the argument order on `set`: slot first, like every other verb
// here, then the key, matching "host add NAME IP"'s object-then-value shape.
// The old "key set KEY -slot N" spelling still works, since -slot being
// present is exactly what marks it.
//
// -net stays a flag: it is genuinely optional (a single-network node never
// needs it), and it names the context rather than the object being acted on.
// keySlotArg resolves which key slot a verb was pointed at, and returns the
// arguments left over once it has been taken out of them.
//
// The slot comes from the first positional argument, or from -slot when that
// is how it was written. -slot winning is what keeps the old "key set KEY
// -slot N" spelling working: its single positional is the key, not the slot,
// and the presence of -slot is exactly the marker saying so. Under the new
// spelling, "key set 2 KEY", the slot is consumed here and the key is the one
// argument that remains — so both forms reach the same switch arm with the
// same two values, and neither needs to know about the other.
//
// Nothing is validated here. Resolution stays deferred to the caller's slot()
// closure because list and generate take no slot at all, and must not fail for
// want of one they were never going to use.
func keySlotArg(slotFlag string, args []string) (slot string, rest []string) {
	if slotFlag != "" {
		return slotFlag, args
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func cmdKey(args []string) {
	if len(args) == 0 {
		fatal("usage: gravinet key <list|show|generate|set|notes|enable|disable|delete|distribute> [SLOT] [-net NAME]")
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

	slotStr, rest = keySlotArg(slotStr, rest)
	slot := func() int {
		if slotStr == "" {
			fatal("which slot? give it as an argument: gravinet key %s N   (0–%d)", sub, config.KeySlots-1)
		}
		v, err := strconv.Atoi(slotStr)
		if err != nil {
			fatal("slot must be a number 0–%d, got %q", config.KeySlots-1, slotStr)
		}
		return v
	}

	sub = expandVerb(sub, v("list"), v("show", "reveal"), v("generate", "gen"), v("set", "import"), v("notes"), v("enable", "disable"), v("delete", "del", "remove"))
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
			fatal("usage: gravinet key set SLOT KEY   (slot 0–%d)", config.KeySlots-1)
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
		// Trailing words are the note, the way "gravinet network notes NAME
		// TEXT..." already reads; -notes still wins if it was given, and an
		// empty tail clears the note, as it does there.
		text := notes
		if len(rest) > 0 {
			text = strings.Join(rest, " ")
		}
		if err := cfg.KeySetNotes(n.Name, slot(), text); err != nil {
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
