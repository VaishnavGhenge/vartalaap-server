package main

import (
	"context"
	"embed"
	"errors"
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
	"github.com/vaishnavghenge/vartalaap-server/internal/email"
	"github.com/vaishnavghenge/vartalaap-server/internal/httpx"
	_ "github.com/vaishnavghenge/vartalaap-server/internal/metrics"
	"github.com/vaishnavghenge/vartalaap-server/internal/roomaccess"
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
	instantRooms := roomaccess.NewInstantRegistry()
	hub.SetRoomEmptyHandler(func(roomID string) {
		instantRooms.MarkEmpty(roomID, time.Now().UTC())
	})
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
	var roomGate httpx.RoomAccessGate
	callRoomGate := func(ctx context.Context, room string, activate bool) error {
		if roomGate == nil {
			if activate {
				return instantRooms.ActivateOrAllow(room, time.Now().UTC(), cfg.InstantRoomEmptyGrace)
			}
			return instantRooms.AllowActive(room, time.Now().UTC(), cfg.InstantRoomEmptyGrace)
		}
		return roomGate(ctx, room, activate)
	}
	signalingGate := func(ctx context.Context, room string) error {
		if err := callRoomGate(ctx, room, true); err != nil {
			switch {
			case errors.Is(err, roomaccess.ErrNotStarted):
				return &signaling.RoomGateError{Code: "ROOM_NOT_STARTED", Message: "This meeting has not started yet."}
			case errors.Is(err, roomaccess.ErrExpired):
				return &signaling.RoomGateError{Code: "ROOM_EXPIRED", Message: "This meeting is no longer active."}
			default:
				return &signaling.RoomGateError{Code: "ROOM_UNAVAILABLE", Message: "This meeting is not available right now."}
			}
		}
		return nil
	}

	mux.HandleFunc("/ws", signaling.NewHandler(hub, cfg.AllowedOrigins, signalingGate))
	mux.HandleFunc("/meets/new", httpx.NewMeetHandler(cfg.AllowedOrigins, meetLimiter, func(room string, now time.Time) {
		instantRooms.Create(room, now, cfg.InstantRoomTTL)
	}))
	mux.HandleFunc("/ice-servers", httpx.NewIceHandler(cf, cfg.AllowedOrigins, iceLimiter, callRoomGate))

	// getBookingStatus is nil until the DB block wires it in. The status handler
	// reads it via closure so the booking-aware path activates without re-registration.
	var getBookingStatus func(ctx context.Context, room string) (httpx.RoomStatusResult, bool)
	mux.HandleFunc("/room/status", httpx.NewRoomStatusHandler(cfg.AllowedOrigins,
		func(ctx context.Context, room string) httpx.RoomStatusResult {
			if getBookingStatus != nil {
				if result, ok := getBookingStatus(ctx, room); ok {
					return result
				}
			}
			// Instant-room fallback (or when DB is unavailable).
			if err := instantRooms.AllowActive(room, time.Now().UTC(), cfg.InstantRoomEmptyGrace); err != nil {
				if errors.Is(err, roomaccess.ErrExpired) {
					return httpx.RoomStatusResult{Status: "ended", Message: "This room is no longer active."}
				}
				// ErrUnknownRoom: instant room hasn't been created yet; first join creates it.
			}
			return httpx.RoomStatusResult{Status: "open"}
		}))

	if cfg.CFCallsAppID != "" && cfg.CFCallsAppToken != "" {
		cfCalls := cfrealtime.New(cfg.CFCallsAppID, cfg.CFCallsAppToken)
		authCfg := httpx.AuthConfig{
			AllowedOrigins: cfg.AllowedOrigins,
			JWTSecret:      cfg.JWTSecret,
			AccessTokenTTL: cfg.AccessTokenTTL,
			SecureCookie:   cfg.SecureCookie,
		}
		httpx.SFUHandlers(mux, hub, sfuRegistry, cfCalls, authCfg, callRoomGate)
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
		mailer := email.NewFromEnv()
		bookingDeps := httpx.BookingDeps{
			Mailer:       mailer,
			PublicAppURL: cfg.PublicAppURL,
			RoomWindow: httpx.BookingRoomWindow{
				OpenBefore: cfg.BookingRoomOpenBefore,
				CloseAfter: cfg.BookingRoomCloseAfter,
			},
		}
		getBookingStatus = func(ctx context.Context, room string) (httpx.RoomStatusResult, bool) {
			b, err := st.GetBookingByMeetCode(ctx, room)
			if err != nil {
				// not a booking room — let instant-room path handle it
				return httpx.RoomStatusResult{}, false
			}
			access := httpx.BookingRoomAccessFor(*b, time.Now().UTC(), bookingDeps.RoomWindow)
			result := httpx.RoomStatusResult{Status: access.Status, Message: access.Message}
			if access.Status == "too_early" && !access.OpensAt.IsZero() {
				t := access.OpensAt.UTC()
				result.OpensAt = &t
			}
			return result, true
		}

		roomGate = func(ctx context.Context, room string, activate bool) error {
			if b, err := st.GetBookingByMeetCode(ctx, room); err == nil {
				access := httpx.BookingRoomAccessFor(*b, time.Now().UTC(), bookingDeps.RoomWindow)
				if access.Status == "open" {
					return nil
				}
				if access.Status == "too_early" {
					return roomaccess.ErrNotStarted
				}
				return roomaccess.ErrExpired
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if activate {
				return instantRooms.ActivateOrAllow(room, time.Now().UTC(), cfg.InstantRoomEmptyGrace)
			}
			return instantRooms.AllowActive(room, time.Now().UTC(), cfg.InstantRoomEmptyGrace)
		}

		httpx.AuthHandlers(mux, st, authCfg)
		httpx.MeHandlers(mux, st, authCfg)
		httpx.BookingHandlers(mux, st, authCfg, bookingDeps)
		httpx.SlotHandlers(mux, st, authCfg, bookingDeps)
		httpx.HoldHandlers(mux, st, authCfg)
		log.Println("Auth endpoints enabled")
		log.Println("Scheduling endpoints enabled: /me/availability, /me/event-types")
		log.Println("Booking endpoints enabled: /bookings, /me/bookings")
		log.Println("Public slot endpoint enabled: /u/{slug}/{event}/slots")
		log.Println("Slot-hold endpoints enabled: /holds, /holds/{token}")
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
