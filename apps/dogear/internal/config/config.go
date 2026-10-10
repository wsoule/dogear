// Package config reads all settings from environment variables and validates
// them at startup so a misconfigured deploy fails fast with a clear message.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"dogear/internal/safety"
)

type Config struct {
	Env         string // development, staging, production
	LogLevel    string
	DatabaseURL string
	Port        string
	BaseURL     string

	SessionSecret []byte

	GitHubClientID, GitHubClientSecret string
	GitHubToken                        string
	AdminGitHubID                      string

	GoogleClientID, GoogleClientSecret string

	AppleClientID, AppleTeamID, AppleKeyID string
	ApplePrivateKey                        string

	TurnstileSiteKey, TurnstileSecretKey string
	Signups                              string // open or invite
	SafeBrowsingAPIKey                   string

	Classifier            string // jev or noop
	JevAPIKey, JevBaseURL string

	GitHubAppID            int64
	GitHubAppPrivateKey    string
	GitHubAppWebhookSecret string
	GitHubAppSlug          string

	IPHashSalt string
	// TrustedProxyHops is how many proxies append to X-Forwarded-For in
	// front of the app (2 on Railway). 0 ignores the header.
	TrustedProxyHops int
	// CloudflareOriginSecret, when set, must arrive as X-Origin-Secret on
	// requests proxied by the site's Cloudflare zone (see safety.ProxyTrust).
	CloudflareOriginSecret string
	ContactEmail           string

	// VoyageAPIKey enables semantic search (Voyage AI embeddings).
	VoyageAPIKey string

	// loggit integration ("Sign in with loggit" + two-way list sync).
	LoggitAPIURL, LoggitWebURL         string
	LoggitClientID, LoggitClientSecret string
}

func (c *Config) Production() bool { return c.Env == "production" }

// ProxyTrust describes the proxies in front of the app for client-IP
// detection.
func (c *Config) ProxyTrust() safety.ProxyTrust {
	host := strings.TrimPrefix(strings.TrimPrefix(c.BaseURL, "https://"), "http://")
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	return safety.ProxyTrust{Hops: c.TrustedProxyHops, SiteHost: host, OriginSecret: c.CloudflareOriginSecret}
}
func (c *Config) Staging() bool { return c.Env == "staging" }

// GitHubLogin reports whether GitHub OAuth is configured.
func (c *Config) GitHubLogin() bool { return c.GitHubClientID != "" && c.GitHubClientSecret != "" }
func (c *Config) GoogleLogin() bool { return c.GoogleClientID != "" && c.GoogleClientSecret != "" }
func (c *Config) AppleLogin() bool {
	return c.AppleClientID != "" && c.AppleTeamID != "" && c.AppleKeyID != "" && c.ApplePrivateKey != ""
}
func (c *Config) GitHubApp() bool { return c.GitHubAppID != 0 && c.GitHubAppPrivateKey != "" }

