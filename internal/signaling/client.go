package signaling

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/coder/websocket"
)

type Client struct {
	id   string
	conn *websocket.Conn
	hub  *Hub
	send chan []byte
	room string
}

func (c *Client) writePump(ctx context.Context) {
	defer c.conn.Close(websocket.StatusNormalClosure, "")
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (c *Client) readPump(ctx context.Context) {
	defer c.hub.leaveAll(c)
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.sendError("invalid JSON")
			continue
		}
		c.handle(&env)
	}
}

func (c *Client) handle(env *Envelope) {
	switch env.Type {
	case MsgJoin:
		if env.Room == "" {
			c.sendError("join requires room")
			return
		}
		c.hub.join(c, env.Room)
	case MsgLeave:
		c.hub.leaveAll(c)
	case MsgSignal:
		if env.To == "" {
			c.sendError("signal requires 'to'")
			return
		}
		c.hub.forwardSignal(c, env)
	default:
		c.sendError("unknown message type: " + string(env.Type))
	}
}

func (c *Client) sendJSON(env *Envelope) {
	b, err := json.Marshal(env)
	if err != nil {
		log.Printf("marshal: %v", err)
		return
	}
	select {
	case c.send <- b:
	default:
	}
}

func (c *Client) sendError(msg string) {
	data, _ := json.Marshal(ErrorData{Message: msg})
	c.sendJSON(&Envelope{Type: MsgError, Data: data})
}
