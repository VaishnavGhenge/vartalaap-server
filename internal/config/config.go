package config

import (
	"log"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port            string
	AllowedOrigins  []string
	CFTurnKeyID     string
	CFTurnAPIToken  string
	CFCallsAppID    string
	CFCallsAppToken string
	SentryDSN       string
	DatabaseURL     string
	JWTSecret       string
	AccessTokenTTL  time.Duration
	SecureCookie    bool
	// PublicAppURL is the canonical base URL the booking surface lives at —
	// used to build absolute room/confirmation links in transactional emails.
	// Falls back to the first AllowedOrigins entry so dev environments work
	// without extra wiring.
	PublicAppURL string
}

func Load() Config {
	cfg := Config{
		Port:            getenv("PORT", "8080"),
		AllowedOrigins:  splitCSV(getenv("ALLOWED_ORIGINS", "http://localhost:3000")),
		CFTurnKeyID:     os.Getenv("CF_TURN_KEY_ID"),
		CFTurnAPIToken:  os.Getenv("CF_TURN_API_TOKEN"),
		CFCallsAppID:    os.Getenv("CF_CALLS_APP_ID"),
		CFCallsAppToken: os.Getenv("CF_CALLS_APP_TOKEN"),
		SentryDSN:       os.Getenv("SENTRY_DSN"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		JWTSecret:       os.Getenv("JWT_SECRET"),
		AccessTokenTTL:  parseDuration(getenv("JWT_ACCESS_TTL", "15m")),
		SecureCookie:    os.Getenv("SECURE_COOKIE") == "true",
		PublicAppURL:    os.Getenv("PUBLIC_APP_URL"),
	}
	if cfg.PublicAppURL == "" && len(cfg.AllowedOrigins) > 0 {
		cfg.PublicAppURL = cfg.AllowedOrigins[0]
	}
	if cfg.CFTurnKeyID == "" || cfg.CFTurnAPIToken == "" {
		log.Println("WARN: CF_TURN_KEY_ID / CF_TURN_API_TOKEN not set — /ice-servers will fail")
	}
	if cfg.CFCallsAppID == "" || cfg.CFCallsAppToken == "" {
		log.Println("WARN: CF_CALLS_APP_ID / CF_CALLS_APP_TOKEN not set — SFU endpoints will be unavailable")
	}
	if cfg.DatabaseURL == "" {
		log.Println("WARN: DATABASE_URL not set — auth endpoints will be unavailable")
	}
	if cfg.JWTSecret == "" {
		log.Println("WARN: JWT_SECRET not set — auth endpoints will be unavailable")
	}
	return cfg
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 15 * time.Minute
	}
	return d
}