// Load reads the environment. need lists what the current subcommand
// requires: "db" for DATABASE_URL, "web" for the server settings.
func Load(need ...string) (*Config, error) {
	c := &Config{
		Env:                    env("ENV", "development"),
		LogLevel:               env("LOG_LEVEL", "info"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		Port:                   env("PORT", "8080"),
		BaseURL:                strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		GitHubClientID:         os.Getenv("GITHUB_CLIENT_ID"),
		GitHubClientSecret:     os.Getenv("GITHUB_CLIENT_SECRET"),
		GitHubToken:            os.Getenv("GITHUB_TOKEN"),
		AdminGitHubID:          os.Getenv("ADMIN_GITHUB_ID"),
		GoogleClientID:         os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:     os.Getenv("GOOGLE_CLIENT_SECRET"),
		AppleClientID:          os.Getenv("APPLE_CLIENT_ID"),
		AppleTeamID:            os.Getenv("APPLE_TEAM_ID"),
		AppleKeyID:             os.Getenv("APPLE_KEY_ID"),
		ApplePrivateKey:        os.Getenv("APPLE_PRIVATE_KEY"),
		TurnstileSiteKey:       os.Getenv("TURNSTILE_SITE_KEY"),
		TurnstileSecretKey:     os.Getenv("TURNSTILE_SECRET_KEY"),
		Signups:                env("SIGNUPS", "invite"),
		SafeBrowsingAPIKey:     os.Getenv("SAFE_BROWSING_API_KEY"),
		Classifier:             env("CLASSIFIER", "noop"),
		JevAPIKey:              os.Getenv("JEV_API_KEY"),
		JevBaseURL:             os.Getenv("JEV_BASE_URL"),
		GitHubAppPrivateKey:    os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		GitHubAppWebhookSecret: os.Getenv("GITHUB_APP_WEBHOOK_SECRET"),
		GitHubAppSlug:          env("GITHUB_APP_SLUG", "dogear-sync"),
		IPHashSalt:             os.Getenv("IP_HASH_SALT"),
		ContactEmail:           env("CONTACT_EMAIL", "hello@dogear.fyi"),
		CloudflareOriginSecret: os.Getenv("CLOUDFLARE_ORIGIN_SECRET"),
		VoyageAPIKey:           os.Getenv("VOYAGE_API_KEY"),
		LoggitAPIURL:           env("LOGGIT_API_URL", "https://api.useloggit.app"),
		LoggitWebURL:           env("LOGGIT_WEB_URL", "https://useloggit.app"),
		LoggitClientID:         os.Getenv("LOGGIT_CLIENT_ID"),
		LoggitClientSecret:     os.Getenv("LOGGIT_CLIENT_SECRET"),
	}
	var errs []error
	// Behind a proxy every request arrives from the proxy's address, so an
	// unset value would put all visitors in one rate-limit bucket. Production
	// must state the proxy depth explicitly (0 is allowed when nothing sits in
	// front of the app).
	if os.Getenv("TRUSTED_PROXY_HOPS") == "" && c.Env == "production" {
		errs = append(errs, fmt.Errorf("TRUSTED_PROXY_HOPS is required in production (2 on Railway)"))
	}
	if v := os.Getenv("TRUSTED_PROXY_HOPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXY_HOPS must be a non-negative integer"))
		}
		c.TrustedProxyHops = n
	}
	if v := os.Getenv("GITHUB_APP_ID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("GITHUB_APP_ID must be numeric"))
		}
		c.GitHubAppID = id
	}
	switch c.Env {
	case "development", "staging", "production":
	default:
		errs = append(errs, fmt.Errorf("ENV must be development, staging, or production"))
	}
	if c.Signups != "open" && c.Signups != "invite" {
		errs = append(errs, fmt.Errorf("SIGNUPS must be open or invite"))
	}
	if c.Classifier != "jev" && c.Classifier != "noop" {
		errs = append(errs, fmt.Errorf("CLASSIFIER must be jev or noop"))
	}
	if c.Classifier == "jev" && c.JevAPIKey == "" {
		errs = append(errs, fmt.Errorf("JEV_API_KEY is required when CLASSIFIER=jev"))
	}
	for _, n := range need {
		switch n {
		case "db":
			if c.DatabaseURL == "" {
				errs = append(errs, fmt.Errorf("DATABASE_URL is required"))
			}
		case "web":
			if c.BaseURL == "" {
				errs = append(errs, fmt.Errorf("BASE_URL is required (e.g. https://dogear.fyi)"))
			}
			raw := os.Getenv("SESSION_SECRET")
			secret, err := base64.StdEncoding.DecodeString(raw)
			if err != nil || len(secret) < 32 {
				if c.Env == "development" && raw == "" {
					secret = []byte("development-only-session-secret-0123456789")
				} else {
					errs = append(errs, fmt.Errorf("SESSION_SECRET must be at least 32 random bytes, base64 encoded"))
				}
			}
			c.SessionSecret = secret
			if c.IPHashSalt == "" {
				if c.Production() {
					errs = append(errs, fmt.Errorf("IP_HASH_SALT is required in production"))
				}
				c.IPHashSalt = "dev-salt"
			}
		}
	}
	return c, errors.Join(errs...)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
