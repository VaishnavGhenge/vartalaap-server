package main

import (
	"log"
	"net/http"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/cfturn"
	"github.com/vaishnavghenge/vartalaap-server/internal/config"
	"github.com/vaishnavghenge/vartalaap-server/internal/httpx"
	"github.com/vaishnavghenge/vartalaap-server/internal/signaling"
)

func main() {
	cfg := config.Load()
	hub := signaling.NewHub()
	cf := cfturn.New(cfg.CFTurnKeyID, cfg.CFTurnAPIToken)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ws", signaling.NewHandler(hub, cfg.AllowedOrigins))
	mux.HandleFunc("/ice-servers", httpx.NewIceHandler(cf, cfg.AllowedOrigins))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("vartalaap-server listening on :%s", cfg.Port)
	log.Fatal(srv.ListenAndServe())
}
