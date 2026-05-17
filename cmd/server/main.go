package main

import (
	"context"
	"embed"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vaishnavghenge/vartalaap-server/internal/cfrealtime"
	"github.com/vaishnavghenge/vartalaap-server/internal/cfturn"
	"github.com/vaishnavghenge/vartalaap-server/internal/config"
	"github.com/vaishnavghenge/vartalaap-server/internal/db"
	"github.com/vaishnavghenge/vartalaap-server/internal/httpx"
	_ "github.com/vaishnavghenge/vartalaap-server/internal/metrics"
	"github.com/vaishnavghenge/vartalaap-server/internal/sfu"
	"github.com/vaishnavghenge/vartalaap-server/internal/signaling"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

//go:embed web/dashboard.html
var dashboardHTML embed.FS

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg := config.Load()

	if cfg.SentryDSN != "" {
		if err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.SentryDSN,
			TracesSampleRate: 0.2,
		}); err != nil {
			log.Printf("WARN: sentry init failed: %v", err)
		} else {
			defer sentry.Flush(2 * time.Second)
			log.Println("Sentry enabled")
		}
	}

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		ms := &http.Server{Addr: "127.0.0.1:9091", Handler: mux}
		if err := ms.ListenAndServe(); err != nil {
			log.Printf("metrics server: %v", err)
		}
	}()

	hub := signaling.NewHub()
	cf := cfturn.New(cfg.CFTurnKeyID, cfg.CFTurnAPIToken)
	sfuRegistry := sfu.NewRegistry()
	meetLimiter := httpx.NewRateLimiter(12, 24)
	iceLimiter := httpx.NewRateLimiter(60, 120)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/stats", httpx.NewStatsHandler())
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		b, _ := dashboardHTML.ReadFile("web/dashboard.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/ws", signaling.NewHandler(hub, cfg.AllowedOrigins))
	mux.HandleFunc("/meets/new", httpx.NewMeetHandler(cfg.AllowedOrigins, meetLimiter))
	mux.HandleFunc("/ice-servers", httpx.NewIceHandler(cf, cfg.AllowedOrigins, iceLimiter))

	if cfg.CFCallsAppID != "" && cfg.CFCallsAppToken != "" {
		cfCalls := cfrealtime.New(cfg.CFCallsAppID, cfg.CFCallsAppToken)
		authCfg := httpx.AuthConfig{
			AllowedOrigins: cfg.AllowedOrigins,
			JWTSecret:      cfg.JWTSecret,
			AccessTokenTTL: cfg.AccessTokenTTL,
			SecureCookie:   cfg.SecureCookie,
		}
		httpx.SFUHandlers(mux, hub, sfuRegistry, cfCalls, authCfg)
		log.Println("SFU endpoints enabled")
	} else {
		log.Println("WARN: SFU endpoints disabled (missing CF_CALLS_APP_ID or CF_CALLS_APP_TOKEN)")
	}

	if cfg.DatabaseURL != "" && cfg.JWTSecret != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		pool, err := db.Open(ctx, cfg.DatabaseURL)
		cancel()
		if err != nil {
			log.Fatalf("db: %v", err)
		}
		defer pool.Close()

		st := store.New(pool)
		authCfg := httpx.AuthConfig{
			AllowedOrigins: cfg.AllowedOrigins,
			JWTSecret:      cfg.JWTSecret,
			AccessTokenTTL: cfg.AccessTokenTTL,
			SecureCookie:   cfg.SecureCookie,
		}
		httpx.AuthHandlers(mux, st, authCfg)
		httpx.MeHandlers(mux, st, authCfg)
		httpx.BookingHandlers(mux, st, authCfg)
		httpx.SlotHandlers(mux, st, authCfg)
		log.Println("Auth endpoints enabled")
		log.Println("Scheduling endpoints enabled: /me/availability, /me/event-types")
		log.Println("Booking endpoints enabled: /bookings, /me/bookings")
		log.Println("Public slot endpoint enabled: /u/{slug}/{event}/slots")
	} else {
		log.Println("WARN: Auth endpoints disabled (missing DATABASE_URL or JWT_SECRET)")
	}

	sentinel := sentryhttp.New(sentryhttp.Options{Repanic: true})

	// Middleware order: RequestID first so every later layer (metrics, logs,
	// Sentry, handlers) sees the same X-Request-Id. Metrics next so the
	// histogram covers the handler's real work, not just the inner mux dispatch.
	// LogMiddleware reads request_id from context and emits a structured line
	// per request. Sentry's sentinel runs nearest the handler so panics inside
	// the handler — not the middleware — are what get captured.
	handler := httpx.RequestIDMiddleware(
		httpx.MetricsMiddleware(
			httpx.LogMiddleware(
				sentinel.Handle(mux),
			),
		),
	)
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("vartalaap-server listening on :%s", cfg.Port)
	log.Fatal(srv.ListenAndServe())
}
