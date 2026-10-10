package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"dogear/internal/db"
	"dogear/internal/ranking"
	"dogear/internal/safety"
)

const indexRepo = "sindresorhus/awesome"

// permissiveLicenses are the SPDX ids whose descriptions may be shown as blurbs.
var permissiveLicenses = []string{"CC0-1.0", "Unlicense", "MIT", "Apache-2.0", "CC-BY-4.0", "CC-BY-SA-4.0"}

type Options struct {
	Force bool   // ignore stored README hashes
	Only  string // import one owner/repo list
	Stars bool   // refresh GitHub stars and priors only
}

// Summary is printed when an import finishes.
type Summary struct {
	ListsFound, ListsSkipped, ListsFailed int
	ItemsParsed, ItemsNew, ItemsDeduped   int
	CategoriesCreated, RowsChanged        int
	Skipped                               map[string]int
	Elapsed                               time.Duration
}

func (s Summary) Print(w io.Writer) {
	fmt.Fprintf(w, "lists found:        %d\n", s.ListsFound)
	fmt.Fprintf(w, "lists skipped:      %d (unchanged)\n", s.ListsSkipped)
	fmt.Fprintf(w, "lists failed:       %d\n", s.ListsFailed)
	fmt.Fprintf(w, "items parsed:       %d\n", s.ItemsParsed)
	fmt.Fprintf(w, "items new:          %d\n", s.ItemsNew)
	fmt.Fprintf(w, "items deduped:      %d\n", s.ItemsDeduped)
	keys := slices.Sorted(maps.Keys(s.Skipped))
	for _, k := range keys {
		fmt.Fprintf(w, "items skipped:      %d (%s)\n", s.Skipped[k], k)
	}
	fmt.Fprintf(w, "categories created: %d\n", s.CategoriesCreated)
	fmt.Fprintf(w, "rows changed:       %d\n", s.RowsChanged)
	fmt.Fprintf(w, "elapsed:            %s\n", s.Elapsed.Round(time.Millisecond))
}

type Importer struct {
	Pool *pgxpool.Pool
	GH   *GitHub
	Log  *slog.Logger

	mu  sync.Mutex
	sum Summary
}

func (im *Importer) add(f func(s *Summary)) {
	im.mu.Lock()
	defer im.mu.Unlock()
	f(&im.sum)
}

// Run imports the awesome index. It is idempotent: a second run on unchanged
// data changes zero rows.
func (im *Importer) Run(ctx context.Context, opt Options) (Summary, error) {
	start := time.Now()
	im.sum = Summary{Skipped: map[string]int{}}
	q := db.New(im.Pool)
	if !opt.Stars {
		if err := im.importLists(ctx, q, opt); err != nil {
			return im.sum, err
		}
	}
	if err := im.seedPriors(ctx, q); err != nil {
		return im.sum, err
	}
	im.sum.Elapsed = time.Since(start)
	return im.sum, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// listSlug turns "avelino/awesome-go" into "go"; a collision with an
// existing different list falls back to the full owner-repo slug.
func listSlug(repo string) string {
	name := repo[strings.Index(repo, "/")+1:]
	name = strings.TrimPrefix(strings.TrimPrefix(name, "awesome-"), "awesome")
	if s := slugify(name); s != "" {
		return s
	}
	return slugify(repo)
}

func (im *Importer) importLists(ctx context.Context, q *db.Queries, opt Options) error {
	md, _, err := im.GH.Readme(ctx, indexRepo)
	if err != nil {
		return fmt.Errorf("fetch index: %w", err)
	}
	cats := ParseIndex(md)
	type job struct {
		list  IndexList
		catID int32
	}
	var jobs []job
	seen := map[string]bool{}
	usedSlugs := map[string]string{}
	for i, c := range cats {
		parent, err := q.UpsertCategory(ctx, db.UpsertCategoryParams{Slug: slugify(c.Name), Name: c.Name, SortOrder: int32(i)})
		if err != nil {
			return fmt.Errorf("category %q: %w", c.Name, err)
		}
		if parent.Inserted {
			im.sum.CategoriesCreated++
		}
		for j, l := range c.Lists {
			if seen[l.Repo] || (opt.Only != "" && !strings.EqualFold(opt.Only, l.Repo)) {
				continue
			}
			seen[l.Repo] = true
			slug := listSlug(l.Repo)
			if other, ok := usedSlugs[slug]; ok && other != l.Repo {
				slug = slugify(l.Repo)
			}
			// A list slug may collide with a top-level category slug.
			if existing, err := q.GetCategory(ctx, slug); err == nil && existing.ParentID == nil {
				slug = slugify(l.Repo)
			}
			usedSlugs[slug] = l.Repo
			title := strings.TrimSpace(l.Title)
			if title == "" {
				title = l.Repo
			}
			if len(title) > 80 {
				title = title[:80]
			}
			sub, err := q.UpsertCategory(ctx, db.UpsertCategoryParams{Slug: slug, Name: title, ParentID: &parent.ID, SortOrder: int32(j)})
			if err != nil {
				return fmt.Errorf("subcategory %q: %w", slug, err)
			}
			if sub.Inserted {
				im.sum.CategoriesCreated++
			}
			jobs = append(jobs, job{list: l, catID: sub.ID})
		}
	}
	im.sum.ListsFound = len(jobs)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, j := range jobs {
		g.Go(func() error {
			if err := im.importList(gctx, j.list, j.catID, opt.Force); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				im.Log.Warn("list failed", "repo", j.list.Repo, "err", err)
				im.add(func(s *Summary) { s.ListsFailed++ })
			}
			return nil
		})
	}
	return g.Wait()
}

