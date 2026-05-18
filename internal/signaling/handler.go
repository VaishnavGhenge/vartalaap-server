package signaling

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/coder/websocket"
)

type RoomGate func(ctx context.Context, room string) error

type RoomGateError struct {
	Code    string
	Message string
}

func (e *RoomGateError) Error() string {
	return e.Message
}

func NewHandler(hub *Hub, allowedOrigins []string, gates ...RoomGate) http.HandlerFunc {
	hosts := originHosts(allowedOrigins)
	var gate RoomGate
	if len(gates) > 0 {
		gate = gates[0]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: hosts,
		})
		if err != nil {
			return
		}
		c := &Client{
			id:       newPeerID(),
			conn:     conn,
			hub:      hub,
			send:     make(chan []byte, 32),
			roomGate: gate,
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
