package service

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"dogear/internal/db"
)

var reportReasons = []string{"spam", "dead", "offensive", "duplicate", "other"}

// Report files a report on an item. Three open reports from distinct users
// move a live item to pending for review.
func (s *Service) Report(ctx context.Context, a *Actor, itemID int64, reason, note string) error {
	if err := require(a, "vote"); err != nil {
		return err
	}
	if !slices.Contains(reportReasons, reason) {
		return errf("invalid", "reason must be one of spam, dead, offensive, duplicate, other")
	}
	note, err := CleanText("note", note, 0, 500)
	if err != nil {
		return err
	}
	dismissed, err := s.Q.DismissedReports(ctx, a.ID())
	if err != nil {
		return fmt.Errorf("dismissed reports: %w", err)
	}
	if dismissed >= 5 {
		return errf("forbidden", "reporting is disabled for this account")
	}
	if err := s.limit("report:"+strconv.FormatInt(a.ID(), 10), reportRules[level(a.User.TrustLevel)]...); err != nil {
		return err
	}
	it, err := s.Q.GetItem(ctx, itemID)
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get item: %w", err)
	}
	return s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.CreateReport(ctx, db.CreateReportParams{ItemID: itemID, ReporterID: a.ID(), Reason: reason, Note: note}); err != nil {
			return fmt.Errorf("create report: %w", err)
		}
		n, err := q.OpenReportCount(ctx, itemID)
		if err != nil {
			return fmt.Errorf("report count: %w", err)
		}
		if n >= 3 && it.Status == "live" {
			if err := q.SetItemStatus(ctx, db.SetItemStatusParams{ID: itemID, Status: "pending"}); err != nil {
				return fmt.Errorf("hold item: %w", err)
			}
			if err := q.SetItemFlags(ctx, db.SetItemFlagsParams{ID: itemID, Flags: dedupe(append(slices.Clone(it.Flags), "reported"))}); err != nil {
				return fmt.Errorf("flag item: %w", err)
			}
		}
		return nil
	})
}

// ModQueue is everything shown on /mod.
type ModQueue struct {
	Pending []db.PendingItemsRow
	Reports []db.OpenReportsRow
	Flags   []db.OpenUserFlagsRow
	Dead    []db.Item
}

func (s *Service) ModQueue(ctx context.Context, a *Actor) (ModQueue, error) {
	var mq ModQueue
	if !a.IsMod() {
		return mq, ErrNotFound
	}
	var err error
	if mq.Pending, err = s.Q.PendingItems(ctx); err != nil {
		return mq, fmt.Errorf("pending: %w", err)
	}
	if mq.Reports, err = s.Q.OpenReports(ctx); err != nil {
		return mq, fmt.Errorf("reports: %w", err)
	}
	if mq.Flags, err = s.Q.OpenUserFlags(ctx); err != nil {
		return mq, fmt.Errorf("user flags: %w", err)
	}
	if mq.Dead, err = s.Q.DeadItems(ctx); err != nil {
		return mq, fmt.Errorf("dead: %w", err)
	}
	return mq, nil
}

// ModItem applies approve, reject, remove, or restore to an item and logs it.
func (s *Service) ModItem(ctx context.Context, a *Actor, itemID int64, action, note string) error {
	if !a.IsMod() {
		return errf("forbidden", "mods only")
	}
	it, err := s.Q.GetItem(ctx, itemID)
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get item: %w", err)
	}
	var status string
	switch action {
	case "approve", "restore":
		status = "live"
	case "reject", "remove":
		status = "removed"
	default:
		return errf("invalid", "unknown action %q", action)
	}
	err = s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		if err := q.SetItemStatus(ctx, db.SetItemStatusParams{ID: itemID, Status: status}); err != nil {
			return fmt.Errorf("set status: %w", err)
		}
		if status == "live" {
			if err := q.SetItemFlags(ctx, db.SetItemFlagsParams{ID: itemID, Flags: []string{}}); err != nil {
				return fmt.Errorf("clear flags: %w", err)
			}
			if it.LinkStatus == "dead" {
				if err := q.RestoreLink(ctx, itemID); err != nil {
					return fmt.Errorf("restore link: %w", err)
				}
			}
		}
		if action == "approve" && it.Status == "pending" && it.CreatedBy != nil {
			if err := q.IncrementApprovedPosts(ctx, *it.CreatedBy); err != nil {
				return fmt.Errorf("approved posts: %w", err)
			}
		}
		if err := q.MarkDirtyForItem(ctx, itemID); err != nil {
			return fmt.Errorf("mark dirty: %w", err)
		}
		return q.InsertModAction(ctx, db.InsertModActionParams{ModID: ptr(a.ID()), ItemID: &itemID, UserID: it.CreatedBy, Action: action, Note: note})
	})
	if err != nil {
		return err
	}
	if it.CreatedBy != nil {
		if status == "live" {
			s.maybePromote(ctx, *it.CreatedBy)
		} else {
			s.maybeDemote(ctx, *it.CreatedBy)
		}
	}
	return nil
}

