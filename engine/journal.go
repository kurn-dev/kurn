package engine

// journalRec is one journal line: an upsert carrying the entry, or a delete
// carrying the ID.
type journalRec struct {
	Op    string `json:"op"` // "upsert" | "delete"
	Entry *Entry `json:"entry,omitempty"`
	ID    string `json:"id,omitempty"`
}

// applyJournalLocked folds a whole journal into the list in one batch: all
// records are applied in order to overlaySrc and a cloned tombstone set with
// exactly Upsert/Delete semantics (last-wins), then the overlay segment is
// rebuilt ONCE and a single new snapshot is stored (one gen bump). Replaying
// via per-record Upsert/Delete would rebuild the overlay per record —
// quadratic in journal length — and is forbidden.
//
// Snapshot invariants upheld (see snapshot doc):
//   - tombstones ⊆ base.byID: an ID is only tombstoned after a byID hit;
//   - a live ID is never in both overlay and un-tombstoned base: an upsert of
//     a base ID sets overlaySrc AND tombstones the base copy;
//   - tombstones never apply to overlay entries: commitOverlayLocked passes
//     the set to the snapshot where Query masks base hits only.
//
// Caller holds l.mu. Records with an unknown op or an upsert without an entry
// are ignored (a torn/corrupt journal tail must not poison replay). jpos is
// the journal byte position the ops end at, for the version stamp.
func (l *List) applyJournalLocked(ops []journalRec, jpos int64) error {
	if len(ops) == 0 {
		return nil
	}
	b, err := l.prepareOverlayLocked(func(src map[string]Entry, tomb map[string]struct{}, base *segment) {
		inBase := func(id string) bool {
			if base == nil {
				return false
			}
			_, ok := base.byID[id]
			return ok
		}
		for _, op := range ops {
			switch op.Op {
			case "upsert":
				if op.Entry == nil {
					continue
				}
				e := *op.Entry
				src[e.ID] = e // later records win, like sequential Upsert
				if inBase(e.ID) {
					tomb[e.ID] = struct{}{} // mask the superseded base copy
				}
			case "delete":
				delete(src, op.ID)
				if inBase(op.ID) {
					tomb[op.ID] = struct{}{}
				}
			}
		}
	})
	if err != nil {
		// A journal that cannot be built (e.g. an exact-mode hot key written
		// before build-time validation existed) is poison: returning the
		// error without committing lets Open quarantine the file and start
		// anyway, instead of panicking mid-replay.
		return err
	}
	l.commitOverlayLockedAt(b, jpos)
	return nil
}
