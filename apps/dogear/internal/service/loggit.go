package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"dogear/internal/db"
	"dogear/internal/loggit"
	"dogear/internal/safety"
)

// LoggitEnabled reports whether the loggit integration is configured.
func (s *Service) LoggitEnabled() bool { return s.Loggit.Enabled() }

// tokenAEAD returns the AES-GCM cipher used to encrypt loggit tokens at rest.
// The key is derived from SESSION_SECRET, so rotating that secret means
// users reconnect loggit.
func (s *Service) tokenAEAD() (cipher.AEAD, error) {
	key := sha256.Sum256(append([]byte("dogear-token-enc|"), s.Cfg.SessionSecret...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Service) seal(plain string) ([]byte, error) {
	aead, err := s.tokenAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (s *Service) open(sealed []byte) (string, error) {
	aead, err := s.tokenAEAD()
	if err != nil {
		return "", err
	}
	if len(sealed) < aead.NonceSize() {
		return "", errors.New("sealed token too short")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	return string(plain), err
}

// LoggitConnect stores a freshly issued loggit token pair for the user.
func (s *Service) LoggitConnect(ctx context.Context, userID int64, t loggit.Token) error {
	acc, err := s.seal(t.AccessToken)
	if err != nil {
		return err
	}
	ref, err := s.seal(t.RefreshToken)
	if err != nil {
		return err
	}
	return s.Q.UpsertLoggitAccount(ctx, db.UpsertLoggitAccountParams{
		UserID: userID, LoggitUserID: t.User.ID, LoggitUsername: t.User.Username,
		AccessTokenEnc: acc, RefreshTokenEnc: ref,
		AccessExpiresAt: time.Now().Add(time.Duration(max(t.ExpiresIn, 60)) * time.Second), Scope: t.Scope,
	})
}

// LoggitAccount returns the user's connected loggit account, or nil.
func (s *Service) LoggitAccount(ctx context.Context, userID int64) *db.LoggitAccount {
	a, err := s.Q.GetLoggitAccount(ctx, userID)
	if err != nil {
		return nil
	}
	return &a
}

// LoggitDisconnect revokes the tokens, forgets the account, and unlinks all
// of the user's lists. Lists themselves stay on both sides.
func (s *Service) LoggitDisconnect(ctx context.Context, userID int64) error {
	if a := s.LoggitAccount(ctx, userID); a != nil {
		if ref, err := s.open(a.RefreshTokenEnc); err == nil {
			_ = s.Loggit.Revoke(ctx, ref)
		}
	}
	if err := s.Q.DeleteUserListLinks(ctx, userID); err != nil {
		return err
	}
	return s.Q.DeleteLoggitAccount(ctx, userID)
}

var errLoggitReconnect = errors.New("loggit access expired; reconnect loggit in settings")

// loggitAccess returns a valid access token, refreshing it when it is about
// to expire.
func (s *Service) loggitAccess(ctx context.Context, userID int64) (string, error) {
	a, err := s.Q.GetLoggitAccount(ctx, userID)
	if err != nil {
		return "", errLoggitReconnect
	}
	if time.Until(a.AccessExpiresAt) > time.Minute {
		return s.open(a.AccessTokenEnc)
	}
	ref, err := s.open(a.RefreshTokenEnc)
	if err != nil {
		return "", errLoggitReconnect
	}
	t, err := s.Loggit.Refresh(ctx, ref)
	if errors.Is(err, loggit.ErrUnauthorized) {
		return "", errLoggitReconnect
	} else if err != nil {
		return "", err
	}
	if t.User.ID == "" {
		t.User = loggit.User{ID: a.LoggitUserID, Username: a.LoggitUsername}
	}
	if err := s.LoggitConnect(ctx, userID, t); err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// LoggitLists returns the user's loggit lists for the link picker, minus
// ones already linked to another Dogear list.
func (s *Service) LoggitLists(ctx context.Context, a *Actor) ([]loggit.List, error) {
	tok, err := s.loggitAccess(ctx, a.ID())
	if err != nil {
		return nil, err
	}
	lists, err := s.Loggit.Lists(ctx, tok)
	if err != nil {
		return nil, err
	}
	linked, _ := s.Q.LinkedLoggitListIDs(ctx, a.ID())
	return slices.DeleteFunc(lists, func(l loggit.List) bool { return slices.Contains(linked, l.ID) || l.DeletedAt != nil }), nil
}

// ListLink is a Dogear list's loggit link as shown on the list page.
type ListLink struct {
	db.ListLink
	URL string
}

func (s *Service) ListLinkFor(ctx context.Context, listID int64) *ListLink {
	l, err := s.Q.GetListLink(ctx, listID)
	if err != nil {
		return nil
	}
	return &ListLink{ListLink: l, URL: strings.TrimRight(s.Cfg.LoggitWebURL, "/") + "/lists/" + l.LoggitListID}
}

// LinkList connects a Dogear list to a loggit list ("new" creates one from
// the Dogear list) and runs the first sync, which unions both sides.
func (s *Service) LinkList(ctx context.Context, a *Actor, slug, loggitListID string) error {
	if err := require(a, ""); err != nil {
		return err
	}
	l, err := s.Q.GetList(ctx, db.GetListParams{UserID: a.ID(), Slug: slug})
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := s.Q.GetListLink(ctx, l.ID); err == nil {
		return errf("invalid", "this list is already linked to loggit")
	}
	tok, err := s.loggitAccess(ctx, a.ID())
	if err != nil {
		return errf("invalid", "%s", err.Error())
	}
	if loggitListID == "new" {
		title, desc, vis := truncateRunes(l.Title, 80), l.Description, loggitVisibility(l.Visibility)
		created, err := s.Loggit.CreateList(ctx, tok, loggit.ListInput{Title: &title, Description: &desc, Visibility: &vis})
		if err != nil {
			return fmt.Errorf("create loggit list: %w", err)
		}
		loggitListID = created.ID
	}
	if err := s.Q.CreateListLink(ctx, db.CreateListLinkParams{ListID: l.ID, UserID: a.ID(), LoggitListID: loggitListID}); err != nil {
		if isUniqueViolation(err) {
			return errf("invalid", "that loggit list is already linked to another of your lists")
		}
		return err
	}
	link, err := s.Q.GetListLink(ctx, l.ID)
	if err != nil {
		return err
	}
	return s.SyncLink(ctx, link)
}

// UnlinkList stops syncing a list. Both lists keep their entries.
func (s *Service) UnlinkList(ctx context.Context, a *Actor, slug string) error {
	l, err := s.Q.GetList(ctx, db.GetListParams{UserID: a.ID(), Slug: slug})
	if err != nil {
		return ErrNotFound
	}
	return s.Q.DeleteListLink(ctx, l.ID)
}

// SyncListNow syncs one of the actor's linked lists immediately.
func (s *Service) SyncListNow(ctx context.Context, a *Actor, slug string) error {
	l, err := s.Q.GetList(ctx, db.GetListParams{UserID: a.ID(), Slug: slug})
	if err != nil {
		return ErrNotFound
	}
	link, err := s.Q.GetListLink(ctx, l.ID)
	if err != nil {
		return ErrNotFound
	}
	if link.Status != "active" {
		_ = s.Q.SetListLinkError(ctx, db.SetListLinkErrorParams{ListID: l.ID, LastError: "", Status: "active"})
	}
	return s.SyncLink(ctx, link)
}

// markLinkDirty asks the sync worker to sync a list soon after a local edit.
func (s *Service) markLinkDirty(ctx context.Context, listID int64) {
	_ = s.Q.MarkListLinkDirty(ctx, listID)
}

// RunLoggitSync syncs dirty or stale links every 30 seconds until ctx ends.
func (s *Service) RunLoggitSync(ctx context.Context) {
	if !s.LoggitEnabled() {
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			links, err := s.Q.ListLinksToSync(ctx, 25)
			if err != nil {
				s.Log.Error("loggit links", "err", err)
				continue
			}
			for _, l := range links {
				if err := s.SyncLink(ctx, l); err != nil {
					s.Log.Warn("loggit sync", "list", l.ListID, "err", err)
				}
			}
		}
	}
}

func loggitVisibility(v string) string {
	if v == "public" {
		return "public"
	}
	return "private"
}

type localEntry struct {
	itemID   int64
	note     string
	position int32
	updated  time.Time
}

// SyncLink runs one three-way merge between a Dogear list and its loggit
// list and applies the result to both sides.
func (s *Service) SyncLink(ctx context.Context, link db.ListLink) (err error) {
	defer func() {
		if err != nil {
			status := "active"
			if errors.Is(err, loggit.ErrNotFound) || errors.Is(err, errLoggitReconnect) {
				status = "broken"
			}
			msg := err.Error()
			if errors.Is(err, loggit.ErrNotFound) {
				msg = "the loggit list no longer exists"
			}
			_ = s.Q.SetListLinkError(context.WithoutCancel(ctx), db.SetListLinkErrorParams{ListID: link.ListID, LastError: msg, Status: status})
		}
	}()
	if u, err := s.Q.GetUser(ctx, link.UserID); err != nil || u.BannedAt != nil {
		return errLoggitReconnect // banned accounts stop syncing
	}
	tok, err := s.loggitAccess(ctx, link.UserID)
	if err != nil {
		return err
	}
	remote, err := s.Loggit.GetList(ctx, tok, link.LoggitListID)
	if err != nil {
		return err
	}
	if remote.DeletedAt != nil {
		return loggit.ErrNotFound
	}
	list, err := s.Q.GetListByID(ctx, link.ListID)
	if err != nil {
		return err
	}
	rows, err := s.Q.ListEntryTimes(ctx, link.ListID)
	if err != nil {
		return err
	}
	media, err := s.mediaForItems(ctx, tok, rows)
	if err != nil {
		return err
	}

	// Build both snapshots keyed by loggit media id.
	localByKey := map[string]localEntry{}
	local := loggit.Side{Entries: map[string]loggit.SideEntry{}, MetaUpdated: list.UpdatedAt,
		Meta: loggit.Meta{Title: list.Title, Description: list.Description, Visibility: loggitVisibility(list.Visibility)}}
	for _, r := range rows {
		k := media[r.ItemID]
		if k == "" || localByKey[k].itemID != 0 {
			continue // Dogear-only item, or a second item for the same work
		}
		localByKey[k] = localEntry{itemID: r.ItemID, note: r.Note, position: r.Position, updated: r.UpdatedAt}
		local.Entries[k] = loggit.SideEntry{Note: r.Note, Updated: r.UpdatedAt}
		local.Order = append(local.Order, k)
	}
	remoteByKey := map[string]loggit.Entry{}
	rs := loggit.Side{Entries: map[string]loggit.SideEntry{}, MetaUpdated: remote.UpdatedAt,
		Meta: loggit.Meta{Title: remote.Title, Description: truncateRunes(remote.Description, 280), Visibility: remote.Visibility}}
	for _, e := range remote.Entries {
		if e.DeletedAt != nil {
			continue
		}
		remoteByKey[e.MediaID] = e
		rs.Entries[e.MediaID] = loggit.SideEntry{Note: truncateRunes(e.Note, 280), Updated: e.UpdatedAt}
		rs.Order = append(rs.Order, e.MediaID)
	}
	var base loggit.Baseline
	_ = json.Unmarshal(link.Baseline, &base)

	plan := loggit.Merge(base, local, rs)
	if !plan.Empty() {
		if err := s.applyPlan(ctx, tok, link, list, &plan, localByKey, remoteByKey); err != nil {
			return err
		}
	}
	next, _ := json.Marshal(plan.Next)
	return s.Q.SaveListLinkSync(ctx, db.SaveListLinkSyncParams{ListID: link.ListID, Baseline: next})
}

// mediaForItems maps Dogear items to loggit media ids, resolving unknown
// items through loggit (results, including "not representable", are cached;
// negative results are retried after a week). Unmapped items get "".
func (s *Service) mediaForItems(ctx context.Context, tok string, rows []db.ListEntryTimesRow) (map[int64]string, error) {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ItemID
	}
	cached, err := s.Q.GetItemMedia(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	known := map[int64]bool{}
	for _, c := range cached {
		if c.MediaID != nil {
			out[c.ItemID] = *c.MediaID
			known[c.ItemID] = true
		} else if time.Since(c.ResolvedAt) < 7*24*time.Hour {
			known[c.ItemID] = true
		}
	}
	resolved := 0
	for _, id := range ids {
		if known[id] || resolved >= 25 { // bound remote calls per sync
			continue
		}
		it, err := s.Q.GetItem(ctx, id)
		if err != nil {
			continue
		}
		resolved++
		ref, ok, err := s.Loggit.Resolve(ctx, tok, it.Url)
		if err != nil {
			return nil, err
		}
		var mid *string
		if ok {
			mid = &ref.ID
			out[id] = ref.ID
		}
		if err := s.Q.SetItemMedia(ctx, db.SetItemMediaParams{ItemID: id, MediaID: mid}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// applyPlan writes a merge plan to both sides. Local writes go straight to
// the database so they don't mark the link dirty again.
func (s *Service) applyPlan(ctx context.Context, tok string, link db.ListLink, list db.List, p *loggit.Plan,
	localByKey map[string]localEntry, remoteByKey map[string]loggit.Entry) error {
	owner, err := s.Q.GetUser(ctx, link.UserID)
	if err != nil {
		return err
	}
	// Remote → local. Entries past the per-run cap, or that loggit maps to a
	// non-catalog URL, are skipped; they are retried on later runs because
	// they stay out of the baseline.
	created := 0
	for _, k := range p.LocalAdd {
		if created >= maxSyncCreates {
			delete(p.Next.Entries, k)
			continue
		}
		created++
		e := remoteByKey[k]
		itemID, err := s.ensureItemForMedia(ctx, owner, e.Media)
		if err != nil {
			s.Log.Warn("loggit sync skip", "list", link.ListID, "media", e.MediaID, "err", err)
			delete(p.Next.Entries, k)
			continue
		}
		note := truncateRunes(safety.SanitizeText(e.Note), 280)
		if err := s.Q.InsertListEntryAtEnd(ctx, db.InsertListEntryAtEndParams{ListID: link.ListID, ItemID: itemID, Note: note}); err != nil {
			return err
		}
		localByKey[k] = localEntry{itemID: itemID, note: note}
	}
	for _, k := range p.LocalDelete {
		if err := s.Q.DeleteListEntry(ctx, db.DeleteListEntryParams{ListID: link.ListID, ItemID: localByKey[k].itemID}); err != nil {
			return err
		}
	}
	for k, note := range p.LocalNote {
		if err := s.Q.SetEntryNote(ctx, db.SetEntryNoteParams{ListID: link.ListID, ItemID: localByKey[k].itemID,
			Note: truncateRunes(safety.SanitizeText(note), 280)}); err != nil {
			return err
		}
	}
	if p.LocalMeta != nil {
		vis := p.LocalMeta.Visibility
		if vis == "private" && list.Visibility == "unlisted" {
			vis = "unlisted"
		}
		title := truncateRunes(safety.SanitizeText(p.LocalMeta.Title), 80)
		if title == "" {
			title = list.Title
		}
		if err := s.Q.SetListFields(ctx, db.SetListFieldsParams{ID: link.ListID, Title: title,
			Description: truncateRunes(safety.SanitizeText(p.LocalMeta.Description), 280), Visibility: vis}); err != nil {
			return err
		}
	}
	if p.LocalOrder != nil {
		if err := s.applyLocalOrder(ctx, link.ListID, p.LocalOrder, localByKey); err != nil {
			return err
		}
	}

	// Local → remote.
	for _, k := range p.RemoteAdd {
		e, err := s.Loggit.AddEntry(ctx, tok, link.LoggitListID, k, truncateRunes(localByKey[k].note, 280))
		var dup *loggit.DuplicateError
		if errors.As(err, &dup) {
			e = loggit.Entry{ID: dup.EntryID, MediaID: k}
		} else if err != nil {
			return fmt.Errorf("add to loggit: %w", err)
		}
		remoteByKey[k] = e
	}
	for _, k := range p.RemoteDelete {
		if err := s.Loggit.DeleteEntry(ctx, tok, link.LoggitListID, remoteByKey[k].ID); err != nil {
			return err
		}
	}
	for k, note := range p.RemoteNote {
		if err := s.Loggit.UpdateEntryNote(ctx, tok, link.LoggitListID, remoteByKey[k].ID, note); err != nil {
			return err
		}
	}
	if p.RemoteMeta != nil {
		m := *p.RemoteMeta
		if _, err := s.Loggit.UpdateList(ctx, tok, link.LoggitListID, loggit.ListInput{Title: &m.Title, Description: &m.Description, Visibility: &m.Visibility}); err != nil {
			return err
		}
	}
	if p.RemoteOrder != nil {
		var ids []string
		for _, k := range p.RemoteOrder {
			if e, ok := remoteByKey[k]; ok && e.ID != "" {
				ids = append(ids, e.ID)
			}
		}
		if err := s.Loggit.SetOrder(ctx, tok, link.LoggitListID, ids); err != nil {
			return err
		}
	}
	return nil
}

// applyLocalOrder puts the synced entries in the agreed order while keeping
// Dogear-only entries in the slots they already occupy.
func (s *Service) applyLocalOrder(ctx context.Context, listID int64, order []string, byKey map[string]localEntry) error {
	rows, err := s.Q.ListEntryTimes(ctx, listID)
	if err != nil {
		return err
	}
	synced := map[int64]bool{}
	var want []int64
	for _, k := range order {
		if e, ok := byKey[k]; ok {
			want = append(want, e.itemID)
			synced[e.itemID] = true
		}
	}
	next := 0
	for i, r := range rows {
		id := r.ItemID
		if synced[id] && next < len(want) {
			id = want[next]
			next++
		}
		if err := s.Q.SetEntryPosition(ctx, db.SetEntryPositionParams{ListID: listID, ItemID: id, Position: int32(i)}); err != nil {
			return err
		}
	}
	return nil
}

// loggitCatalogDomains are the only sites a loggit work's URL may point to.
// Items created from sync skip the posting form, so anything else (a
// malformed or malicious loggit response) is refused rather than published.
var loggitCatalogDomains = []string{"themoviedb.org", "boardgamegeek.com", "igdb.com", "openlibrary.org",
	"books.google.com", "useloggit.app"}

// maxSyncCreates bounds how many new Dogear items one sync run may create.
const maxSyncCreates = 50

func catalogDomain(domain string) bool {
	for _, d := range loggitCatalogDomains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

// mediaKinds maps loggit media types to Dogear kinds and categories.
var mediaKinds = map[string][2]string{
	"movie": {"video", "film-tv"}, "tv": {"video", "film-tv"},
	"book": {"book", "books-shelf"}, "videogame": {"app", "games-shelf"}, "boardgame": {"other", "games-shelf"},
}

// ensureItemForMedia finds the Dogear item for a loggit work by its
// canonical URL, creating it (under the list owner, through the normal
// trust rules) when Dogear doesn't have it yet.
func (s *Service) ensureItemForMedia(ctx context.Context, owner db.User, m loggit.MediaRef) (int64, error) {
	if id, err := s.Q.ItemForMedia(ctx, &m.ID); err == nil {
		return id, nil
	}
	raw := m.URL
	if raw == "" {
		raw = m.LoggitURL
	}
	norm, err := safety.NormalizeURL(raw)
	if err != nil {
		return 0, fmt.Errorf("bad media url %q", raw)
	}
	domain := safety.Domain(norm)
	if !catalogDomain(domain) {
		return 0, fmt.Errorf("refusing non-catalog url from loggit: %s", domain)
	}
	if blocked, err := s.Q.DomainBlocked(ctx, domain); err != nil || blocked {
		return 0, fmt.Errorf("blocked domain %s", domain)
	}
	if it, err := s.Q.GetItemByNorm(ctx, norm); err == nil {
		_ = s.Q.SetItemMedia(ctx, db.SetItemMediaParams{ItemID: it.ID, MediaID: &m.ID})
		return it.ID, nil
	}
	kind, catSlug := "other", "film-tv"
	if v, ok := mediaKinds[m.Type]; ok {
		kind, catSlug = v[0], v[1]
	}
	title := truncateRunes(safety.SanitizeText(m.Title), 120)
	if title == "" {
		title = "Untitled"
	}
	status, flags := "live", []string{"from_loggit"}
	if owner.TrustLevel < 1 || owner.ShadowBannedAt != nil {
		status = "pending"
	}
	if safety.LooksLikeInjection(title) {
		status, flags = "pending", append(flags, "injection_pattern")
	}
	var created db.Item
	err = s.inTxQ(ctx, func(q *db.Queries) error {
		created, err = q.InsertItem(ctx, db.InsertItemParams{Url: norm, UrlNorm: norm, Domain: safety.Domain(norm),
			Title: title, CreatedBy: &owner.ID, Via: "api", Status: status, Kind: kind, Flags: flags})
		if err != nil {
			return err
		}
		if c, err := q.GetCategory(ctx, catSlug); err == nil {
			if err := q.AddItemCategory(ctx, db.AddItemCategoryParams{ItemID: created.ID, CategoryID: c.ID}); err != nil {
				return err
			}
		}
		return q.SetItemMedia(ctx, db.SetItemMediaParams{ItemID: created.ID, MediaID: &m.ID})
	})
	if err != nil {
		if isUniqueViolation(err) {
			if it, err := s.Q.GetItemByNorm(ctx, norm); err == nil {
				return it.ID, nil
			}
		}
		return 0, err
	}
	return created.ID, nil
}

// UnmappedItems returns which of a list's items loggit cannot represent, so
// the list page can label them "not on loggit".
func (s *Service) UnmappedItems(ctx context.Context, items []int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := s.Q.GetItemMedia(ctx, items)
	if err != nil {
		return out
	}
	for _, r := range rows {
		if r.MediaID == nil {
			out[r.ItemID] = true
		}
	}
	return out
}
