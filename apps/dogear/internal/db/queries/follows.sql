-- name: FollowUser :exec
INSERT INTO follows (follower_id, followee_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: UnfollowUser :exec
DELETE FROM follows WHERE follower_id = $1 AND followee_id = $2;

-- name: UnfollowBothWays :exec
-- Drops follows in either direction between two users, used when one
-- blocks the other.
DELETE FROM follows
WHERE (follower_id = @a::bigint AND followee_id = @b::bigint)
   OR (follower_id = @b::bigint AND followee_id = @a::bigint);

-- name: IsFollowing :one
SELECT EXISTS (SELECT 1 FROM follows WHERE follower_id = $1 AND followee_id = $2);

-- name: CountFollowing :one
-- Every follow the user has made, for the per-user cap.
SELECT count(*) FROM follows WHERE follower_id = $1;

-- name: FollowCounts :one
-- Followers and following as shown on a profile. Banned and shadow-banned
-- accounts are left out on both sides, so they cannot inflate a count.
SELECT
  (SELECT count(*) FROM follows f JOIN users u ON u.id = f.follower_id
   WHERE f.followee_id = @user_id AND u.banned_at IS NULL AND u.shadow_banned_at IS NULL)::int AS followers,
  (SELECT count(*) FROM follows f JOIN users u ON u.id = f.followee_id
   WHERE f.follower_id = @user_id AND u.banned_at IS NULL AND u.shadow_banned_at IS NULL)::int AS following;

-- name: FollowingSavesPage :many
-- One page of saves by the people a user follows, newest first,
-- keyset-paginated on (saved_at, user_id, item_id). It applies the public
-- archive rule: the saver is not banned or shadow-banned, the item is live
-- with a working link, and none of the saver's tags on it is non-public.
-- Blocks in either direction hide the saver. The caller still checks the
-- item's poster with visibleTo, as Archive does.
SELECT s.user_id, u.username, s.note, s.saved_at, sqlc.embed(i)
FROM follows f
JOIN saves s ON s.user_id = f.followee_id
JOIN users u ON u.id = s.user_id
JOIN items i ON i.id = s.item_id
WHERE f.follower_id = @viewer_id
  AND u.banned_at IS NULL AND u.shadow_banned_at IS NULL
  AND i.status = 'live' AND i.link_status <> 'dead'
  AND NOT EXISTS (SELECT 1 FROM user_blocks b
       WHERE (b.blocker_id = @viewer_id AND b.blocked_id = s.user_id)
          OR (b.blocker_id = s.user_id AND b.blocked_id = @viewer_id))
  AND NOT EXISTS (
       SELECT 1 FROM list_items li JOIN lists l ON l.id = li.list_id
       WHERE l.user_id = s.user_id AND li.item_id = s.item_id AND l.visibility <> 'public')
  AND (sqlc.narg(before_at)::timestamptz IS NULL
       OR (s.saved_at, s.user_id, s.item_id) < (sqlc.narg(before_at)::timestamptz, @before_user::bigint, @before_item::bigint))
ORDER BY s.saved_at DESC, s.user_id DESC, s.item_id DESC
LIMIT @lim;

-- name: RecentPublicSaves :many
-- The newest public saves across everyone, for the home page strip. Same
-- public archive rule as FollowingSavesPage, plus: only trusted savers
-- (trust level 1+) to keep new-account spam off the home page, and the
-- item's poster must not be banned or shadow-banned (what visibleTo checks).
-- A signed-in viewer (viewer_id > 0) does not see their own saves or anyone
-- they have blocked, as saver or poster. The caller keeps one save per user
-- and per item.
SELECT s.user_id, u.username, s.saved_at, i.id, i.title
FROM saves s
JOIN users u ON u.id = s.user_id
JOIN items i ON i.id = s.item_id
LEFT JOIN users p ON p.id = i.created_by
WHERE u.banned_at IS NULL AND u.shadow_banned_at IS NULL AND u.trust_level >= 1
  AND i.status = 'live' AND i.link_status <> 'dead'
  AND (p.id IS NULL OR (p.banned_at IS NULL AND p.shadow_banned_at IS NULL))
  AND s.user_id <> @viewer_id
  AND NOT EXISTS (SELECT 1 FROM user_blocks b
       WHERE b.blocker_id = @viewer_id AND b.blocked_id IN (s.user_id, i.created_by))
  AND NOT EXISTS (
       SELECT 1 FROM list_items li JOIN lists l ON l.id = li.list_id
       WHERE l.user_id = s.user_id AND li.item_id = s.item_id AND l.visibility <> 'public')
ORDER BY s.saved_at DESC
LIMIT @lim;

-- name: SuggestedPeople :many
-- Trusted users with the most public saves, for the empty /following page.
-- Leaves out the viewer, people they already follow, and blocks either way.
SELECT u.username, u.display_name, count(*)::int AS saves
FROM saves s
JOIN users u ON u.id = s.user_id
JOIN items i ON i.id = s.item_id
WHERE u.banned_at IS NULL AND u.shadow_banned_at IS NULL AND u.trust_level >= 1
  AND u.id <> @viewer_id
  AND i.status = 'live' AND i.link_status <> 'dead'
  AND NOT EXISTS (SELECT 1 FROM follows f WHERE f.follower_id = @viewer_id AND f.followee_id = u.id)
  AND NOT EXISTS (SELECT 1 FROM user_blocks b
       WHERE (b.blocker_id = @viewer_id AND b.blocked_id = u.id)
          OR (b.blocker_id = u.id AND b.blocked_id = @viewer_id))
  AND NOT EXISTS (
       SELECT 1 FROM list_items li JOIN lists l ON l.id = li.list_id
       WHERE l.user_id = s.user_id AND li.item_id = s.item_id AND l.visibility <> 'public')
GROUP BY u.id, u.username, u.display_name
ORDER BY count(*) DESC, u.id
LIMIT @lim;

-- name: UserFollowing :many
-- Everyone the user follows, for account export.
SELECT u.username, f.created_at
FROM follows f JOIN users u ON u.id = f.followee_id
WHERE f.follower_id = $1 ORDER BY f.created_at;