// importList fetches one awesome list and upserts its items in a single
// transaction. Unchanged READMEs (same sha) are skipped unless forced.
func (im *Importer) importList(ctx context.Context, l IndexList, catID int32, force bool) error {
	q := db.New(im.Pool)
	repo, err := im.GH.Repo(ctx, l.Repo)
	if err != nil {
		return fmt.Errorf("repo meta: %w", err)
	}
	existing, err := q.GetSourceList(ctx, l.Repo)
	hasExisting := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("source list: %w", err)
	}
	md, sha, err := im.GH.Readme(ctx, l.Repo)
	if err != nil {
		return fmt.Errorf("readme: %w", err)
	}
	var license *string
	if repo.License != "" {
		license = &repo.License
	}
	stars := int32(repo.Stars)
	title := l.Title
	if title == "" {
		title = l.Repo
	}

	tx, err := im.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tq := q.WithTx(tx)
	srcID, err := tq.UpsertSourceList(ctx, db.UpsertSourceListParams{Repo: l.Repo, Title: title, CategoryID: &catID, License: license, Stars: &stars})
	changed := 0
	switch {
	case err == nil:
		if hasExisting {
			changed++
		}
	case errors.Is(err, pgx.ErrNoRows):
		srcID = existing.ID // nothing changed on the source row
	default:
		return fmt.Errorf("upsert source: %w", err)
	}
	if !force && hasExisting && existing.ReadmeSha != nil && *existing.ReadmeSha == sha {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		im.add(func(s *Summary) { s.ListsSkipped++; s.RowsChanged += changed })
		return nil
	}

	items, skips := ParseList(md)
	permissive := slices.Contains(permissiveLicenses, repo.License)
	var touched []int64
	var parsed, created, deduped int
	extra := map[string]int{}
	for _, it := range items {
		parsed++
		norm, err := safety.NormalizeURL(it.URL)
		if err != nil {
			extra["bad_url"]++
			continue
		}
		domain := safety.Domain(norm)
		if safety.IsShortener(domain) {
			extra["shortener"]++
			continue
		}
		kind := "site"
		if _, _, ok := safety.GitHubRepo(norm); ok {
			kind = "tool"
		}
		blurb := ""
		if permissive {
			blurb = safety.Truncate(safety.SanitizeText(it.Description), 280)
		}
		itemTitle := safety.Truncate(safety.SanitizeText(it.Title), 120)
		if itemTitle == "" {
			extra["no_title"]++
			continue
		}
		id, err := tq.InsertImportedItem(ctx, db.InsertImportedItemParams{Url: norm, UrlNorm: norm, Domain: domain, Title: itemTitle, Blurb: blurb, Kind: kind})
		if errors.Is(err, pgx.ErrNoRows) {
			ex, err := tq.ImportedItemByNorm(ctx, norm)
			if err != nil {
				return fmt.Errorf("existing item: %w", err)
			}
			id = ex.ID
			deduped++
			// Never overwrite user-posted or voted items; fill in a blurb
			// for imported ones when this list is permissive and it lacked one.
			if ex.Via == "import" && ex.Blurb == "" && blurb != "" {
				n, err := tq.UpdateImportedText(ctx, db.UpdateImportedTextParams{ID: id, Title: ex.Title, Blurb: blurb})
				if err != nil {
					return fmt.Errorf("update text: %w", err)
				}
				changed += int(n)
			}
		} else if err != nil {
			return fmt.Errorf("insert item %s: %w", norm, err)
		} else {
			created++
			changed++
		}
		n, err := tq.InsertItemSource(ctx, db.InsertItemSourceParams{ItemID: id, SourceListID: srcID,
			HeadingPath: safety.Truncate(it.HeadingPath, 300), OriginalDescription: it.Description})
		if err != nil {
			return fmt.Errorf("item source: %w", err)
		}
		changed += int(n)
		n, err = tq.InsertItemCategory(ctx, db.InsertItemCategoryParams{ItemID: id, CategoryID: catID})
		if err != nil {
			return fmt.Errorf("item category: %w", err)
		}
		changed += int(n)
		touched = append(touched, id)
	}
	if len(touched) > 0 {
		n, err := tq.RecountListCount(ctx, touched)
		if err != nil {
			return fmt.Errorf("list count: %w", err)
		}
		changed += int(n)
	}
	if err := tq.MarkSourceImported(ctx, db.MarkSourceImportedParams{ID: srcID, ReadmeSha: &sha}); err != nil {
		return fmt.Errorf("mark imported: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	im.add(func(s *Summary) {
		s.ItemsParsed += parsed
		s.ItemsNew += created
		s.ItemsDeduped += deduped
		s.RowsChanged += changed
		for k, v := range skips {
			s.Skipped[k] += v
		}
		for k, v := range extra {
			s.Skipped[k] += v
		}
	})
	im.Log.Info("imported list", "repo", l.Repo, "items", parsed, "new", created)
	return nil
}

