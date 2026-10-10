// Package ranking holds the Wilson-score ranking used by every feed.
package ranking

import "math"

const (
	z            = 1.96
	pseudoVotes  = 8.0
	maxStarsNorm = 50000.0
	maxListsNorm = 5
)

// Prior returns the 0..1 prior for an imported item from its GitHub stars
// (nil when unknown) and the number of awesome lists that contain it.
func Prior(stars *int, listCount int) float64 {
	normLists := float64(min(listCount, maxListsNorm)) / maxListsNorm
	if stars == nil {
		return 0.5 * normLists
	}
	normStars := math.Min(1, math.Log1p(float64(max(*stars, 0)))/math.Log1p(maxStarsNorm))
	return 0.6*normStars + 0.4*normLists
}

// Score is the lower bound of the 95% Wilson interval over real votes plus
// pseudo-votes for imported items, so imported items have a sensible order
// before anyone votes and one like never outranks a long track record.
func Score(likes, dislikes int, prior float64, imported bool) float64 {
	pseudoN := 0.0
	if imported {
		pseudoN = pseudoVotes
	}
	n := float64(likes+dislikes) + pseudoN
	if n == 0 {
		return 0
	}
	p := (float64(likes) + pseudoN*prior) / n
	z2 := z * z
	return (p + z2/(2*n) - z*math.Sqrt((p*(1-p)+z2/(4*n))/n)) / (1 + z2/n)
}
