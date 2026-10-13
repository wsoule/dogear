package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"dogear/internal/classify"
	"dogear/internal/db"
	"dogear/internal/safety"
)

type NewItem struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Blurb    string `json:"blurb"`
	Category string `json:"category"`
	Kind     string `json:"kind"`
	Creator  string `json:"creator"`
	List     string `json:"list"`
	Note     string `json:"note"`
}

// CleanText sanitizes plain text and validates its length and content,
// returning an invalid Error that names the field.
func CleanText(field, s string, minLen, maxLen int) (string, error) {
	s = safety.SanitizeText(s)
	n := utf8.RuneCountInString(s)
	if n < minLen {
		if minLen == 1 {
			return "", errf("invalid", "%s is required", field)
		}
		return "", errf("invalid", "%s must be at least %d characters", field, minLen)
	}
	if n > maxLen {
		return "", errf("invalid", "%s must be at most %d characters", field, maxLen)
	}
	if s != "" && safety.HasMarkupOrURL(s) {
		return "", errf("invalid", "%s must be plain text without links or markup", field)
	}
	return s, nil
}

// checkURL normalizes a submitted URL and applies the URL rules: scheme,
// length, credentials, shorteners, blocked domains, and duplicates. A link
// that is on Dogear only as someone's import (a personal item) is not a
// duplicate: personalID is that item's id, for CreateItem to promote.
func (s *Service) checkURL(ctx context.Context, raw string) (norm, domain string, personalID int64, err error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 2048 {
		return "", "", 0, errf("invalid", "url must be at most 2048 characters")
	}
	norm, err = safety.NormalizeURL(raw)
	if err != nil {
		return "", "", 0, errf("invalid", "url must be a public http or https link")
	}
	domain = safety.Domain(norm)
	if safety.IsShortener(domain) {
		return "", "", 0, errf("invalid", "link shorteners are not accepted; use the full URL")
	}
	blocked, err := s.Q.DomainBlocked(ctx, domain)
	if err != nil {
		return "", "", 0, fmt.Errorf("domain blocked: %w", err)
	}
	if blocked {
		return "", "", 0, errf("invalid", "links to %s are not accepted", domain)
	}
	existing, err := s.Q.GetItemByNorm(ctx, norm)
	if err == nil {
		if existing.Status == "personal" {
			return norm, domain, existing.ID, nil
		}
		return "", "", 0, &Error{Code: "duplicate", Message: "this link is already on Dogear", ItemID: existing.ID}
	} else if !notFound(err) {
		return "", "", 0, fmt.Errorf("dup check: %w", err)
	}
	return norm, domain, 0, nil
}

