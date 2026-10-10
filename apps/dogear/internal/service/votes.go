package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dogear/internal/db"
	"dogear/internal/ranking"
)

const (
	maxCountedVotesPerDay      = 200
	maxCountedAgentVotesPerDay = 30
)

// VoteState is what the vote buttons render after a change.
type VoteState struct {
	ItemID int64 `json:"item_id"`
	Likes  int   `json:"likes"`
	MyVote int   `json:"my_vote"`
}

// Vote sets the actor's vote on an item to +1 or -1, or removes it when value
// is 0. Counters and score change in the same transaction under a row lock on
// the item, so concurrent votes never lose updates.
func (s *Service) Vote(ctx context.Context, a *Actor, itemID int64, value int) (VoteState, error) {
	if err := require(a, "vote"); err != nil {
		return VoteState{}, err
	}
	if value != 1 && value != -1 && value != 0 {
		return VoteState{}, errf("invalid", "value must be 1 or -1")
	}
	if err := s.limit("vote:"+strconv.FormatInt(a.ID(), 10), voteRules[level(a.User.TrustLevel)]...); err != nil {
		return VoteState{}, err
	}
	// A vote counts only for trusted, non-shadow-banned users under the daily caps.
	counted := a.User.TrustLevel >= 1 && a.User.ShadowBannedAt == nil
	if counted && value != 0 {
		n, err := s.Q.CountedVotesToday(ctx, a.ID())
		if err != nil {
			return VoteState{}, fmt.Errorf("votes today: %w", err)
		}
		if n >= maxCountedVotesPerDay {
			counted = false
		}
		if a.TokenID != nil && counted {
			n, err := s.Q.CountedAgentVotesToday(ctx, a.ID())
			if err != nil {
				return VoteState{}, fmt.Errorf("agent votes today: %w", err)
			}
			if n >= maxCountedAgentVotesPerDay {
				counted = false
			}
		}
	}
	via := a.Via
	if via == "" {
		via = "web"
	}
	var st VoteState
	err := s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		it, err := q.GetItemForUpdate(ctx, itemID)
		if notFound(err) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("lock item: %w", err)
		}
		if it.Status != "live" {
			return errf("invalid", "only live items can be voted on")
		}
		likes, dislikes := int(it.LikeCount), int(it.DislikeCount)
		old, err := q.GetVote(ctx, db.GetVoteParams{UserID: a.ID(), ItemID: itemID})
		switch {
		case err == nil:
			if old.Counted {
				if old.Value == 1 {
					likes--
				} else {
					dislikes--
				}
			}
		case !notFound(err):
			return fmt.Errorf("get vote: %w", err)
		}
		if value == 0 {
			if err := q.DeleteVote(ctx, db.DeleteVoteParams{UserID: a.ID(), ItemID: itemID}); err != nil {
				return fmt.Errorf("delete vote: %w", err)
			}
		} else {
			// Changing an already-counted vote keeps it counted even past the cap.
			c := counted || (err == nil && old.Counted)
			if err := q.UpsertVote(ctx, db.UpsertVoteParams{UserID: a.ID(), ItemID: itemID, Value: int16(value), Counted: c, Via: via}); err != nil {
				return fmt.Errorf("upsert vote: %w", err)
			}
			if c {
				if value == 1 {
					likes++
				} else {
					dislikes++
				}
			}
		}
		score := ranking.Score(likes, dislikes, float64(it.PriorScore), it.Via == "import")
		if err := q.SetItemCounts(ctx, db.SetItemCountsParams{ID: itemID, LikeCount: int32(likes), DislikeCount: int32(dislikes), Score: score}); err != nil {
			return fmt.Errorf("set counts: %w", err)
		}
		st = VoteState{ItemID: itemID, Likes: likes, MyVote: value}
		return nil
	})
	return st, err
}

// recountItems recomputes like/dislike counts and score from the votes table
// for each item, used after trust changes, bans, and account deletion.
func recountItems(ctx context.Context, q *db.Queries, ids []int64) error {
	for _, id := range ids {
		it, err := q.GetItemForUpdate(ctx, id)
		if notFound(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("lock item %d: %w", id, err)
		}
		c, err := q.RecountItem(ctx, id)
		if err != nil {
			return fmt.Errorf("recount %d: %w", id, err)
		}
		score := ranking.Score(int(c.Likes), int(c.Dislikes), float64(it.PriorScore), it.Via == "import")
		if err := q.SetItemCounts(ctx, db.SetItemCountsParams{ID: id, LikeCount: c.Likes, DislikeCount: c.Dislikes, Score: score}); err != nil {
			return fmt.Errorf("set counts %d: %w", id, err)
		}
	}
	return nil
}

// SetTrust promotes or demotes a user. In one transaction it flips whether
// their votes count and recounts every item they voted on.
func (s *Service) SetTrust(ctx context.Context, userID int64, lvl int16) error {
	return s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		u, err := q.GetUser(ctx, userID)
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if u.TrustLevel == lvl {
			return nil
		}
		if err := q.SetTrustLevel(ctx, db.SetTrustLevelParams{ID: userID, TrustLevel: lvl}); err != nil {
			return fmt.Errorf("set trust: %w", err)
		}
		return recountUserVotes(ctx, q, userID, lvl >= 1 && u.ShadowBannedAt == nil)
	})
}

func recountUserVotes(ctx context.Context, q *db.Queries, userID int64, counted bool) error {
	if err := q.SetUserVotesCounted(ctx, db.SetUserVotesCountedParams{UserID: userID, Counted: counted}); err != nil {
		return fmt.Errorf("set counted: %w", err)
	}
	ids, err := q.ItemsVotedBy(ctx, userID)
	if err != nil {
		return fmt.Errorf("voted items: %w", err)
	}
	return recountItems(ctx, q, ids)
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
