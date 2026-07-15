package mesh

import (
	"encoding/base64"
	"errors"
	"time"

	"gravinet/internal/crypto"
)

// Rotated/distributed-key propagation, and retraction.
//
// Rotating the network key manually means placing the new key on every node out
// of band. Because every current member already holds the current key and every
// control message rides inside the encrypted session (see sendControl), a node
// can instead flood a new key over the existing mesh: the channel is already
// authenticated and confidential to members, so distributing the next key this
// way does not lower the trust bar — anyone able to forge the message already
// holds a current key and is already a full member. Each recipient folds the key
// into its live set immediately (so handshakes can use it at once) and re-floods
// it once, giving epidemic propagation with KeyID-based de-duplication.
//
// A key already known to a recipient (by identity) but arriving with a changed
// label is treated as a relabel, not a no-op: the label is updated and the
// message re-flooded once, so an admin renaming a distributed key's label sees
// that name follow it to every peer that has a copy.
//
// Retraction (KeyDel) is the inverse of distribution: "stop trusting this key
// mesh-wide". Any node currently holding the key — origin or recipient alike —
// can retract it, symmetric with the fact that any of them could already forge
// authentication with it; there is no separate notion of "the" origin once a
// key has spread. A retraction is honored (and re-flooded) by a node that
// currently has the key, and ignored (flood stops) by one that doesn't — the
// same termination rule ctrlBanDel/ctrlRouteDel already use. Removing the key
// still goes through config.KeyDelete, which refuses to remove a network's only
// enabled key, so a retraction can never brick this node's own connectivity.
//
// This is deliberately additive on the distribute side: it never removes a key
// by itself. The rotation sequence is "distribute the new key, let it reach the
// mesh, then retire the old one" — retirement stays an explicit,
// operator-coordinated config change (which the reload path already turns into
// a re-handshake onto a still-valid key via dropRetiredKeySessions), or an
// explicit retraction. Keeping incident response manual is intentional: the one
// case propagation must NOT be used for is *incident* rotation. If you are
// rotating because the current key may be exposed, do not hand the replacement
// to the mesh over a channel that key protects — re-key those nodes out of band.
// Propagation is for scheduled hygiene rotation, not compromise recovery.

// propagatedKey is a mesh-learned key plus the metadata needed to persist it.
type propagatedKey struct {
	raw     []byte
	label   string
	expires int64 // unix nanos; 0 = never
	// slotHint is the slot this key occupies on whichever node most recently
	// (re)distributed it, so a recipient with that slot free uses the same
	// one; -1 means no preference (e.g. the plain rotation-DistributeKey path,
	// which has no natural originating slot to hint).
	slotHint int
}

// noSlotHint marks "no preference" in the wire encoding (a real hint is 0-7).
const noSlotHint = 0xFF

func encodeKeyAdd(raw []byte, label string, expiresNano int64, slotHint int) []byte {
	out := []byte{ctrlKeyAdd}
	out = appendLenStr(out, string(raw)) // raw 32-byte key; the session AEAD protects it in transit
	out = appendLenStr(out, label)
	var exp [8]byte
	putUint64(exp[:], uint64(expiresNano))
	out = append(out, exp[:]...)
	sh := byte(noSlotHint)
	if slotHint >= 0 && slotHint < 8 {
		sh = byte(slotHint)
	}
	return append(out, sh)
}

// decodeKeyAdd decodes an add/relabel message. The trailing slot-hint byte is
// optional on read (older peers never sent it): its absence decodes to no
// hint (-1), not an error, so this stays wire-compatible both ways — an old
// peer ignores the extra trailing byte a new one sends (it never checks for
// leftover bytes), and a new peer tolerates an old peer's message not having it.
func decodeKeyAdd(b []byte) (raw []byte, label string, expiresNano int64, slotHint int, ok bool) {
	r := reader{b: b}
	ks, ok := r.lenStr()
	if !ok {
		return nil, "", 0, -1, false
	}
	label, ok = r.lenStr()
	if !ok {
		return nil, "", 0, -1, false
	}
	exp, ok := r.u64()
	if !ok {
		return nil, "", 0, -1, false
	}
	slotHint = -1
	if sh, ok2 := r.byte(); ok2 && sh < 8 {
		slotHint = int(sh)
	}
	return []byte(ks), label, int64(exp), slotHint, true
}

func encodeKeyDel(id crypto.KeyID) []byte {
	out := make([]byte, 0, 1+len(id))
	out = append(out, ctrlKeyDel)
	return append(out, id[:]...)
}

func decodeKeyDel(b []byte) (crypto.KeyID, bool) {
	var id crypto.KeyID
	if len(b) < len(id) {
		return id, false
	}
	copy(id[:], b[:len(id)])
	return id, true
}

