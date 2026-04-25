package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the runtime configuration. No field is Supabase-specific; any
// Postgres DATABASE_URL works.
type Config struct {
	Env         string
	Port        string
	BaseURL     string // public URL of this API (for magic-link links)
	AdminAppURL string // public URL of the admin dashboard

	DatabaseURL string

	// Resend (email)
	ResendAPIKey string
	ResendFrom   string
	FounderEmail string

	// Telegram (push)
	TelegramBotToken string
	TelegramChatID   string

	// Security
	CookieSecure     bool
	AllowedOrigins   []string
	InternalHMACKey  string
	HotLeadThreshold int
}

func Load() (*Config, error) {
	c := &Config{
		Env:              envOr("APP_ENV", "development"),
		Port:             envOr("PORT", "8080"),
		BaseURL:          envOr("BASE_URL", "http://localhost:8080"),
		AdminAppURL:      envOr("ADMIN_APP_URL", "http://localhost:3000/admin"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		ResendAPIKey:     os.Getenv("RESEND_API_KEY"),
		ResendFrom:       envOr("RESEND_FROM", "HillPrime <no-reply@hillprimeinnovations.com>"),
		FounderEmail:     envOr("FOUNDER_EMAIL", "founder@hillprimeinnovations.com"),
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:   os.Getenv("TELEGRAM_CHAT_ID"),
		InternalHMACKey:  os.Getenv("INTERNAL_HMAC_KEY"),
		HotLeadThreshold: envInt("HOT_LEAD_THRESHOLD", 60),
		AllowedOrigins: splitAndTrim(envOr("ALLOWED_ORIGINS",
			"https://hillprimeinnovations.com,https://www.hillprimeinnovations.com,http://localhost:3000")),
	}
	c.CookieSecure = c.Env == "production"

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.InternalHMACKey == "" {
		missing = append(missing, "INTERNAL_HMAC_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