// ModReport upholds or dismisses a report. Upholding removes the item.
func (s *Service) ModReport(ctx context.Context, a *Actor, reportID int64, uphold bool) error {
	if !a.IsMod() {
		return errf("forbidden", "mods only")
	}
	status := "dismissed"
	if uphold {
		status = "upheld"
	}
	r, err := s.Q.SetReportStatus(ctx, db.SetReportStatusParams{ID: reportID, Status: status})
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("set report: %w", err)
	}
	if err := s.Q.InsertModAction(ctx, db.InsertModActionParams{ModID: ptr(a.ID()), ItemID: &r.ItemID, Action: "report_" + status}); err != nil {
		return fmt.Errorf("mod action: %w", err)
	}
	if uphold {
		return s.ModItem(ctx, a, r.ItemID, "remove", "report upheld")
	}
	// When every open report is dismissed, a reported item goes back live.
	n, err := s.Q.OpenReportCount(ctx, r.ItemID)
	if err == nil && n == 0 {
		if it, err := s.Q.GetItem(ctx, r.ItemID); err == nil && it.Status == "pending" && slices.Equal(it.Flags, []string{"reported"}) {
			return s.ModItem(ctx, a, r.ItemID, "restore", "reports dismissed")
		}
	}
	return nil
}

// ModUser applies ban, unban, shadowban, unshadowban, promote, demote, or
// clear_flags to a user and logs it. A ban deletes the user's votes and
// recounts the affected items; cascade also bans everyone they invited.
func (s *Service) ModUser(ctx context.Context, a *Actor, username, action string, cascade bool) error {
	if !a.IsMod() {
		return errf("forbidden", "mods only")
	}
	u, err := s.Q.GetUserByUsername(ctx, username)
	if notFound(err) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if u.Role == "admin" && action != "clear_flags" {
		return errf("forbidden", "admins cannot be moderated")
	}
	switch action {
	case "ban":
		if err := s.ban(ctx, u.ID); err != nil {
			return err
		}
		if cascade {
			invited, err := s.Q.InvitedUsers(ctx, &u.ID)
			if err != nil {
				return fmt.Errorf("invited: %w", err)
			}
			for _, id := range invited {
				if err := s.ban(ctx, id); err != nil {
					return err
				}
			}
		}
	case "unban":
		err = s.Q.UnbanUser(ctx, u.ID)
	case "shadowban", "unshadowban":
		err = s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
			if action == "shadowban" {
				if err := q.ShadowBanUser(ctx, u.ID); err != nil {
					return err
				}
				return recountUserVotes(ctx, q, u.ID, false)
			}
			if err := q.UnshadowBanUser(ctx, u.ID); err != nil {
				return err
			}
			return recountUserVotes(ctx, q, u.ID, u.TrustLevel >= 1)
		})
	case "promote":
		err = s.SetTrust(ctx, u.ID, 1)
	case "demote":
		err = s.SetTrust(ctx, u.ID, 0)
	case "clear_flags":
		err = s.Q.ClearUserFlags(ctx, u.ID)
	case "make_mod":
		if a.User.Role != "admin" {
			return errf("forbidden", "admins only")
		}
		err = s.Q.SetUserRole(ctx, db.SetUserRoleParams{ID: u.ID, Role: "mod"})
	default:
		return errf("invalid", "unknown action %q", action)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return s.Q.InsertModAction(ctx, db.InsertModActionParams{ModID: ptr(a.ID()), UserID: &u.ID, Action: action})
}

