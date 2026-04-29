package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/vaishnavghenge/vartalaap-server/internal/cfturn"
	"github.com/vaishnavghenge/vartalaap-server/internal/config"
	"github.com/vaishnavghenge/vartalaap-server/internal/httpx"
	"github.com/vaishnavghenge/vartalaap-server/internal/signaling"
)

func main() {
	// Structured JSON logs so platforms like Fly/Railway/Datadog/Loki can
	// parse and index fields without custom parsing rules.
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

	hub := signaling.NewHub()
	cf := cfturn.New(cfg.CFTurnKeyID, cfg.CFTurnAPIToken)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ws", signaling.NewHandler(hub, cfg.AllowedOrigins))
	mux.HandleFunc("/ice-servers", httpx.NewIceHandler(cf, cfg.AllowedOrigins))

	sentinel := sentryhttp.New(sentryhttp.Options{Repanic: true})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           sentinel.Handle(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("vartalaap-server listening on :%s", cfg.Port)
	log.Fatal(srv.ListenAndServe())
}
