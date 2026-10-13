package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"dogear/internal/db"
	"dogear/internal/safety"
)

const (
	sessionLifetime = 30 * 24 * time.Hour
	tokenPrefix     = "afk_"
	invitesPerUser  = 3
)

// ExternalIdentity is a verified profile returned by an OAuth provider.
type ExternalIdentity struct {
	Provider          string
	ProviderUserID    string
	SuggestedUsername string
	DisplayName       string
	AvatarURL         string
	ProviderCreatedAt *time.Time
}

var (
	usernameRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,28}[a-z0-9]$`)
	reservedUsernames = []string{"admin", "administrator", "api", "about", "auth", "c", "i", "k", "u", "mod",
		"moderator", "new", "search", "settings", "submit", "static", "dogear", "root", "support", "help",
		"terms", "privacy", "invite", "mcp", "llms", "staff", "system", "null", "undefined", "me", "github", "test"}
)

// RandomToken returns n random bytes as unpadded base64url.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Hash is the sha256 used for stored session ids, tokens, and codes.
func Hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// ValidUsername reports whether a username is well-formed and not reserved.
func ValidUsername(u string) error {
	if !usernameRe.MatchString(u) || strings.Contains(u, "--") {
		return errf("invalid", "usernames are 3–30 lowercase letters, digits, or single dashes")
	}
	if slices.Contains(reservedUsernames, u) {
		return errf("invalid", "that username is reserved")
	}
	return nil
}

var nonUsername = regexp.MustCompile(`[^a-z0-9]+`)

// SuggestUsername lowercases a handle or name and appends a numeric suffix
// until it is free.
func (s *Service) SuggestUsername(ctx context.Context, base string) string {
	base = strings.Trim(nonUsername.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if len(base) > 24 {
		base = strings.Trim(base[:24], "-")
	}
	if len(base) < 3 {
		base = "reader"
	}
	for i := 1; i < 10000; i++ {
		cand := base
		if i > 1 {
			cand = base + strconv.Itoa(i)
		}
		if ValidUsername(cand) != nil {
			continue
		}
		taken, err := s.Q.UsernameTaken(ctx, cand)
		if err == nil && !taken {
			return cand
		}
	}
	return base + RandomToken(3)
}

// FindIdentity returns the user linked to a provider identity, if any.
func (s *Service) FindIdentity(ctx context.Context, provider, providerUserID string) (*db.User, error) {
	id, err := s.Q.GetIdentity(ctx, db.GetIdentityParams{Provider: provider, ProviderUserID: providerUserID})
	if notFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("get identity: %w", err)
	}
	u, err := s.Q.GetUser(ctx, id.UserID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return &u, nil
}

// OnLogin runs after a successful sign-in: the configured admin GitHub id
// gets the admin role (matched on the numeric id, never the username).
func (s *Service) OnLogin(ctx context.Context, u *db.User, ext ExternalIdentity) error {
	if u.BannedAt != nil {
		return errf("forbidden", "this account is banned")
	}
	if ext.Provider == "github" && s.Cfg.AdminGitHubID != "" && ext.ProviderUserID == s.Cfg.AdminGitHubID && u.Role != "admin" {
		if err := s.Q.SetUserRole(ctx, db.SetUserRoleParams{ID: u.ID, Role: "admin"}); err != nil {
			return fmt.Errorf("set admin: %w", err)
		}
		u.Role = "admin"
		if u.TrustLevel < 1 {
			if err := s.SetTrust(ctx, u.ID, 1); err != nil {
				return err
			}
			u.TrustLevel = 1
		}
	}
	return nil
}

// SignUp creates a user from a verified identity after they confirm a
// username. In invite mode a valid unused invite code is required. Every new
// user gets a default public "Favorites" list.
func (s *Service) SignUp(ctx context.Context, ext ExternalIdentity, username, inviteCode, clientIP string) (db.User, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if err := ValidUsername(username); err != nil {
		return db.User{}, err
	}
	var invite *db.Invite
	if s.Cfg.Signups == "invite" && !s.isAdminIdentity(ext) {
		if inviteCode == "" {
			return db.User{}, errf("forbidden", "Dogear is invite-only right now; you need an invite link")
		}
		inv, err := s.Q.GetInvite(ctx, Hash(inviteCode))
		if notFound(err) {
			return db.User{}, errf("forbidden", "that invite is invalid or already used")
		} else if err != nil {
			return db.User{}, fmt.Errorf("get invite: %w", err)
		}
		invite = &inv
	}
	ipHash := sha256.Sum256([]byte(s.Cfg.IPHashSalt + "|" + clientIP))
	var u db.User
	err := s.inTx(ctx, func(q *db.Queries, _ pgx.Tx) error {
		taken, err := q.UsernameTaken(ctx, username)
		if err != nil {
			return fmt.Errorf("username check: %w", err)
		}
		if taken {
			return errf("invalid", "that username is taken")
		}
		var invitedBy *int64
		if invite != nil {
			invitedBy = &invite.InviterID
		}
		var display *string
		if d := truncateRunes(safety.SanitizeText(ext.DisplayName), 60); d != "" && !safety.HasMarkupOrURL(d) {
			display = &d
		}
		u, err = q.CreateUser(ctx, db.CreateUserParams{Username: username, AvatarUrl: strPtr(ext.AvatarURL),
			SignupIpHash: ipHash[:], InvitedBy: invitedBy, DisplayName: display})
		if err != nil {
			if isUniqueViolation(err) {
				return errf("invalid", "that username is taken")
			}
			return fmt.Errorf("create user: %w", err)
		}
		if _, err := q.CreateIdentity(ctx, db.CreateIdentityParams{UserID: u.ID, Provider: ext.Provider,
			ProviderUserID: ext.ProviderUserID, ProviderCreatedAt: ext.ProviderCreatedAt}); err != nil {
			if isUniqueViolation(err) {
				return errf("invalid", "that sign-in is already linked to an account")
			}
			return fmt.Errorf("create identity: %w", err)
		}
		if invite != nil {
			n, err := q.UseInvite(ctx, db.UseInviteParams{CodeHash: invite.CodeHash, UsedBy: &u.ID})
			if err != nil {
				return fmt.Errorf("use invite: %w", err)
			}
			if n == 0 {
				return errf("forbidden", "that invite was just used")
			}
		}
		if _, err := q.CreateList(ctx, db.CreateListParams{UserID: u.ID, Slug: "favorites", Title: "Favorites", Visibility: "public"}); err != nil {
			return fmt.Errorf("favorites list: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.User{}, err
	}
	if err := s.OnLogin(ctx, &u, ext); err != nil {
		return db.User{}, err
	}
	return u, nil
}

func (s *Service) isAdminIdentity(ext ExternalIdentity) bool {
	return ext.Provider == "github" && s.Cfg.AdminGitHubID != "" && ext.ProviderUserID == s.Cfg.AdminGitHubID
}

// LinkIdentity attaches another sign-in provider to a signed-in user. An
// identity already linked to someone else cannot be linked. Accounts are
// never merged by email.
func (s *Service) LinkIdentity(ctx context.Context, u db.User, ext ExternalIdentity) error {
	existing, err := s.Q.GetIdentity(ctx, db.GetIdentityParams{Provider: ext.Provider, ProviderUserID: ext.ProviderUserID})
	if err == nil {
		if existing.UserID == u.ID {
			return nil
		}
		return errf("invalid", "that %s account is already linked to another Dogear user", ext.Provider)
	} else if !notFound(err) {
		return fmt.Errorf("get identity: %w", err)
	}
	ids, err := s.Q.ListIdentities(ctx, u.ID)
	if err != nil {
		return fmt.Errorf("list identities: %w", err)
	}
	for _, id := range ids {
		if id.Provider == ext.Provider {
			return errf("invalid", "unlink your current %s account first", ext.Provider)
		}
	}
	if _, err := s.Q.CreateIdentity(ctx, db.CreateIdentityParams{UserID: u.ID, Provider: ext.Provider,
		ProviderUserID: ext.ProviderUserID, ProviderCreatedAt: ext.ProviderCreatedAt}); err != nil {
		return fmt.Errorf("create identity: %w", err)
	}
	return s.OnLogin(ctx, &u, ext)
}

// UnlinkIdentity removes a provider; the last remaining identity stays.
func (s *Service) UnlinkIdentity(ctx context.Context, u db.User, provider string) error {
	ids, err := s.Q.ListIdentities(ctx, u.ID)
	if err != nil {
		return fmt.Errorf("list identities: %w", err)
	}
	if len(ids) <= 1 {
		return errf("invalid", "you cannot unlink your only sign-in method")
	}
	return s.Q.DeleteIdentity(ctx, db.DeleteIdentityParams{UserID: u.ID, Provider: provider})
}

func (s *Service) Identities(ctx context.Context, userID int64) ([]db.Identity, error) {
	return s.Q.ListIdentities(ctx, userID)
}

// CreateSession stores a new session and returns the raw cookie value; only
// its sha256 is kept in the database.
func (s *Service) CreateSession(ctx context.Context, userID int64) (string, error) {
	raw := RandomToken(32)
	if err := s.Q.CreateSession(ctx, db.CreateSessionParams{IDHash: Hash(raw), UserID: userID, ExpiresAt: time.Now().Add(sessionLifetime)}); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return raw, nil
}

// SessionUser resolves a session cookie to its user, extending the session
// on use (at most once an hour to avoid a write per request).
func (s *Service) SessionUser(ctx context.Context, raw string) (*db.User, error) {
	if raw == "" {
		return nil, nil
	}
	sess, err := s.Q.GetSession(ctx, Hash(raw))
	if notFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	u, err := s.Q.GetUser(ctx, sess.UserID)
	if err != nil {
		return nil, fmt.Errorf("session user: %w", err)
	}
	if u.BannedAt != nil {
		return nil, nil
	}
	if time.Until(sess.ExpiresAt) < sessionLifetime-time.Hour {
		_ = s.Q.ExtendSession(ctx, db.ExtendSessionParams{IDHash: sess.IDHash, ExpiresAt: time.Now().Add(sessionLifetime)})
	}
	return &u, nil
}

func (s *Service) DeleteSession(ctx context.Context, raw string) error {
	return s.Q.DeleteSession(ctx, Hash(raw))
}

var validScopes = []string{"read", "post", "vote"}

// CreateToken issues an API token. It is shown once; only its hash and an
// 8-character prefix are stored. Tokens are created only from the web UI.
func (s *Service) CreateToken(ctx context.Context, u db.User, name string, scopes []string) (string, db.ApiToken, error) {
	name, err := CleanText("name", name, 1, 40)
	if err != nil {
		return "", db.ApiToken{}, err
	}
	if len(scopes) == 0 {
		scopes = validScopes
	}
	for _, sc := range scopes {
		if !slices.Contains(validScopes, sc) {
			return "", db.ApiToken{}, errf("invalid", "unknown scope %q", sc)
		}
	}
	raw := tokenPrefix + RandomToken(32)
	t, err := s.Q.CreateToken(ctx, db.CreateTokenParams{UserID: u.ID, Name: name, Prefix: raw[:8], TokenHash: Hash(raw), Scopes: scopes})
	if err != nil {
		return "", db.ApiToken{}, fmt.Errorf("create token: %w", err)
	}
	return raw, t, nil
}

func (s *Service) Tokens(ctx context.Context, userID int64) ([]db.ApiToken, error) {
	return s.Q.ListTokens(ctx, userID)
}

func (s *Service) RevokeToken(ctx context.Context, userID, tokenID int64) error {
	return s.Q.RevokeToken(ctx, db.RevokeTokenParams{ID: tokenID, UserID: userID})
}

// TokenActor resolves a bearer token. Revoked tokens and banned owners fail.
func (s *Service) TokenActor(ctx context.Context, raw, via string) (*Actor, error) {
	if !strings.HasPrefix(raw, tokenPrefix) {
		return nil, ErrUnauthorized
	}
	t, err := s.Q.GetTokenByHash(ctx, Hash(raw))
	if notFound(err) {
		return nil, ErrUnauthorized
	} else if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}
	u, err := s.Q.GetUser(ctx, t.UserID)
	if err != nil {
		return nil, fmt.Errorf("token user: %w", err)
	}
	if u.BannedAt != nil {
		return nil, ErrUnauthorized
	}
	_ = s.Q.TouchToken(ctx, t.ID)
	return &Actor{User: u, TokenID: &t.ID, Scopes: t.Scopes, Via: via}, nil
}

// CreateInvite issues one of a trusted user's invites.
func (s *Service) CreateInvite(ctx context.Context, u db.User) (string, error) {
	if u.TrustLevel < 1 && u.Role == "user" {
		return "", errf("forbidden", "invites unlock once your account is trusted")
	}
	n, err := s.Q.CountInvites(ctx, u.ID)
	if err != nil {
		return "", fmt.Errorf("count invites: %w", err)
	}
	if n >= invitesPerUser && u.Role != "admin" {
		return "", errf("forbidden", "you have used all %d invites", invitesPerUser)
	}
	code := RandomToken(12)
	if err := s.Q.CreateInvite(ctx, db.CreateInviteParams{CodeHash: Hash(code), InviterID: u.ID}); err != nil {
		return "", fmt.Errorf("create invite: %w", err)
	}
	return code, nil
}

func (s *Service) Invites(ctx context.Context, userID int64) ([]db.ListInvitesRow, error) {
	return s.Q.ListInvites(ctx, userID)
}

// InviteValid reports whether an invite code exists and is unused.
func (s *Service) InviteValid(ctx context.Context, code string) bool {
	_, err := s.Q.GetInvite(ctx, Hash(code))
	return err == nil
}

// ValidChallenge reports whether a PKCE S256 code challenge is well-formed
// (43 base64url characters, the encoding of a sha256 digest).
func ValidChallenge(c string) bool {
	b, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(b) == sha256.Size
}

// CreateMobileCode issues a one-time 60-second code for the native app,
// bound to the app's PKCE S256 challenge.
func (s *Service) CreateMobileCode(ctx context.Context, userID int64, challenge string) (string, error) {
	if !ValidChallenge(challenge) {
		return "", errf("invalid", "missing or invalid code_challenge")
	}
	code := RandomToken(32)
	if err := s.Q.CreateMobileCode(ctx, db.CreateMobileCodeParams{CodeHash: Hash(code), UserID: userID,
		ExpiresAt: time.Now().Add(60 * time.Second), CodeChallenge: challenge}); err != nil {
		return "", fmt.Errorf("mobile code: %w", err)
	}
	return code, nil
}

// ExchangeMobileCode trades a one-time code plus the PKCE verifier for a
// device API token. The code is consumed even when the verifier is wrong, so
// an intercepted code cannot be retried.
func (s *Service) ExchangeMobileCode(ctx context.Context, code, verifier, device string) (string, db.User, error) {
	row, err := s.Q.UseMobileCode(ctx, Hash(code))
	if notFound(err) {
		return "", db.User{}, ErrUnauthorized
	} else if err != nil {
		return "", db.User{}, fmt.Errorf("use code: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	if verifier == "" || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(row.CodeChallenge)) != 1 {
		return "", db.User{}, ErrUnauthorized
	}
	u, err := s.Q.GetUser(ctx, row.UserID)
	if err != nil {
		return "", db.User{}, fmt.Errorf("get user: %w", err)
	}
	if device == "" {
		device = "mobile"
	}
	raw, _, err := s.CreateToken(ctx, u, device, nil)
	return raw, u, err
}

// GetUserByUsername returns a non-banned user.
func (s *Service) GetUserByUsername(ctx context.Context, username string) (db.User, error) {
	u, err := s.Q.GetUserByUsername(ctx, username)
	if notFound(err) || (err == nil && u.BannedAt != nil) {
		return db.User{}, ErrNotFound
	}
	return u, err
}

type ProfileInput struct {
	DisplayName string `json:"display_name"`
	Bio         string `json:"bio"`
	Website     string `json:"website"`
}

// UpdateProfile edits display name, bio, and website (one http(s) link).
func (s *Service) UpdateProfile(ctx context.Context, u db.User, in ProfileInput) (db.User, error) {
	name, err := CleanText("display name", in.DisplayName, 0, 60)
	if err != nil {
		return u, err
	}
	bio, err := CleanText("bio", in.Bio, 0, 280)
	if err != nil {
		return u, err
	}
	var website *string
	if w := strings.TrimSpace(in.Website); w != "" {
		norm, err := safety.NormalizeURL(w)
		if err != nil {
			return u, errf("invalid", "website must be an http or https link")
		}
		website = &norm
	}
	out, err := s.Q.UpdateProfile(ctx, db.UpdateProfileParams{ID: u.ID, DisplayName: strPtr(name), Bio: bio, Website: website})
	if err != nil {
		return u, fmt.Errorf("update profile: %w", err)
	}
	s.markDirty(ctx, u.ID)
	return out, nil
}

// Profile is a user's page: their archive (first page), tags, and posts.
type Profile struct {
	User    db.User
	Lists   []List // the user's tags; only public ones for other viewers
	Archive ArchiveView
	Posts   []Item
	IsOwner bool
	Blocked bool
	// FollowerCount and FollowingCount count visible accounts on each side;
	// ViewerFollows is true when the signed-in viewer follows this user.
	FollowerCount, FollowingCount int
	ViewerFollows                 bool
	// SaveCount is how many saves this viewer can see: all of them for the
	// owner, the public archive's count for everyone else.
	SaveCount int
}

// GetProfile returns a profile with the archive, tags, and posts as the
// viewer may see them.
func (s *Service) GetProfile(ctx context.Context, viewer *Actor, username string) (Profile, error) {
	u, err := s.GetUserByUsername(ctx, username)
	if err != nil {
		return Profile{}, err
	}
	p := Profile{User: u, IsOwner: viewer != nil && viewer.ID() == u.ID}
	if u.ShadowBannedAt != nil && !p.IsOwner && !viewer.IsMod() {
		return Profile{}, ErrNotFound
	}
	if p.Lists, err = s.userLists(ctx, u.ID, p.IsOwner); err != nil {
		return Profile{}, err
	}
	if p.Archive, err = s.Archive(ctx, viewer, u.ID, ""); err != nil {
		return Profile{}, err
	}
	if p.Posts, err = s.UserItems(ctx, viewer, u.ID, 100); err != nil {
		return Profile{}, err
	}
	if viewer != nil && !p.IsOwner {
		p.Blocked, _ = s.Q.IsBlocked(ctx, db.IsBlockedParams{BlockerID: viewer.ID(), BlockedID: u.ID})
		p.ViewerFollows = s.IsFollowing(ctx, viewer, u.ID)
	}
	if p.FollowerCount, p.FollowingCount, err = s.FollowCounts(ctx, u.ID); err != nil {
		return Profile{}, err
	}
	var n int64
	if p.IsOwner {
		n, err = s.Q.CountSaves(ctx, u.ID)
	} else {
		n, err = s.Q.CountPublicSaves(ctx, u.ID)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("count saves: %w", err)
	}
	p.SaveCount = int(n)
	return p, nil
}