// keyAddOutcome distinguishes why addPropagatedKey did or didn't ask its
// caller to re-flood, so callers other than onKeyAdd (namely DistributeKey and
// FloodKey, which always want to flood regardless) don't need to care, while
// onKeyAdd's epidemic termination does.
type keyAddOutcome int

const (
	keyAddUnchanged  keyAddOutcome = iota // already known, nothing changed — stop the flood
	keyAddNew                             // new to this node — flood onward
	keyAddMetaChanged                     // already known, but label and/or expiry changed — flood onward
)

// addPropagatedKey folds raw into the live key set (if not already present)
// and records/updates its metadata for persistence, reporting new/changed/
// unchanged so the caller knows whether to keep flooding. allowRelabel governs
// what happens when the key is already known: FloodKey (used for
// distributing, relabeling, and re-pushing an expiry change from the GUI)
// passes true, so a changed label or expiry is adopted; DistributeKey passes
// false, since its whole contract is "this must be a genuinely new key" — a
// repeat call with different metadata is a metadata update, not a rotation,
// and belongs to FloodKey instead. Done as a single atomic check-and-update
// under ns.mu rather than a separate pre-check, so there's no race between two
// concurrent calls for the same key.
func (e *Engine) addPropagatedKey(ns *netState, raw []byte, label string, expiresNano int64, slotHint int, allowRelabel bool) keyAddOutcome {
	if len(raw) != crypto.KeySize {
		return keyAddUnchanged
	}
	id := crypto.DeriveKeyID(raw)
	ns.mu.Lock()
	defer ns.mu.Unlock()
	cur := ns.keys.Load()
	if cur != nil {
		if _, dup := cur.Lookup(id); dup {
			if !allowRelabel {
				return keyAddUnchanged
			}
			if ns.propagatedKeys == nil {
				ns.propagatedKeys = make(map[crypto.KeyID]propagatedKey)
			}
			existing, tracked := ns.propagatedKeys[id]
			// Expiry travels the same way a relabel does: an admin changing
			// a distributed key's expiry after the fact expects it to
			// follow the key to every peer holding a copy, exactly like
			// renaming its label does (see config.KeySetExpiry's doc). So
			// either changing is enough to treat this as an update worth
			// flooding onward, and both fields are adopted together, not
			// just whichever one changed.
			if tracked && (existing.label != label || existing.expires != expiresNano) {
				existing.label = label
				existing.expires = expiresNano
				existing.slotHint = slotHint
				ns.propagatedKeys[id] = existing
				return keyAddMetaChanged
			}
			return keyAddUnchanged // known, same label and expiry → stop the flood
		}
	}
	ns.keys.Store(cur.With(raw)) // usable for handshakes immediately
	if ns.propagatedKeys == nil {
		ns.propagatedKeys = make(map[crypto.KeyID]propagatedKey)
	}
	ns.propagatedKeys[id] = propagatedKey{raw: append([]byte(nil), raw...), label: label, expires: expiresNano, slotHint: slotHint}
	return keyAddNew
}

// onKeyAdd handles a flooded add/metadata-update: fold it in (or update the
// label/expiry) and continue the flood if anything changed; if not (we
// already have this exact key/label/expiry), stop (de-dup).
func (e *Engine) onKeyAdd(ps *peerSession, body []byte) {
	raw, label, exp, slotHint, ok := decodeKeyAdd(body)
	if !ok || len(raw) != crypto.KeySize {
		return
	}
	ns := ps.net
	switch e.addPropagatedKey(ns, raw, label, exp, slotHint, true) {
	case keyAddUnchanged:
		return // known already, same label/expiry; do not re-flood
	case keyAddNew:
		e.log.Infof("mesh: received distributed key %x (label=%q) on net %x", crypto.DeriveKeyID(raw), label, ns.spec.ID)
	case keyAddMetaChanged:
		e.log.Infof("mesh: key %x updated (label=%q) on net %x", crypto.DeriveKeyID(raw), label, ns.spec.ID)
	}
	e.notifyChange(ns.spec.ID) // let the persist hook write it (or the new label/expiry) into config
	e.floodControl(ns, encodeKeyAdd(raw, label, exp, slotHint), ps)
}

