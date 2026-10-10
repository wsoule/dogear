package loggit

import (
	"slices"
	"testing"
	"time"
)

var (
	t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

func side(order []string, notes map[string]string, at time.Time) Side {
	s := Side{Entries: map[string]SideEntry{}, Order: order, Meta: Meta{Title: "Picks", Visibility: "public"}, MetaUpdated: at}
	for _, k := range order {
		s.Entries[k] = SideEntry{Note: notes[k], Updated: at}
	}
	return s
}

func base(order []string, notes map[string]string) Baseline {
	b := Baseline{Entries: map[string]BaseEntry{}, Order: order, Meta: &Meta{Title: "Picks", Visibility: "public"}}
	for _, k := range order {
		b.Entries[k] = BaseEntry{Note: notes[k]}
	}
	return b
}

func TestFirstLinkUnionsBothSides(t *testing.T) {
	p := Merge(Baseline{}, side([]string{"a", "b"}, nil, t0), side([]string{"b", "c"}, nil, t0))
	if !slices.Equal(p.RemoteAdd, []string{"a"}) || !slices.Equal(p.LocalAdd, []string{"c"}) {
		t.Fatalf("adds: local %v remote %v", p.LocalAdd, p.RemoteAdd)
	}
	if !slices.Equal(p.Next.Order, []string{"a", "b", "c"}) {
		t.Fatalf("order %v", p.Next.Order)
	}
	if len(p.LocalDelete)+len(p.RemoteDelete) != 0 {
		t.Fatal("first link must not delete")
	}
}

func TestDeletesPropagateAndBeatEdits(t *testing.T) {
	b := base([]string{"a", "b"}, map[string]string{"a": "x", "b": "y"})
	local := side([]string{"a"}, map[string]string{"a": "x"}, t1)                      // b deleted locally
	remote := side([]string{"a", "b"}, map[string]string{"a": "x", "b": "edited"}, t2) // b edited remotely, later
	p := Merge(b, local, remote)
	if !slices.Equal(p.RemoteDelete, []string{"b"}) || len(p.LocalAdd) != 0 {
		t.Fatalf("delete should win: %+v", p)
	}
}

func TestNoteEditsFlowAndNewestWins(t *testing.T) {
	b := base([]string{"a", "b"}, map[string]string{"a": "old", "b": "old"})
	local := side([]string{"a", "b"}, map[string]string{"a": "local", "b": "local-b"}, t1)
	remote := side([]string{"a", "b"}, map[string]string{"a": "old", "b": "remote-b"}, t2)
	p := Merge(b, local, remote)
	if p.RemoteNote["a"] != "local" {
		t.Fatalf("one-sided edit should copy: %v", p.RemoteNote)
	}
	if p.LocalNote["b"] != "remote-b" {
		t.Fatalf("newer edit should win: %v", p.LocalNote)
	}
}

func TestOrderFollowsChangedSide(t *testing.T) {
	b := base([]string{"a", "b", "c"}, nil)
	local := side([]string{"a", "b", "c"}, nil, t0)
	remote := side([]string{"c", "a", "b"}, nil, t1)
	p := Merge(b, local, remote)
	if !slices.Equal(p.LocalOrder, []string{"c", "a", "b"}) || p.RemoteOrder != nil {
		t.Fatalf("local %v remote %v", p.LocalOrder, p.RemoteOrder)
	}
}

func TestUnchangedIsEmpty(t *testing.T) {
	b := base([]string{"a", "b"}, map[string]string{"a": "n"})
	p := Merge(b, side([]string{"a", "b"}, map[string]string{"a": "n"}, t0), side([]string{"a", "b"}, map[string]string{"a": "n"}, t1))
	if !p.Empty() {
		t.Fatalf("expected no-op, got %+v", p)
	}
}

func TestMetaNewerWins(t *testing.T) {
	b := base(nil, nil)
	local := side(nil, nil, t1)
	local.Meta.Title = "Local title"
	remote := side(nil, nil, t2)
	remote.Meta.Title = "Remote title"
	p := Merge(b, local, remote)
	if p.LocalMeta == nil || p.LocalMeta.Title != "Remote title" || p.RemoteMeta != nil {
		t.Fatalf("meta: %+v %+v", p.LocalMeta, p.RemoteMeta)
	}
}

func TestAddsLandAtEndOnBothSides(t *testing.T) {
	b := base([]string{"a"}, nil)
	p := Merge(b, side([]string{"a", "l"}, nil, t0), side([]string{"a", "r"}, nil, t0))
	if !slices.Equal(p.Next.Order, []string{"a", "l", "r"}) {
		t.Fatalf("order %v", p.Next.Order)
	}
	// Remote gets l appended after r, so it must be reordered to a,l,r.
	if !slices.Equal(p.RemoteOrder, []string{"a", "l", "r"}) || p.LocalOrder != nil {
		t.Fatalf("local %v remote %v", p.LocalOrder, p.RemoteOrder)
	}
}
