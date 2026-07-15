# Security fixes (v185)

All five findings from the audit are fixed, with tests. Summary of what changed
and — for the highest-severity one — the design decision behind the final shape.

## 1. Inbound overlay source-address spoofing (was HIGH)

`deliverInner` now verifies the inner packet's source before writing it to the
TUN, via `sourceAllowedFrom` (internal/mesh/engine.go). Dropped packets are
counted per peer as `spoofDrop` (exposed in PeerInfo / the peers API).

**Design decision — a narrow, identity-focused rule.** A first attempt required
the source to be the peer's own overlay address *or* inside a prefix it
advertises, with strict per-address allow-listing. That broke two legitimate,
tested behaviours: NAT masquerade (a peer emits packets from an arbitrary
`Translate` address that is neither its overlay address nor an advertised
prefix) and gatewaying. There is no way for the receiver to know a *sending*
peer's NAT config, so strict allow-listing would have forced every masquerade
address to be pre-advertised — a real functional regression for no proportional
gain.

The final rule blocks precisely the thing that actually matters and can't be
otherwise defended against: **a peer may not source a packet from an overlay
address another peer currently owns** (identity impersonation). A peer's own
overlay address is always allowed; an advertised-gateway prefix is allowed; and
any source no other peer claims (NAT translate addresses, gatewayed hosts) is
allowed. This makes impersonating another node's mesh identity impossible while
leaving NAT/masquerade/forwarding working. Unparseable headers fail closed.

Tests (internal/mesh/antispoof_test.go): own-source allowed; another peer's
overlay identity dropped + counted; advertised-prefix source allowed; unclaimed
translate/masquerade source allowed; unparseable dropped.

## 2. HS_INIT replay within the timestamp window (was MEDIUM)

The ±120 s timestamp window is now backed by a replay cache keyed on the
initiator's ephemeral X25519 public key (a natural single-use nonce), checked
only after authentication so it never fills with attacker-chosen data. Bounded
at `maxHSSeen` entries with lapsed/oldest eviction; entries expire after one
skew window, the same horizon `freshTimestamp` would otherwise still accept a
replay within. (internal/mesh/handshake_engine.go)

Tests: duplicate ephemeral rejected; distinct ephemerals both accepted; an
entry older than the window no longer counts as a replay; cache stays bounded
under a unique-ephemeral flood.

## 3. Management-proxy path-traversal (was LOW)

`handleProxy` (internal/webadmin/cluster.go) now rejects any `path` containing
`..` or its percent-encoded forms (`%2e`, `%2f`, `%5c`), and — belt and
suspenders — re-verifies the parsed `req.URL.Path` still begins with `/api/`
after net/url normalization, not just the raw string.

Test (internal/webadmin/cluster_test.go): a battery of traversal encodings all
return 403.

## 4. Config atomic-write race / predictable temp name (was LOW)

`Config.SaveTo` (internal/config/config.go) now writes to a uniquely-named
`os.CreateTemp` file in the target directory (created `0600`), fsyncs, then
renames, cleaning up the temp on any error — instead of a fixed `.tmp` name that
two concurrent saves could clobber and that a predictable name invited a symlink
race on.

Tests (internal/config/config_test.go): correct `0600` perms and no leftover
temp; 16 concurrent saves leave the file loadable with no stray temp files.

## 5. Session-signing-secret fallback was guessable (was LOW)

`signingSecret` (internal/webadmin/webadmin.go) no longer falls back to a
time-derived key if `crypto/rand` fails; it fails closed (logs and panics)
rather than sign session cookies with a predictable key. crypto/rand failing on
a functioning OS is near-impossible; if it happens, refusing to serve
authenticated sessions is the safe outcome.

---

All changes: `go build`, `go vet`, and `go test ./...` pass on linux; the binary
cross-compiles for freebsd/linux/darwin/windows; and `go test -race` is clean on
the changed mesh hot paths.