// seedPriors fetches stars for every imported GitHub repo item, then
// recomputes prior_score and score for all imported items (the rescore helper).
func (im *Importer) seedPriors(ctx context.Context, q *db.Queries) error {
	ghItems, err := q.ImportedGitHubItems(ctx)
	if err != nil {
		return fmt.Errorf("github items: %w", err)
	}
	byRepo := map[string][]int64{}
	for _, it := range ghItems {
		if owner, repo, ok := safety.GitHubRepo(it.UrlNorm); ok {
			key := strings.ToLower(owner + "/" + repo)
			byRepo[key] = append(byRepo[key], it.ID)
		}
	}
	repos := slices.Collect(maps.Keys(byRepo))
	sort.Strings(repos)
	if len(repos) > 0 {
		stars, err := im.GH.Stars(ctx, repos)
		if err != nil {
			im.Log.Warn("stars", "err", err)
		}
		for repo, n := range stars {
			for _, id := range byRepo[repo] {
				if err := q.SetStars(ctx, db.SetStarsParams{ID: id, GithubStars: ptr32(int32(n))}); err != nil {
					return fmt.Errorf("set stars: %w", err)
				}
			}
		}
	}
	return Rescore(ctx, q, &im.sum.RowsChanged)
}

// Rescore recomputes prior_score and score for imported items, writing only
// rows whose values changed.
func Rescore(ctx context.Context, q *db.Queries, changed *int) error {
	rows, err := q.ImportedItemsForRescore(ctx)
	if err != nil {
		return fmt.Errorf("rescore rows: %w", err)
	}
	for _, r := range rows {
		var stars *int
		if r.GithubStars != nil {
			n := int(*r.GithubStars)
			stars = &n
		}
		prior := float32(ranking.Prior(stars, int(r.ListCount)))
		score := ranking.Score(int(r.LikeCount), int(r.DislikeCount), float64(prior), true)
		if prior == r.PriorScore && abs(score-r.Score) < 1e-9 {
			continue
		}
		if err := q.SetPriorAndScore(ctx, db.SetPriorAndScoreParams{ID: r.ID, PriorScore: prior, Score: score}); err != nil {
			return fmt.Errorf("set score: %w", err)
		}
		if changed != nil {
			*changed++
		}
	}
	return nil
}

func ptr32(v int32) *int32 { return &v }

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
