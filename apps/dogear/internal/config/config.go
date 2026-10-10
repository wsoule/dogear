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

	IPHashSalt   string
	ContactEmail string
}

func (c *Config) Production() bool { return c.Env == "production" }
func (c *Config) Staging() bool    { return c.Env == "staging" }

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
	}
	var errs []error
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
