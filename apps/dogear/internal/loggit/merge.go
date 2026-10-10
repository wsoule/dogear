package loggit

import (
	"slices"
	"time"
)

// SideEntry is one list entry as seen on one side, keyed elsewhere by the
// loggit media id.
type SideEntry struct {
	Note    string
	Updated time.Time
}

// Meta is the synced list-level fields. Visibility is normalized to
// "public" or "private" (Dogear's "unlisted" counts as private).
type Meta struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
}

// Side is a full snapshot of one list.
type Side struct {
	Entries     map[string]SideEntry
	Order       []string // keys in display order
	Meta        Meta
	MetaUpdated time.Time
}

// BaseEntry is what an entry looked like on both sides after the last sync.
type BaseEntry struct {
	Note string `json:"note"`
}

// Baseline is the state both sides agreed on after the last sync. Empty
// (zero value) means the lists were just linked.
type Baseline struct {
	Entries map[string]BaseEntry `json:"entries"`
	Order   []string             `json:"order"`
	Meta    *Meta                `json:"meta,omitempty"`
}

// Plan is what a sync must do to bring both sides to the same state.
type Plan struct {
	LocalAdd, LocalDelete   []string
	RemoteAdd, RemoteDelete []string
	LocalNote, RemoteNote   map[string]string
	LocalOrder, RemoteOrder []string // nil when that side's order is already right
	LocalMeta, RemoteMeta   *Meta    // nil when unchanged
	Next                    Baseline // baseline to store once the plan is applied
}

// Empty reports whether the plan changes nothing on either side.
func (p Plan) Empty() bool {
	return len(p.LocalAdd)+len(p.LocalDelete)+len(p.RemoteAdd)+len(p.RemoteDelete)+
		len(p.LocalNote)+len(p.RemoteNote) == 0 &&
		p.LocalOrder == nil && p.RemoteOrder == nil && p.LocalMeta == nil && p.RemoteMeta == nil
}

// Merge computes a three-way merge of the local (Dogear) and remote (loggit)
// lists against the last synced baseline:
//   - an entry on one side only is added to the other side, unless it was in
//     the baseline, in which case the other side deleted it and the delete
//     wins;
//   - when a note changed on one side only, that change is copied; when both
//     changed, the more recently edited one wins;
//   - order and list fields follow whichever side changed since the baseline,
//     and the more recently edited list when both did. On first link the
//     local (Dogear) side wins ties.
func Merge(base Baseline, local, remote Side) Plan {
	p := Plan{LocalNote: map[string]string{}, RemoteNote: map[string]string{}}
	final := map[string]string{} // key -> agreed note
	keys := unionKeys(local.Entries, remote.Entries, base.Entries)
	for _, k := range keys {
		l, inL := local.Entries[k]
		r, inR := remote.Entries[k]
		b, inB := base.Entries[k]
		switch {
		case inL && inR:
			note := l.Note
			if l.Note != r.Note {
				lChanged := !inB || l.Note != b.Note
				rChanged := !inB || r.Note != b.Note
				switch {
				case lChanged && !rChanged:
					note = l.Note
				case rChanged && !lChanged:
					note = r.Note
				case r.Updated.After(l.Updated):
					note = r.Note
				}
				if note != l.Note {
					p.LocalNote[k] = note
				}
				if note != r.Note {
					p.RemoteNote[k] = note
				}
			}
			final[k] = note
		case inL && !inR:
			if inB {
				p.LocalDelete = append(p.LocalDelete, k)
			} else {
				p.RemoteAdd = append(p.RemoteAdd, k)
				final[k] = l.Note
			}
		case !inL && inR:
			if inB {
				p.RemoteDelete = append(p.RemoteDelete, k)
			} else {
				p.LocalAdd = append(p.LocalAdd, k)
				final[k] = r.Note
			}
		}
	}

	// Order: compare each side's relative order of the entries that were in
	// the baseline and are still on both sides.
	common := func(seq []string) []string {
		return slices.DeleteFunc(slices.Clone(seq), func(k string) bool {
			_, inB := base.Entries[k]
			_, inL := local.Entries[k]
			_, inR := remote.Entries[k]
			return !(inB && inL && inR)
		})
	}
	baseRel := common(base.Order)
	lOrderChanged := !slices.Equal(common(local.Order), baseRel)
	rOrderChanged := !slices.Equal(common(remote.Order), baseRel)
	winner, other := local.Order, remote.Order
	if rOrderChanged && (!lOrderChanged || remote.MetaUpdated.After(local.MetaUpdated)) {
		winner, other = remote.Order, local.Order
	}
	inFinal := func(k string) bool { _, ok := final[k]; return ok }
	order := slices.DeleteFunc(slices.Clone(winner), func(k string) bool { return !inFinal(k) })
	for _, k := range other {
		if inFinal(k) && !slices.Contains(order, k) {
			order = append(order, k)
		}
	}
	// Anything left (only possible with inconsistent snapshots) goes last.
	for _, k := range keys {
		if inFinal(k) && !slices.Contains(order, k) {
			order = append(order, k)
		}
	}
	// Each side appends its adds at the end; only reorder a side when that
	// result differs from the agreed order.
	afterAdds := func(seq, adds []string) []string {
		now := slices.DeleteFunc(slices.Clone(seq), func(k string) bool { return !inFinal(k) })
		return append(now, adds...)
	}
	if !slices.Equal(afterAdds(local.Order, p.LocalAdd), order) {
		p.LocalOrder = order
	}
	if !slices.Equal(afterAdds(remote.Order, p.RemoteAdd), order) {
		p.RemoteOrder = order
	}

	// List fields.
	meta := local.Meta
	if local.Meta != remote.Meta {
		lChanged := base.Meta == nil || local.Meta != *base.Meta
		rChanged := base.Meta != nil && remote.Meta != *base.Meta
		if rChanged && (!lChanged || remote.MetaUpdated.After(local.MetaUpdated)) {
			meta = remote.Meta
		}
		if meta != local.Meta {
			m := meta
			p.LocalMeta = &m
		}
		if meta != remote.Meta {
			m := meta
			p.RemoteMeta = &m
		}
	}

	p.Next = Baseline{Entries: map[string]BaseEntry{}, Order: order, Meta: &meta}
	for k, note := range final {
		p.Next.Entries[k] = BaseEntry{Note: note}
	}
	return p
}

func unionKeys[A, B, C any](a map[string]A, b map[string]B, c map[string]C) []string {
	seen := map[string]bool{}
	var out []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range a {
		add(k)
	}
	for k := range b {
		add(k)
	}
	for k := range c {
		add(k)
	}
	slices.Sort(out)
	return out
}
