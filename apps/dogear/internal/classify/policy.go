package classify

import (
	"net/url"
	"strings"

	"dogear/internal/safety"
)

// UnsortedSlug is the hidden category for items the classifier cannot place.
const UnsortedSlug = "unsorted"

// Decision flags.
const (
	FlagUnsafe            = "unsafe"
	FlagPromo             = "promo"
	FlagInjection         = "injection"
	FlagMismatch          = "mismatch"
	FlagUnsorted          = "unsorted"
	FlagClassifierUnknown = "classifier_unknown"
)

// Thresholds are the policy cut-offs, tuned against testdata/classify/eval.jsonl.
type Thresholds struct {
	Unsafe, PromoReject, PromoHold, Injection, Mismatch, Category float64
}

// DefaultThresholds returns the spec's starting thresholds.
func DefaultThresholds() Thresholds {
	return Thresholds{Unsafe: 0.8, PromoReject: 0.95, PromoHold: 0.7, Injection: 0.7, Mismatch: 0.8, Category: 0.6}
}

// Decision is what the policy says to do with a submission. The classifier can
// only reject or hold; it never makes an item live. When neither Reject nor
// Hold is set, the poster's trust level decides the status.
type Decision struct {
	Reject bool
	Hold   bool
	Flags  []string
	// CategorySlug is the final category: "" means keep the poster's choice.
	// It is UnsortedSlug when Unsorted is true.
	CategorySlug string
	Unsorted     bool
}

// Decide applies the spec's policy table to a classifier result.
// posterCategory is the slug the poster supplied ("" if none). Noop results
// always hold with flag "classifier_unknown". Category rules: a poster's
// category is kept unless the classifier confidently says "none of these"
// (then unsorted and held); with no poster category, the classifier's slug is
// used when its confidence reaches t.Category, else unsorted and held.
func Decide(r Result, posterCategory string, t Thresholds) Decision {
	var d Decision
	flag := func(f string) { d.Flags = append(d.Flags, f) }
	unsorted := func() {
		d.Unsorted, d.Hold, d.CategorySlug = true, true, UnsortedSlug
		flag(FlagUnsorted)
	}

	if r.Model == NoopModel {
		d.Hold = true
		flag(FlagClassifierUnknown)
		if posterCategory == "" {
			unsorted()
		}
		return d
	}

	if r.UnsafeConf >= t.Unsafe {
		d.Reject = true
		flag(FlagUnsafe)
	}
	switch {
	case r.PromoConf >= t.PromoReject:
		d.Reject = true
		flag(FlagPromo)
	case r.PromoConf >= t.PromoHold:
		d.Hold = true
		flag(FlagPromo)
	}
	if r.InjectionConf >= t.Injection {
		d.Hold = true
		flag(FlagInjection)
	}
	if r.MismatchConf >= t.Mismatch {
		d.Hold = true
		flag(FlagMismatch)
	}

	if posterCategory != "" {
		if r.CategorySlug == "" && r.CategoryConf >= t.Category {
			unsorted()
		}
	} else {
		if r.CategorySlug != "" && r.CategoryConf >= t.Category {
			d.CategorySlug = r.CategorySlug
		} else {
			unsorted()
		}
	}
	return d
}

// minKindConf is the confidence the classifier's kind needs before it is used.
const minKindConf = 0.6

// KindFor implements "Assigning a kind": the poster's choice wins, then domain
// rules (YouTube -> video, a GitHub repo -> tool, app stores -> app), then the
// classifier's kind when its confidence is at least 0.6, else "other".
// normURL is the normalized item URL.
func KindFor(posterKind, normURL string, r Result) string {
	if pk := strings.ToLower(strings.TrimSpace(posterKind)); IsKind(pk) {
		return pk
	}
	if k := kindFromDomain(normURL); k != "" {
		return k
	}
	if IsKind(r.Kind) && r.KindConf >= minKindConf {
		return r.Kind
	}
	return "other"
}

// githubNonRepo lists first path segments on github.com that are site pages,
// not user or org names, so /{seg}/{x} there is not a repository.
var githubNonRepo = map[string]bool{
	"orgs": true, "topics": true, "sponsors": true, "marketplace": true, "features": true,
	"collections": true, "settings": true, "about": true, "pricing": true, "explore": true,
	"trending": true, "enterprise": true, "login": true, "join": true, "site": true,
	"apps": true, "users": true, "search": true, "notifications": true, "issues": true,
	"pulls": true, "events": true, "readme": true, "security": true, "customer-stories": true,
}

// kindFromDomain returns the kind implied by well-known hosts, or "".
func kindFromDomain(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch host {
	case "youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be":
		if safety.IsYouTubeChannel(raw) {
			return "channel"
		}
		return "video"
	case "apps.apple.com", "play.google.com":
		return "app"
	case "github.com":
		segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
		if len(segs) >= 2 && !githubNonRepo[strings.ToLower(segs[0])] {
			return "tool"
		}
	}
	return ""
}