// CreateItem posts a new item. It returns the created item; Status tells the
// caller whether it is live or pending review. Posting a link that exists
// only as someone's import runs every check as usual and then turns that
// personal item into the post in place, so saves of it keep working.
func (s *Service) CreateItem(ctx context.Context, a *Actor, in NewItem) (Item, error) {
	if err := require(a, "post"); err != nil {
		return Item{}, err
	}
	norm, domain, personalID, err := s.checkURL(ctx, in.URL)
	if err != nil {
		return Item{}, err
	}
	title, err := CleanText("title", in.Title, 1, 120)
	if err != nil {
		return Item{}, err
	}
	blurb, err := CleanText("blurb", in.Blurb, 10, 280)
	if err != nil {
		return Item{}, err
	}
	creator, err := CleanText("creator", in.Creator, 0, 120)
	if err != nil {
		return Item{}, err
	}
	note, err := CleanText("note", in.Note, 0, 280)
	if err != nil {
		return Item{}, err
	}
	if in.Kind != "" && !slices.Contains(Kinds, in.Kind) {
		return Item{}, errf("invalid", "kind must be one of %s", strings.Join(Kinds, ", "))
	}
	var category *db.Category
	if in.Category != "" {
		c, err := s.Q.GetCategory(ctx, in.Category)
		if notFound(err) || (err == nil && (c.ParentID == nil || c.Slug == "unsorted")) {
			return Item{}, errf("invalid", "category must be the slug of an existing subcategory")
		} else if err != nil {
			return Item{}, fmt.Errorf("category: %w", err)
		}
		category = &c
	}
	var list *db.List
	if in.List != "" {
		l, err := s.Q.GetList(ctx, db.GetListParams{UserID: a.ID(), Slug: in.List})
		if notFound(err) {
			return Item{}, errf("invalid", "you have no list %q", in.List)
		} else if err != nil {
			return Item{}, fmt.Errorf("list: %w", err)
		}
		list = &l
	}
	n, err := s.Q.UserDomainPosts(ctx, db.UserDomainPostsParams{CreatedBy: ptr(a.ID()), Domain: domain})
	if err != nil {
		return Item{}, fmt.Errorf("domain posts: %w", err)
	}
	if n >= 2 {
		return Item{}, &Error{Code: "rate_limited", Message: "you can post at most 2 links per domain per week", RetryAfter: 24 * time.Hour}
	}
	if key, rules := postKey(a); true {
		if err := s.limit(key, rules...); err != nil {
			return Item{}, err
		}
	}
	if unsafe, err := s.safeBrowsingFlagged(ctx, norm); err != nil {
		s.Log.Warn("safe browsing", "err", err)
	} else if unsafe {
		return Item{}, errf("rejected", "this link is flagged as unsafe")
	}

	// Classify. Errors never block posting; the item is held and retried.
	var flags []string
	if safety.LooksLikeInjection(title + " " + blurb + " " + note) {
		flags = append(flags, "injection_pattern")
	}
	posterCat := ""
	if category != nil {
		posterCat = category.Slug
	}
	res, cerr := s.classifyURL(ctx, a, norm, title, blurb)
	var dec classify.Decision
	if cerr != nil {
		s.Log.Warn("classify", "url", norm, "err", cerr)
		flags = append(flags, "classifier_error")
	} else {
		dec = classify.Decide(res, posterCat, s.Thresholds)
		if dec.Reject {
			return Item{}, errf("rejected", "this link does not meet the posting rules")
		}
		flags = append(flags, dec.Flags...)
	}
	kind := classify.KindFor(in.Kind, norm, res)
	if creator == "" {
		if e, err := s.Enrich(ctx, norm); err == nil {
			creator = e.Creator
		}
	}
	finalCat := category
	if cerr == nil {
		switch {
		case dec.Unsorted:
			finalCat = nil
		case dec.CategorySlug != "" && category == nil:
			if c, err := s.Q.GetCategory(ctx, dec.CategorySlug); err == nil && c.ParentID != nil {
				finalCat = &c
			}
		}
	}
	if finalCat == nil {
		c, err := s.Q.GetCategory(ctx, "unsorted")
		if err != nil {
			return Item{}, fmt.Errorf("unsorted category: %w", err)
		}
		finalCat = &c
		if !slices.Contains(flags, "unsorted") {
			flags = append(flags, "unsorted")
		}
	}

	status := "live"
	if a.User.TrustLevel < 1 || len(flags) > 0 || (cerr == nil && dec.Hold) {
		status = "pending"
	}
	via := a.Via
	if via == "" {
		via = "web"
	}
	var created db.Item
	err = s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if personalID != 0 {
			created, err = q.PromotePersonalItem(ctx, db.PromotePersonalItemParams{
				ID: personalID, Url: norm, Title: title, Blurb: blurb,
				CreatedBy: ptr(a.ID()), CreatedViaToken: a.TokenID, Via: via, Status: status,
				Kind: kind, Creator: truncateRunes(creator, 120), Flags: nonNil(flags),
			})
			if notFound(err) {
				// Someone else posted it first.
				return &Error{Code: "duplicate", Message: "this link is already on Dogear", ItemID: personalID}
			} else if err != nil {
				return fmt.Errorf("promote item: %w", err)
			}
			if err := q.ClearItemCategories(ctx, created.ID); err != nil {
				return fmt.Errorf("item category: %w", err)
			}
		} else {
			created, err = q.InsertItem(ctx, db.InsertItemParams{
				Url: norm, UrlNorm: norm, Domain: domain, Title: title, Blurb: blurb,
				CreatedBy: ptr(a.ID()), CreatedViaToken: a.TokenID, Via: via, Status: status,
				Kind: kind, Creator: truncateRunes(creator, 120), Flags: nonNil(flags),
			})
		}
		if err != nil {
			if isUniqueViolation(err) {
				return &Error{Code: "duplicate", Message: "this link is already on Dogear"}
			}
			return fmt.Errorf("insert item: %w", err)
		}
		if err := q.AddItemCategory(ctx, db.AddItemCategoryParams{ItemID: created.ID, CategoryID: finalCat.ID}); err != nil {
			return fmt.Errorf("item category: %w", err)
		}
		if cerr == nil && res.Model != "" {
			if err := saveClassification(ctx, q, created.ID, res); err != nil {
				return err
			}
		}
		// Everything you post also goes in your archive, tagged if you
		// picked a tag.
		if list != nil {
			if err := s.addEntry(ctx, q, a, *list, created.ID, &note, nil); err != nil {
				return err
			}
		} else if err := s.saveItem(ctx, q, a, created.ID, &note); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if e, ok := AsError(err); ok && e.Code == "duplicate" && e.ItemID == 0 {
			if ex, err := s.Q.GetItemByNorm(ctx, norm); err == nil {
				e.ItemID = ex.ID
			}
		}
		return Item{}, err
	}
	// File it into topics by keyword now; meaning follows once its
	// embedding is made (BackfillEmbeddings).
	if err := s.Q.FileItemTopicsByKeyword(ctx, created.ID); err != nil {
		s.Log.Warn("file item topics", "item", created.ID, "err", err)
	}
	items, err := s.decorate(ctx, a, []db.Item{created})
	if err != nil {
		return Item{}, err
	}
	return items[0], nil
}

