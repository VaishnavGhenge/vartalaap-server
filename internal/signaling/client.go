package signaling

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
	"github.com/vaishnavghenge/vartalaap-server/internal/quality"
)

const (
	sendQueueTimeout = 2 * time.Second
	readIdleTimeout  = 60 * time.Second
)

type Client struct {
	id   string
	conn *websocket.Conn
	hub  *Hub
	send chan []byte
	room string

	mu            sync.RWMutex
	name          string
	audio         bool
	video         bool
	screenSharing bool
	presenceID    string
}

func (c *Client) info() PeerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return PeerInfo{ID: c.id, Name: c.name, Audio: c.audio, Video: c.video, ScreenSharing: c.screenSharing}
}

func (c *Client) setState(audio, video bool, screenSharing bool) {
	c.mu.Lock()
	c.audio = audio
	c.video = video
	c.screenSharing = screenSharing
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
	metrics.ActivePeers.Inc()
	slog.Info("ws_connect", "peer_id", c.id)
	defer func() {
		metrics.ActivePeers.Dec()
		room := c.room
		c.hub.leaveAll(c)
		quality.Default.Delete(c.id)
		slog.Info("ws_disconnect", "peer_id", c.id, "room", room)
	}()
	for {
		rctx, cancel := context.WithTimeout(ctx, readIdleTimeout)
		_, data, err := c.conn.Read(rctx)
		cancel()
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
		c.presenceID = jd.PresenceID
		c.mu.Unlock()
		metrics.JoinsTotal.Inc()
		slog.Info("ws_msg", "type", "join", "peer_id", c.id, "presence_id", jd.PresenceID, "room", env.Room, "name", jd.Name, "audio", jd.Audio, "video", jd.Video)
		c.hub.join(c, env.Room)
	case MsgLeave:
		slog.Info("ws_msg", "type", "leave", "peer_id", c.id, "room", c.room)
		c.hub.leaveAll(c)
	case MsgPeerState:
		var st PeerStateData
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &st)
		}
		slog.Info("ws_msg", "type", "peer-state", "peer_id", c.id, "room", c.room, "audio", st.Audio, "video", st.Video, "speaking", st.Speaking, "screen_sharing", st.ScreenSharing)
		c.setState(st.Audio, st.Video, st.ScreenSharing)
		c.hub.broadcastState(c, st)
	case MsgSignal:
		if env.To == "" {
			c.sendError("signal requires 'to'")
			return
		}
		st := signalSubtype(env.Data)
		metrics.SignalsTotal.WithLabelValues(st).Inc()
		slog.Info("ws_msg", "type", "signal", "peer_id", c.id, "room", c.room, "to", env.To, "signal_type", st)
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
			quality.Default.Set(quality.PeerReport{
				PeerID:              c.id,
				Room:                room,
				Quality:             string(p.Quality),
				NetworkPressure:     string(p.NetworkPressure),
				RoundTripTimeMs:     p.RoundTripTimeMs,
				PacketLossPercent:   p.PacketLossPercent,
				OutboundBitrateKbps: int(p.OutboundBitrateKbps),
				InboundBitrateKbps:  int(p.InboundBitrateKbps),
				CandidateType:       string(p.CandidateType),
				JitterMs:            p.JitterMs,
				EncodingLevel:       p.EncodingLevel,
				VideoHeld:           p.VideoHeld,
			})
			args = append(args,
				slog.Group("peer_"+p.PeerID,
					"remote_id", p.PeerID,
					"quality", p.Quality,
					"network_pressure", p.NetworkPressure,
					"rtt_ms", p.RoundTripTimeMs,
					"loss_pct", p.PacketLossPercent,
					"out_kbps", p.OutboundBitrateKbps,
					"in_kbps", p.InboundBitrateKbps,
					"candidate", p.CandidateType,
					"jitter_ms", p.JitterMs,
					"encoding_level", p.EncodingLevel,
					"video_held", p.VideoHeld,
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
	if !c.enqueue(b) {
		slog.Warn("signaling_send_timeout", "peer_id", c.id, "type", env.Type)
	}
}

func (c *Client) enqueue(msg []byte) bool {
	timer := time.NewTimer(sendQueueTimeout)
	defer timer.Stop()

	select {
	case c.send <- msg:
		return true
	case <-timer.C:
		return false
	}
}

func (c *Client) enqueueBestEffort(msg []byte) bool {
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

func (c *Client) sendError(msg string) {
	data, _ := json.Marshal(ErrorData{Message: msg})
	c.sendJSON(&Envelope{Type: MsgError, Data: data})
}

func signalSubtype(data json.RawMessage) string {
	var v struct {
		Type      string          `json:"type"`
		Candidate json.RawMessage `json:"candidate"`
	}
	if len(data) == 0 || json.Unmarshal(data, &v) != nil {
		return "unknown"
	}
	if v.Type != "" {
		return v.Type // "offer" or "answer"
	}
	if len(v.Candidate) > 0 {
		return "candidate"
	}
	return "unknown"
}
