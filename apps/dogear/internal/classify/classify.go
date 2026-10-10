// Package classify screens and labels submitted items. A Classifier answers
// six narrow questions about a submission (kind, category, promo, mismatch,
// injection, unsafe); Decide turns those answers into a reject/hold decision
// and KindFor picks the item's final kind.
package classify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Kinds is the fixed set of item kinds, matching the items.kind CHECK constraint.
var Kinds = []string{"site", "tool", "app", "book", "device", "video", "article", "other"}

// IsKind reports whether k is one of Kinds.
func IsKind(k string) bool {
	for _, v := range Kinds {
		if v == k {
			return true
		}
	}
	return false
}

// Input is everything the classifier sees about one submission. All text
// fields are untrusted and are sent to the provider only as data.
type Input struct {
	URL, Title, Blurb, PageTitle, MetaDescription, PageText string
	// Categories are the leaf categories the classifier may choose from.
	Categories []Category
}

// Category is one leaf category offered to the category question.
type Category struct {
	Slug, Name, Description string
}

// Result holds the raw classifier answers. Confidences are in [0,1].
type Result struct {
	Kind     string
	KindConf float64
	// CategorySlug is "" when the classifier chose "none of these".
	CategorySlug string
	CategoryConf float64

	PromoConf, MismatchConf, InjectionConf, UnsafeConf float64

	Model string
}

// Classifier classifies one submission.
type Classifier interface {
	Classify(ctx context.Context, in Input) (Result, error)
}

// NoopModel is the Result.Model value Noop returns.
const NoopModel = "noop"

// Noop returns unknown for everything (zero confidences, Model "noop"); policy
// treats it as "hold".
type Noop struct{}

// Classify implements Classifier.
func (Noop) Classify(context.Context, Input) (Result, error) {
	return Result{Model: NoopModel}, nil
}

// New builds the classifier selected by the CLASSIFIER setting. name is "jev"
// or "noop" ("" also means noop). baseURL and httpClient may be empty/nil to
// use the defaults.
func New(name, apiKey, baseURL string, httpClient *http.Client) (Classifier, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "noop":
		return Noop{}, nil
	case "jev":
		if apiKey == "" {
			return nil, fmt.Errorf("classify: jev classifier requires an API key")
		}
		return &Jev{APIKey: apiKey, BaseURL: baseURL, HTTP: httpClient}, nil
	default:
		return nil, fmt.Errorf("classify: unknown classifier %q (want jev or noop)", name)
	}
}
