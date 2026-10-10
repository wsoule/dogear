-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = $1;

-- name: UsernameTaken :one
SELECT EXISTS (SELECT 1 FROM users WHERE username = $1);

-- name: CreateUser :one
INSERT INTO users (username, avatar_url, signup_ip_hash, invited_by, display_name)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: SetUserRole :exec
UPDATE users SET role = $2 WHERE id = $1;

-- name: SetTrustLevel :exec
UPDATE users SET trust_level = $2 WHERE id = $1;

-- name: UpdateProfile :one
UPDATE users SET display_name = $2, bio = $3, website = $4 WHERE id = $1 RETURNING *;

-- name: BanUser :exec
UPDATE users SET banned_at = now() WHERE id = $1;

-- name: UnbanUser :exec
UPDATE users SET banned_at = NULL WHERE id = $1;

-- name: ShadowBanUser :exec
UPDATE users SET shadow_banned_at = now() WHERE id = $1;

-- name: UnshadowBanUser :exec
UPDATE users SET shadow_banned_at = NULL WHERE id = $1;

-- name: IncrementApprovedPosts :exec
UPDATE users SET approved_posts = approved_posts + 1 WHERE id = $1;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;

-- name: InvitedUsers :many
SELECT id FROM users WHERE invited_by = $1;

-- name: GetIdentity :one
SELECT * FROM identities WHERE provider = $1 AND provider_user_id = $2;

-- name: CreateIdentity :one
INSERT INTO identities (user_id, provider, provider_user_id, provider_created_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListIdentities :many
SELECT * FROM identities WHERE user_id = $1 ORDER BY created_at;

-- name: DeleteIdentity :exec
DELETE FROM identities WHERE user_id = $1 AND provider = $2;

-- name: GitHubIdentity :one
SELECT * FROM identities WHERE user_id = $1 AND provider = 'github';

-- name: CreateSession :exec
INSERT INTO sessions (id_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: GetSession :one
SELECT * FROM sessions WHERE id_hash = $1 AND expires_at > now();

-- name: ExtendSession :exec
UPDATE sessions SET expires_at = $2 WHERE id_hash = $1;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id_hash = $1;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < now();

-- name: CreateToken :one
INSERT INTO api_tokens (user_id, name, prefix, token_hash, scopes)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetTokenByHash :one
SELECT * FROM api_tokens WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: TouchToken :exec
UPDATE api_tokens SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ListTokens :many
SELECT * FROM api_tokens WHERE user_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC;

-- name: RevokeToken :exec
UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: CreateDeviceToken :one
INSERT INTO api_tokens (user_id, name, prefix, token_hash, scopes)
VALUES ($1, $2, $3, $4, '{read,post,vote}')
RETURNING *;

-- name: DeleteOldRevokedTokens :exec
DELETE FROM api_tokens WHERE revoked_at < now() - interval '30 days';

-- name: CreateInvite :exec
INSERT INTO invites (code_hash, inviter_id) VALUES ($1, $2);

-- name: GetInvite :one
SELECT * FROM invites WHERE code_hash = $1 AND used_at IS NULL;

-- name: UseInvite :execrows
UPDATE invites SET used_by = $2, used_at = now() WHERE code_hash = $1 AND used_at IS NULL;

-- name: CountInvites :one
SELECT count(*) FROM invites WHERE inviter_id = $1;

-- name: ListInvites :many
SELECT i.created_at, i.used_at, u.username AS used_by_username
FROM invites i LEFT JOIN users u ON u.id = i.used_by
WHERE i.inviter_id = $1 ORDER BY i.created_at;

-- name: BlockUser :exec
INSERT INTO user_blocks (blocker_id, blocked_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: UnblockUser :exec
DELETE FROM user_blocks WHERE blocker_id = $1 AND blocked_id = $2;

-- name: IsBlocked :one
SELECT EXISTS (SELECT 1 FROM user_blocks WHERE blocker_id = $1 AND blocked_id = $2);

-- name: CreateMobileCode :exec
INSERT INTO mobile_login_codes (code_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: UseMobileCode :one
UPDATE mobile_login_codes SET used_at = now()
WHERE code_hash = $1 AND used_at IS NULL AND expires_at > now()
RETURNING user_id;

-- name: ClearOldIPHashes :exec
UPDATE users SET signup_ip_hash = NULL
WHERE signup_ip_hash IS NOT NULL AND created_at < now() - interval '90 days';

-- name: PromotionCandidates :many
-- Level 0 users eligible by either path in the Moderation section.
SELECT u.id FROM users u
WHERE u.trust_level = 0 AND u.banned_at IS NULL AND (
  (u.approved_posts >= 3 AND u.created_at < now() - interval '7 days')
  OR (u.created_at < now() - interval '24 hours' AND EXISTS (
    SELECT 1 FROM identities i WHERE i.user_id = u.id AND i.provider = 'github'
      AND i.provider_created_at < now() - interval '1 year'))
);

-- name: RecentRemovals :one
SELECT count(*) FROM items
WHERE created_by = $1 AND status = 'removed' AND removed_at > now() - interval '7 days';

-- name: DismissedReports :one
SELECT count(*) FROM reports WHERE reporter_id = $1 AND status = 'dismissed';

-- name: FlagUser :exec
INSERT INTO user_flags (user_id, reason) VALUES ($1, $2)
ON CONFLICT (user_id, reason) DO NOTHING;

-- name: ClearUserFlags :exec
UPDATE user_flags SET cleared_at = now() WHERE user_id = $1 AND cleared_at IS NULL;

-- name: OpenUserFlags :many
SELECT f.user_id, f.reason, f.created_at, u.username, u.trust_level, u.shadow_banned_at, u.banned_at
FROM user_flags f JOIN users u ON u.id = f.user_id
WHERE f.cleared_at IS NULL ORDER BY f.created_at DESC LIMIT 200;

-- name: SignupRings :many
-- Groups of 5+ accounts created within one hour sharing a signup IP hash.
SELECT a.id FROM users a
WHERE a.signup_ip_hash IS NOT NULL AND a.created_at > now() - interval '2 days'
  AND (SELECT count(*) FROM users b WHERE b.signup_ip_hash = a.signup_ip_hash
       AND b.created_at BETWEEN a.created_at - interval '1 hour' AND a.created_at + interval '1 hour') >= 5;

-- name: VoteRings :many
-- Pairs of accounts that voted on 10+ of the same items with at least 80% overlap.
WITH recent AS (
  SELECT user_id, item_id FROM votes WHERE created_at > now() - interval '30 days'
), totals AS (
  SELECT user_id, count(*) AS n FROM recent GROUP BY user_id HAVING count(*) >= 10
), pairs AS (
  SELECT a.user_id AS u1, b.user_id AS u2, count(*) AS shared
  FROM recent a JOIN recent b ON a.item_id = b.item_id AND a.user_id < b.user_id
  WHERE a.user_id IN (SELECT user_id FROM totals) AND b.user_id IN (SELECT user_id FROM totals)
  GROUP BY a.user_id, b.user_id HAVING count(*) >= 10
)
SELECT p.u1, p.u2 FROM pairs p
JOIN totals t1 ON t1.user_id = p.u1 JOIN totals t2 ON t2.user_id = p.u2
WHERE p.shared::float / LEAST(t1.n, t2.n) >= 0.8;