func (s *Service) ban(ctx context.Context, userID int64) error {
	return s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		ids, err := q.ItemsVotedBy(ctx, userID)
		if err != nil {
			return fmt.Errorf("voted items: %w", err)
		}
		if err := q.BanUser(ctx, userID); err != nil {
			return fmt.Errorf("ban: %w", err)
		}
		if err := q.DeleteUserVotes(ctx, userID); err != nil {
			return fmt.Errorf("delete votes: %w", err)
		}
		return recountItems(ctx, q, ids)
	})
}

// maybePromote promotes a level 0 user who now meets either promotion path.
func (s *Service) maybePromote(ctx context.Context, userID int64) {
	ids, err := s.Q.PromotionCandidates(ctx)
	if err != nil {
		s.Log.Error("promotion candidates", "err", err)
		return
	}
	if slices.Contains(ids, userID) {
		if err := s.SetTrust(ctx, userID, 1); err != nil {
			s.Log.Error("promote", "user", userID, "err", err)
		}
	}
}

// maybeDemote returns a user to level 0 after 2 removed items in 7 days.
func (s *Service) maybeDemote(ctx context.Context, userID int64) {
	n, err := s.Q.RecentRemovals(ctx, &userID)
	if err != nil {
		s.Log.Error("recent removals", "err", err)
		return
	}
	if n >= 2 {
		if u, err := s.Q.GetUser(ctx, userID); err == nil && u.Role == "user" {
			if err := s.SetTrust(ctx, userID, 0); err != nil {
				s.Log.Error("demote", "user", userID, "err", err)
			}
		}
	}
}

// BlockDomain adds a domain to the blocklist (admin tool).
func (s *Service) BlockDomain(ctx context.Context, a *Actor, domain, reason string) error {
	if !a.IsMod() {
		return errf("forbidden", "mods only")
	}
	return s.Q.BlockDomain(ctx, db.BlockDomainParams{Domain: domain, Reason: reason})
}

// Maintenance runs the periodic jobs: automatic promotion, ring detection,
// retention cleanup, and classifier retries. serve calls it on a timer.
func (s *Service) Maintenance(ctx context.Context) {
	if err := s.FileTopics(ctx); err != nil {
		s.Log.Error("file topics", "err", err)
	}
	ids, err := s.Q.PromotionCandidates(ctx)
	if err != nil {
		s.Log.Error("promotion candidates", "err", err)
	}
	for _, id := range ids {
		if err := s.SetTrust(ctx, id, 1); err != nil {
			s.Log.Error("promote", "user", id, "err", err)
		}
	}
	if rings, err := s.Q.SignupRings(ctx); err == nil {
		for _, id := range rings {
			_ = s.Q.FlagUser(ctx, db.FlagUserParams{UserID: id, Reason: "signup_ring"})
		}
	} else {
		s.Log.Error("signup rings", "err", err)
	}
	if pairs, err := s.Q.VoteRings(ctx); err == nil {
		for _, p := range pairs {
			_ = s.Q.FlagUser(ctx, db.FlagUserParams{UserID: p.U1, Reason: "vote_ring"})
			_ = s.Q.FlagUser(ctx, db.FlagUserParams{UserID: p.U2, Reason: "vote_ring"})
		}
	} else {
		s.Log.Error("vote rings", "err", err)
	}
	for _, f := range []func(context.Context) error{s.Q.ClearOldIPHashes, s.Q.DeleteExpiredSessions, s.Q.DeleteOldRevokedTokens} {
		if err := f(ctx); err != nil {
			s.Log.Error("retention", "err", err)
		}
	}
}

// RunBackground runs classifier retries every minute and maintenance hourly
// until ctx is done.
func (s *Service) RunBackground(ctx context.Context) {
	minute := time.NewTicker(time.Minute)
	hour := time.NewTicker(time.Hour)
	defer minute.Stop()
	defer hour.Stop()
	s.Maintenance(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-minute.C:
			s.RetryClassification(ctx)
			s.BackfillImages(ctx, 60)
			s.BackfillTranscripts(ctx, 10)
			s.BackfillMedia(ctx, 10)
		case <-hour.C:
			s.Maintenance(ctx)
		}
	}
}
