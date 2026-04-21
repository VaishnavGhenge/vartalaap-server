package config

import (
	"log"
	"os"
	"strings"
)

type Config struct {
	Port           string
	AllowedOrigins []string
	CFTurnKeyID    string
	CFTurnAPIToken string
}

func Load() Config {
	cfg := Config{
		Port:           getenv("PORT", "8080"),
		AllowedOrigins: splitCSV(getenv("ALLOWED_ORIGINS", "http://localhost:3000")),
		CFTurnKeyID:    os.Getenv("CF_TURN_KEY_ID"),
		CFTurnAPIToken: os.Getenv("CF_TURN_API_TOKEN"),
	}
	if cfg.CFTurnKeyID == "" || cfg.CFTurnAPIToken == "" {
		log.Println("WARN: CF_TURN_KEY_ID / CF_TURN_API_TOKEN not set — /ice-servers will fail")
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
