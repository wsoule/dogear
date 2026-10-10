package safety

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// isInvisibleFormatRune reports bidi override/isolate/mark characters and
// zero-width characters. They are used to visually disguise text (e.g. to
// reverse a URL or hide instructions), so SanitizeText removes them outright.
func isInvisibleFormatRune(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E: // LRE, RLE, PDF, LRO, RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0x200E, r == 0x200F, r == 0x061C: // LRM, RLM, ALM
		return true
	case r >= 0x200B && r <= 0x200D: // ZWSP, ZWNJ, ZWJ
		return true
	case r == 0xFEFF, r == 0x2060: // BOM / ZWNBSP, word joiner
		return true
	}
	return false
}

// SanitizeText turns untrusted user or imported text into a single line of
// plain text: invalid UTF-8, bidi controls and zero-width characters are
// removed, control characters (including newlines and tabs) become spaces,
// whitespace runs collapse to one space, and the result is trimmed.
func SanitizeText(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case isInvisibleFormatRune(r):
			// drop
		case unicode.IsControl(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// tldPattern lists TLDs treated as URL-like when they follow a bare word, as in
// "example.com" or "foo.io/bar". It deliberately leaves out TLDs that collide
// with common file extensions or library names (.js, .md, .sh, .py, .rs, .go,
// .so, .ts, .in), so "Node.js" or "README.md" do not count as URLs. Matching is
// case-sensitive on the TLD so product names like "ASP.NET" are not flagged.
const tldPattern = `com|net|org|io|dev|app|ai|co|me|xyz|info|biz|ly|gg|tv|us|uk|de|fr|ru|cn|jp|eu|ca|au|nl|be|ch|se|no|fi|it|es|pl|br|site|online|store|tech|page|link|click|top|club|shop|blog|cc|fm|tk|ml|ga|cf|gq|ws|edu|gov|live|news|space|website|cloud|pro|today|art`

var (
	markupRe = regexp.MustCompile(strings.Join([]string{
		`</?[A-Za-z!?][^<>]*>`,          // HTML/XML tags, comments, doctype
		`\[[^\]]*\]\([^)]*\)`,           // markdown links / images
		`(?i:\b[a-z][a-z0-9+.\-]*://)`,  // any scheme://
		`(?i:\b(?:mailto|javascript):)`, // scheme without slashes
		`(?i:\bwww\.[a-z0-9-]+)`,        // www.anything
		`\b[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*\.(?:` + tldPattern + `)\b`, // bare domain
	}, "|"))
)

// HasMarkupOrURL reports whether text contains HTML-like tags, markdown links,
// or anything URL-like (scheme://, www., or a bare domain such as example.com
// or foo.io/path). Titles and blurbs must be plain text without links, so the
// service layer rejects text for which this returns true.
func HasMarkupOrURL(s string) bool {
	return markupRe.MatchString(s)
}

// injectionPatterns match instruction-like text addressed to an AI model.
// They are a cheap pre-filter in front of the classifier; matches send an item
// to moderation rather than rejecting it, so mild false positives are fine.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bignore\s+(?:all\s+|any\s+)?(?:of\s+)?(?:the\s+|your\s+|my\s+)?(?:previous|prior|above|earlier|preceding|foregoing|past)\s+(?:instructions?|prompts?|messages?|directions?|rules|context|commands?)`),
	regexp.MustCompile(`\bignore\s+(?:all\s+|any\s+)?(?:your|the)\s+(?:instructions|rules|guidelines)`),
	regexp.MustCompile(`\bdisregard\s+(?:\S+\s+){0,4}?(?:instructions?|prompts?|rules|directions?|guidelines|messages?)`),
	regexp.MustCompile(`\bforget\s+(?:\S+\s+){0,4}?(?:instructions?|prompts?|rules|guidelines)`),
	regexp.MustCompile(`\boverride\s+(?:\S+\s+){0,3}?(?:instructions?|rules|guidelines|safety)`),
	regexp.MustCompile(`\bsystem\s+prompts?\b`),
	regexp.MustCompile(`\byou\s+must\b`),
	regexp.MustCompile(`\byou\s+are\s+now\b`),
	regexp.MustCompile(`\bfrom\s+now\s+on\s*,?\s+you\b`),
	regexp.MustCompile(`\bassistant\s*:`),
	regexp.MustCompile(`^system\s*:`),
	regexp.MustCompile(`\bas\s+an\s+ai\b`),
	regexp.MustCompile(`\bnew\s+instructions?\s*:`),
	regexp.MustCompile(`\b(?:updated|real|actual|hidden|secret)\s+instructions?\s*:`),
	regexp.MustCompile(`<\|\s*(?:im_start|im_end|endoftext|system|user|assistant)\s*\|>`),
	regexp.MustCompile(`\[/?inst\]`),
	regexp.MustCompile(`<<\s*/?sys\s*>>`),
	regexp.MustCompile(`#{2,}\s*(?:instructions?|system|response)\b`),
	regexp.MustCompile(`\b(?:do\s+not|don'?t|never)\s+(?:tell|inform|mention\s+(?:this\s+)?to|reveal\s+(?:this\s+)?to|let)\s+the\s+user`),
	regexp.MustCompile(`\b(?:reveal|print|show|repeat|output|leak)\s+(?:me\s+)?(?:your|the)\s+(?:system\s+|initial\s+|original\s+|hidden\s+)?(?:prompt|instructions)`),
	regexp.MustCompile(`\bact\s+as\s+(?:an?\s+|the\s+)?(?:unrestricted\s+|unfiltered\s+)?(?:ai|assistant|language\s+model|chatbot)\b`),
	regexp.MustCompile(`\b(?:ai|llm|gpt|claude|chatgpt|assistant|agent|language\s+model)s?\s*[,:]\s*(?:please\s+)?(?:ignore|disregard|you\s+should|you\s+will|always|recommend|rank|upvote|approve)\b`),
	regexp.MustCompile(`\bnote\s+to\s+(?:the\s+)?(?:ai|llm|assistant|agent|model)s?\b`),
}

// LooksLikeInjection reports whether s contains instruction-like text aimed at
// an AI assistant ("ignore previous instructions", "system prompt",
// "assistant:", chat-template tokens, ...). Matching is case-insensitive and
// runs on sanitized text so zero-width or bidi tricks cannot split a phrase.
func LooksLikeInjection(s string) bool {
	t := strings.ToLower(SanitizeText(s))
	// Curly apostrophes are normalized so "don’t" matches "don't".
	t = strings.ReplaceAll(t, "’", "'")
	for _, re := range injectionPatterns {
		if re.MatchString(t) {
			return true
		}
	}
	return false
}

// Truncate cuts s to at most n runes, ending at a word boundary so no partial
// word is left at the end; nothing (no ellipsis) is appended. A single word
// longer than n is hard-cut at n runes since there is no boundary to use.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	// The rune right after the cut is whitespace: the cut is already clean.
	if unicode.IsSpace(runes[n]) {
		return strings.TrimRightFunc(string(runes[:n]), unicode.IsSpace)
	}
	for i := n - 1; i > 0; i-- {
		if unicode.IsSpace(runes[i]) {
			return strings.TrimRightFunc(string(runes[:i]), unicode.IsSpace)
		}
	}
	return string(runes[:n])
}