func saveClassification(ctx context.Context, q *db.Queries, itemID int64, r classify.Result) error {
	f := func(v float64) *float32 { x := float32(v); return &x }
	err := q.UpsertClassification(ctx, db.UpsertClassificationParams{
		ItemID: itemID, CategorySlug: strPtr(r.CategorySlug), CategoryConf: f(r.CategoryConf),
		PromoConf: f(r.PromoConf), MismatchConf: f(r.MismatchConf), InjectionConf: f(r.InjectionConf),
		UnsafeConf: f(r.UnsafeConf), Kind: strPtr(r.Kind), KindConf: f(r.KindConf), Model: r.Model,
	})
	if err != nil {
		return fmt.Errorf("save classification: %w", err)
	}
	return nil
}

// classifyURL fetches the page through the SSRF-safe client and runs the
// classifier, caching by normalized URL and rate-limiting per user so spam
// cannot run up the bill.
func (s *Service) classifyURL(ctx context.Context, a *Actor, norm, title, blurb string) (classify.Result, error) {
	if r, ok := s.classifyCache.Get(norm + "\x00" + title + "\x00" + blurb); ok {
		return r, nil
	}
	if a != nil {
		if err := s.limit(fmt.Sprintf("classify:%d", a.ID()), classifyRules...); err != nil {
			return classify.Result{}, fmt.Errorf("classify rate limit")
		}
	}
	page, _ := s.fetchPage(ctx, norm, 2*time.Second, 512<<10)
	leaves, err := s.LeafCategories(ctx)
	if err != nil {
		return classify.Result{}, err
	}
	cats := make([]classify.Category, 0, len(leaves))
	for _, l := range leaves {
		cats = append(cats, classify.Category{Slug: l.Slug, Name: l.Name})
	}
	in := classify.Input{URL: norm, Title: title, Blurb: blurb, PageTitle: page.Title,
		MetaDescription: page.Description, PageText: page.Text, Categories: cats}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := s.Classifier.Classify(cctx, in)
	if err != nil {
		return r, err
	}
	// The regex pre-filter runs in front of the classifier: a match counts as
	// a near-certain injection so the policy always holds the item.
	if safety.LooksLikeInjection(page.Title+" "+page.Description+" "+page.Text) && r.InjectionConf < 0.99 {
		r.InjectionConf = 0.99
	}
	if strings.EqualFold(page.OGType, "article") {
		r.PageKind = "article"
	}
	s.classifyCache.Set(norm+"\x00"+title+"\x00"+blurb, r)
	return r, nil
}

