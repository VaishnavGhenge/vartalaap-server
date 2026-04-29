package signaling

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Client struct {
	id   string
	conn *websocket.Conn
	hub  *Hub
	send chan []byte
	room string

	mu    sync.RWMutex
	name  string
	audio bool
	video bool
}

func (c *Client) info() PeerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return PeerInfo{ID: c.id, Name: c.name, Audio: c.audio, Video: c.video}
}

func (c *Client) setState(audio, video bool) {
	c.mu.Lock()
	c.audio = audio
	c.video = video
	c.mu.Unlock()
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
		var jd JoinData
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &jd)
		}
		c.mu.Lock()
		c.name = jd.Name
		c.audio = jd.Audio
		c.video = jd.Video
		c.mu.Unlock()
		c.hub.join(c, env.Room)
	case MsgLeave:
		c.hub.leaveAll(c)
	case MsgPeerState:
		var st PeerStateData
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &st)
		}
		c.setState(st.Audio, st.Video)
		c.hub.broadcastState(c, st)
	case MsgSignal:
		if env.To == "" {
			c.sendError("signal requires 'to'")
			return
		}
		c.hub.forwardSignal(c, env)
	case MsgPing:
		c.sendJSON(&Envelope{Type: MsgPong})
	case MsgStatsReport:
		var rd StatsReportData
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &rd)
		}
		c.mu.RLock()
		name := c.name
		room := c.room
		c.mu.RUnlock()
		args := []any{
			"room", room,
			"peer_id", c.id,
			"peer_name", name,
		}
		for _, p := range rd.Peers {
			args = append(args,
				slog.Group("peer_"+p.PeerID,
					"remote_id", p.PeerID,
					"quality", p.Quality,
					"rtt_ms", p.RoundTripTimeMs,
					"loss_pct", p.PacketLossPercent,
					"out_kbps", p.OutboundBitrateKbps,
					"in_kbps", p.InboundBitrateKbps,
					"candidate", p.CandidateType,
					"jitter_ms", p.JitterMs,
				),
			)
		}
		slog.Info("stats_report", args...)
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