// onKeyDel handles a flooded retraction: if we currently hold this key and
// haven't already processed this exact retraction, mark it for removal (the
// persist hook applies it via config.KeyDelete, which refuses to strip a
// network's last enabled key) and continue the flood; otherwise stop — either
// we never had it, or we've already seen and processed this retraction, so
// re-flooding again would be redundant. Checking "already marked" rather than
// "no longer in the live key set" is what makes this immediately idempotent:
// removal from the live set only happens later, indirectly, once the persist
// hook's config change is picked up by a reload — waiting on that would leave
// a window where a retransmitted or re-gossiped copy of the same retraction
// keeps re-triggering a re-flood.
func (e *Engine) onKeyDel(ps *peerSession, body []byte) {
	id, ok := decodeKeyDel(body)
	if !ok {
		return
	}
	ns := ps.net
	ns.mu.Lock()
	if ns.retractedKeys != nil && ns.retractedKeys[id] {
		ns.mu.Unlock()
		return // already processed this exact retraction — stop the flood
	}
	cur := ns.keys.Load()
	have := false
	if cur != nil {
		_, have = cur.Lookup(id)
	}
	if have {
		if ns.retractedKeys == nil {
			ns.retractedKeys = make(map[crypto.KeyID]bool)
		}
		ns.retractedKeys[id] = true
	}
	ns.mu.Unlock()
	if !have {
		return // don't have it, and never processed it → stop the flood
	}
	e.log.Infof("mesh: key %x retracted on net %x", id, ns.spec.ID)
	e.notifyChange(ns.spec.ID) // let the persist hook remove it from config
	e.floodControl(ns, encodeKeyDel(id), ps)
}

// DistributeKey adds keyB64 to this network's live key set and floods it to
// every current member so they add it too. It is the "propagate a rotated key"
// primitive: distribute the new key, let it reach the mesh, then retire the old
// key in config (which forces each peer to re-handshake onto a still-valid key).
//
// It is additive and never removes a key. label and expiresNano are metadata
// carried for persistence and the admin UI; expiresNano is unix nanoseconds, 0
// meaning "never". See the file header for why this must not be used for
// compromise-driven rotation.
func (e *Engine) DistributeKey(networkID uint64, keyB64, label string, expiresNano int64) error {
	ns := e.network(networkID)
	if ns == nil {
		return errors.New("unknown network")
	}
	raw, err := crypto.DecodeKey(keyB64)
	if err != nil {
		return err
	}
	// allowRelabel=false: DistributeKey's whole contract is "this must be a
	// genuinely new key" — a repeat call with a different label is a relabel,
	// not a rotation, and belongs to FloodKey instead.
	if e.addPropagatedKey(ns, raw, label, expiresNano, -1, false) == keyAddUnchanged {
		return errors.New("key already present on this network")
	}
	e.log.Infof("mesh: distributing rotated key %x (label=%q) on net %x", crypto.DeriveKeyID(raw), label, networkID)
	e.notifyChange(networkID)
	e.floodControl(ns, encodeKeyAdd(raw, label, expiresNano, -1), nil)
	return nil
}

// FloodKey (re)sends a key this node already holds to every currently
// connected peer on the network, for the web UI's per-key "Distributed"
// checkbox: push a key just generated or imported here out to the mesh
// without manual copy/paste, ahead of retiring an old one. Recipients fold it
// in over the same path as DistributeKey (onKeyAdd), including their own
// further re-flood to peers not directly connected to this node, so
// propagation still reaches the whole mesh in hops, not just this node's
// immediate peers — but only peers connected at the moment of the call (or a
// later call) receive it; anyone offline the entire time needs the key some
// other way. slot is this node's own slot for the key (0-7), passed along as a
// hint so a recipient with that exact slot free uses it too — pass -1 for no
// preference (e.g. a pure relabel where placement doesn't matter, since the
// recipient already has the key).
//
// Unlike DistributeKey — which onboards a brand-new key the mesh doesn't know
// yet and refuses a repeat as a loop guard — this is expected to be called on
// a key this node already owns in config, and can be called again later (e.g.
// once more peers have joined, or to push a relabel) without being refused.
func (e *Engine) FloodKey(networkID uint64, keyB64, label string, expiresNano int64, slot int) error {
	ns := e.network(networkID)
	if ns == nil {
		return errors.New("unknown network")
	}
	raw, err := crypto.DecodeKey(keyB64)
	if err != nil {
		return err
	}
	id := crypto.DeriveKeyID(raw)
	ns.mu.Lock()
	cur := ns.keys.Load()
	have := false
	if cur != nil {
		_, have = cur.Lookup(id)
	}
	if !have {
		ns.keys.Store(cur.With(raw))
	}
	ns.mu.Unlock()
	e.log.Infof("mesh: distributing key %x (label=%q) to connected peers on net %x", id, label, networkID)
	e.floodControl(ns, encodeKeyAdd(raw, label, expiresNano, slot), nil)
	return nil
}

// RetractKey floods a retraction of keyB64 to every currently connected peer
// on the network, for the web UI's "Distributed" checkbox being unticked: ask
// every peer that has a copy to remove it. It does not touch this node's own
// key slot (see the file header for why retraction is symmetric rather than
// origin-only) — the caller is expected to separately clear its own
// Distributed flag in config, which is bookkeeping only, not a key removal.
func (e *Engine) RetractKey(networkID uint64, keyB64 string) error {
	ns := e.network(networkID)
	if ns == nil {
		return errors.New("unknown network")
	}
	raw, err := crypto.DecodeKey(keyB64)
	if err != nil {
		return err
	}
	id := crypto.DeriveKeyID(raw)
	e.log.Infof("mesh: retracting key %x from connected peers on net %x", id, networkID)
	e.floodControl(ns, encodeKeyDel(id), nil)
	return nil
}

