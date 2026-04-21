package signaling

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/coder/websocket"
)

func NewHandler(hub *Hub, allowedOrigins []string) http.HandlerFunc {
	hosts := originHosts(allowedOrigins)
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: hosts,
		})
		if err != nil {
			return
		}
		c := &Client{
			id:   newPeerID(),
			conn: conn,
			hub:  hub,
			send: make(chan []byte, 32),
		}
		welcome, _ := json.Marshal(Envelope{Type: MsgWelcome, From: c.id})
		c.send <- welcome

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		go c.writePump(ctx)
		c.readPump(ctx)
	}
}

func originHosts(origins []string) []string {
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		h := o
		for _, p := range []string{"https://", "http://", "wss://", "ws://"} {
			h = strings.TrimPrefix(h, p)
		}
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}