// fetchPage downloads a public page with a short timeout and size cap and
// extracts its title, description, and visible text.
func (s *Service) fetchPage(ctx context.Context, u string, timeout time.Duration, maxBytes int64) (classify.Page, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return classify.Page{}, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := s.Fetch.Do(req)
	if err != nil {
		return classify.Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return classify.Page{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "html") {
		return classify.Page{}, fmt.Errorf("not html: %s", ct)
	}
	body, err := safety.ReadCapped(resp.Body, maxBytes)
	if err != nil {
		return classify.Page{}, err
	}
	return classify.ExtractPage(body), nil
}

// Enrichment is what a submit-time lookup suggests for an item. Values are
// suggestions only; the poster can edit them and failure never blocks a post.
type Enrichment struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Creator string `json:"creator"`
}

var isbnRe = regexp.MustCompile(`(?:^|[^0-9])(97[89][0-9]{10}|[0-9]{9}[0-9Xx])(?:[^0-9]|$)`)

// Enrich suggests a title, kind, and creator for a URL using domain rules,
// YouTube oEmbed, Open Library, and the page's meta tags.
func (s *Service) Enrich(ctx context.Context, norm string) (Enrichment, error) {
	e := Enrichment{URL: norm, Kind: classify.KindFor("", norm, classify.Result{})}
	if e.Kind == "other" {
		e.Kind = ""
	}
	if owner, repo, ok := safety.GitHubRepo(norm); ok {
		e.Creator = owner
		e.Title = repo
	}
	if _, ok := safety.YouTubeID(norm); ok {
		var o struct {
			Title      string `json:"title"`
			AuthorName string `json:"author_name"`
		}
		if err := s.getJSON(ctx, "https://www.youtube.com/oembed?format=json&url="+url.QueryEscape(norm), &o); err == nil {
			e.Title, e.Creator = o.Title, o.AuthorName
			return e, nil
		}
	}
	if m := isbnRe.FindStringSubmatch(norm); m != nil {
		var b map[string]struct {
			Title   string `json:"title"`
			Authors []struct {
				Name string `json:"name"`
			} `json:"authors"`
		}
		key := "ISBN:" + strings.ToUpper(m[1])
		if err := s.getJSON(ctx, "https://openlibrary.org/api/books?format=json&jscmd=data&bibkeys="+url.QueryEscape(key), &b); err == nil {
			if book, ok := b[key]; ok {
				e.Kind, e.Title = "book", book.Title
				if len(book.Authors) > 0 {
					e.Creator = book.Authors[0].Name
				}
				return e, nil
			}
		}
	}
	page, err := s.fetchPage(ctx, norm, 3*time.Second, 512<<10)
	if err != nil {
		if e.Title != "" {
			return e, nil
		}
		return e, err
	}
	if e.Title == "" {
		e.Title = firstNonEmpty(page.OGTitle, page.Title)
	}
	if e.Creator == "" {
		e.Creator = page.Author
	}
	if e.Kind == "book" && e.Creator == "" && e.Title != "" {
		var r struct {
			Docs []struct {
				AuthorName []string `json:"author_name"`
			} `json:"docs"`
		}
		if err := s.getJSON(ctx, "https://openlibrary.org/search.json?limit=1&title="+url.QueryEscape(e.Title), &r); err == nil &&
			len(r.Docs) > 0 && len(r.Docs[0].AuthorName) > 0 {
			e.Creator = r.Docs[0].AuthorName[0]
		}
	}
	e.Title = truncateRunes(safety.SanitizeText(e.Title), 120)
	e.Creator = truncateRunes(safety.SanitizeText(e.Creator), 120)
	return e, nil
}

