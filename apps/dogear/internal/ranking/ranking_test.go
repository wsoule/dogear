package ranking

import (
	"math"
	"testing"
)

func ptr(i int) *int { return &i }

func TestScore(t *testing.T) {
	tests := []struct {
		name            string
		likes, dislikes int
		prior           float64
		imported        bool
		want, tolerance float64
	}{
		{"no votes user item", 0, 0, 0, false, 0, 0},
		{"top imported item", 0, 0, 1.0, true, 0.68, 0.01},
		{"one like user post", 1, 0, 0, false, 0.21, 0.01},
		{"95 likes 5 dislikes", 95, 5, 0, false, 0.888, 0.01},
		{"zero prior imported", 0, 0, 0, true, 0, 0.0001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Score(tt.likes, tt.dislikes, tt.prior, tt.imported)
			if math.Abs(got-tt.want) > tt.tolerance {
				t.Fatalf("Score() = %.4f, want %.4f ± %.4f", got, tt.want, tt.tolerance)
			}
		})
	}
}

func TestOneLikeDoesNotBeatTrackRecord(t *testing.T) {
	if Score(1, 0, 0, false) >= Score(95, 5, 0, false) {
		t.Fatal("one like outranked 95 likes and 5 dislikes")
	}
}

func TestRealVotesDominatePrior(t *testing.T) {
	imported := Score(0, 30, 1.0, true)
	if imported > 0.2 {
		t.Fatalf("30 dislikes should sink a top prior, got %.3f", imported)
	}
}

func TestPrior(t *testing.T) {
	tests := []struct {
		name      string
		stars     *int
		lists     int
		want, tol float64
	}{
		{"unknown stars one list", nil, 1, 0.1, 1e-9},
		{"unknown stars capped lists", nil, 9, 0.5, 1e-9},
		{"max stars max lists", ptr(50000), 5, 1.0, 1e-9},
		{"over max stars", ptr(500000), 5, 1.0, 1e-9},
		{"zero stars one list", ptr(0), 1, 0.08, 1e-9},
		{"1000 stars two lists", ptr(1000), 2, 0.6*math.Log1p(1000)/math.Log1p(50000) + 0.16, 1e-9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Prior(tt.stars, tt.lists); math.Abs(got-tt.want) > tt.tol {
				t.Fatalf("Prior() = %.4f, want %.4f", got, tt.want)
			}
		})
	}
}
