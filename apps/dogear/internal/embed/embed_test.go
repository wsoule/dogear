package embed

import "testing"

func vec(xs ...int8) []int8 {
	v := make([]int8, Dims)
	copy(v, xs)
	return v
}

func TestNearest(t *testing.T) {
	ix := NewIndex()
	ix.Put(1, vec(100, 0))
	ix.Put(2, vec(70, 70))
	ix.Put(3, vec(0, 100))
	ix.Put(4, vec(-100, 0))
	got := ix.Nearest(vec(90, 10), 2, 0)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("nearest: %+v", got)
	}
	if got := ix.Nearest(vec(90, 10), 1, 1); got[0].ID != 2 {
		t.Fatalf("exclude: %+v", got)
	}
	ix.Put(4, vec(95, 5)) // replace
	if got := ix.Nearest(vec(90, 10), 1, 0); got[0].ID != 4 && got[0].ID != 1 {
		t.Fatalf("replace: %+v", got)
	}
	if ix.Len() != 4 {
		t.Fatalf("len %d", ix.Len())
	}
	b := Bytes(vec(-5, 7))
	if v := FromBytes(b); v[0] != -5 || v[1] != 7 {
		t.Fatalf("bytes roundtrip: %v", v[:2])
	}
}