// Preview powers the submit form: it normalizes the URL, reports duplicates,
// and fetches a suggested title server-side through the SSRF-safe client.
func (s *Service) Preview(ctx context.Context, a *Actor, raw string) (Enrichment, error) {
	if err := require(a, "post"); err != nil {
		return Enrichment{}, err
	}
	if err := s.limit(fmt.Sprintf("preview:%d", a.ID()), previewRules...); err != nil {
		return Enrichment{}, err
	}
	norm, _, _, err := s.checkURL(ctx, raw)
	if err != nil {
		return Enrichment{}, err
	}
	e, _ := s.Enrich(ctx, norm)
	e.URL = norm
	return e, nil
}

func (s *Service) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "DogearBot/1.0 (+"+s.Cfg.BaseURL+"/about)")
	resp, err := s.API.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", u, resp.StatusCode)
	}
	b, err := safety.ReadCapped(resp.Body, 1<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// safeBrowsingFlagged checks a URL against the Google Safe Browsing Lookup API.
func (s *Service) safeBrowsingFlagged(ctx context.Context, u string) (bool, error) {
	if s.Cfg.SafeBrowsingAPIKey == "" {
		return false, nil
	}
	body, _ := json.Marshal(map[string]any{
		"client": map[string]string{"clientId": "dogear", "clientVersion": "1.0"},
		"threatInfo": map[string]any{
			"threatTypes":      []string{"MALWARE", "SOCIAL_ENGINEERING", "UNWANTED_SOFTWARE", "POTENTIALLY_HARMFUL_APPLICATION"},
			"platformTypes":    []string{"ANY_PLATFORM"},
			"threatEntryTypes": []string{"URL"},
			"threatEntries":    []map[string]string{{"url": u}},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://safebrowsing.googleapis.com/v4/threatMatches:find?key="+url.QueryEscape(s.Cfg.SafeBrowsingAPIKey), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.API.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("safe browsing status %d", resp.StatusCode)
	}
	var out struct {
		Matches []json.RawMessage `json:"matches"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return len(out.Matches) > 0, nil
}

// RetryClassification re-runs the classifier for items held because of a
// classifier error. It only ever adds flags; it never makes an item live.
func (s *Service) RetryClassification(ctx context.Context) {
	items, err := s.Q.ItemsNeedingClassification(ctx)
	if err != nil {
		s.Log.Error("retry classification", "err", err)
		return
	}
	for _, it := range items {
		res, err := s.classifyURL(ctx, nil, it.UrlNorm, it.Title, it.Blurb)
		if err != nil {
			continue
		}
		cat := ""
		if c, err := s.Q.ItemCategory(ctx, it.ID); err == nil && c.Slug != "unsorted" {
			cat = c.Slug
		}
		dec := classify.Decide(res, cat, s.Thresholds)
		flags := slices.DeleteFunc(slices.Clone(it.Flags), func(f string) bool { return f == "classifier_error" })
		flags = append(flags, dec.Flags...)
		if dec.Reject {
			flags = append(flags, "classifier_reject")
		}
		if err := saveClassification(ctx, s.Q, it.ID, res); err != nil {
			s.Log.Error("save classification", "err", err)
			continue
		}
		if err := s.Q.SetItemFlags(ctx, db.SetItemFlagsParams{ID: it.ID, Flags: nonNil(dedupe(flags))}); err != nil {
			s.Log.Error("set flags", "err", err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func dedupe(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return safety.Truncate(s, n)
}