// PropagatedKeyInfo is a mesh-learned key surfaced to the persist hook / admin.
type PropagatedKeyInfo struct {
	KeyB64   string
	Label    string
	Expires  string // RFC3339; "" = never
	SlotHint int    // 0-7 = preferred slot if free; -1 = no preference
}

// expiryRFC3339 formats a propagatedKey's unix-nanos expiry the same way
// everywhere it's compared against or written to config's RFC3339 Expires
// string: 0 means never, so it maps to "" rather than the 1970 epoch.
func expiryRFC3339(nano int64) string {
	if nano == 0 {
		return ""
	}
	return time.Unix(0, nano).UTC().Format(time.RFC3339)
}

// PropagatedKeys returns the keys this node learned via mesh propagation, so
// the persist hook can fold any not-yet-in-config keys into config (in
// SlotHint if that slot is free, else the first free slot), and reconcile the
// label/expiry of any already in config whose mesh-learned copy has since
// changed. Returns raw key material (base64) — callers write it only to the
// same place config keys live.
func (e *Engine) PropagatedKeys(networkID uint64) []PropagatedKeyInfo {
	ns := e.network(networkID)
	if ns == nil {
		return nil
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	out := make([]PropagatedKeyInfo, 0, len(ns.propagatedKeys))
	for _, pk := range ns.propagatedKeys {
		out = append(out, PropagatedKeyInfo{
			KeyB64:   base64.StdEncoding.EncodeToString(pk.raw),
			Label:    pk.label,
			Expires:  expiryRFC3339(pk.expires),
			SlotHint: pk.slotHint,
		})
	}
	return out
}

// RetractedKeys returns the IDs of keys this node has been told to retract
// (via a flooded ctrlKeyDel) that it still holds, so the persist hook can
// remove the matching config slot — subject to config.KeyDelete's own refusal
// to strip a network's last enabled key.
func (e *Engine) RetractedKeys(networkID uint64) []crypto.KeyID {
	ns := e.network(networkID)
	if ns == nil {
		return nil
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	out := make([]crypto.KeyID, 0, len(ns.retractedKeys))
	for id := range ns.retractedKeys {
		out = append(out, id)
	}
	return out
}

// forgetConfiguredKeys drops propagated-key metadata that config has fully
// caught up with — i.e. the key is present in the given (freshly rebuilt) live
// key set *and* labels agrees with what the mesh last told us the label is
// *and* expires agrees with what the mesh last told us the expiry is. Called
// from the reload path so a key/label/expiry, once persisted, is no longer
// re-surfaced by PropagatedKeys — but a label or expiry change the persist
// hook hasn't written yet (or that raced with an intervening admin edit)
// keeps the entry alive so the next persist cycle retries the reconciliation,
// rather than a slot the operator later clears being silently re-learned
// forever, or a pending relabel/expiry-change being dropped on the floor.
// Caller must hold ns.mu.
func (ns *netState) forgetConfiguredKeys(ks *crypto.KeySet, labels map[crypto.KeyID]string, expires map[crypto.KeyID]string) {
	if ks == nil || len(ns.propagatedKeys) == 0 {
		return
	}
	for id, pk := range ns.propagatedKeys {
		if _, ok := ks.Lookup(id); !ok {
			continue // config doesn't have it yet; the persist hook still needs to add it
		}
		if labels[id] != pk.label {
			continue // config has it, but under a different label; still needs reconciling
		}
		if expires[id] != expiryRFC3339(pk.expires) {
			continue // config has it, but with a different expiry; still needs reconciling
		}
		delete(ns.propagatedKeys, id)
	}
}

// forgetAppliedRetractions drops retraction bookkeeping once the key is
// actually gone from the live key set — i.e. the persist hook's
// config.KeyDelete succeeded and a reload rebuilt the live set without it.
// A retraction config.KeyDelete refused (the network's last enabled key) is
// deliberately NOT forgotten here: it stays pending so the persist hook keeps
// retrying once the operator resolves that (e.g. enables another key), rather
// than the retraction being silently abandoned. Caller must hold ns.mu.
func (ns *netState) forgetAppliedRetractions(ks *crypto.KeySet) {
	if len(ns.retractedKeys) == 0 {
		return
	}
	for id := range ns.retractedKeys {
		still := false
		if ks != nil {
			_, still = ks.Lookup(id)
		}
		if !still {
			delete(ns.retractedKeys, id)
		}
	}
}
