package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
	"github.com/vaishnavghenge/vartalaap-server/internal/quality"
)

const (
	sendQueueTimeout = 2 * time.Second
	readIdleTimeout  = 60 * time.Second

	// sfu-announce bounds: a peer publishes at most audio + video + screen,
	// and CF session/track identifiers are UUID-sized.
	maxAnnouncedTracks = 8
	maxSfuIDLen        = 256
)

var roomIDPattern = regexp.MustCompile(`^[a-z2-9]{3}-[a-z2-9]{4}-[a-z2-9]{3}$`)

type Client struct {
	id       string
	conn     *websocket.Conn
	hub      *Hub
	send     chan []byte
	room     string
	roomGate RoomGate

	mu            sync.RWMutex
	name          string
	audio         bool
	video         bool
	screenSharing bool
	videoHeld     bool
	presenceID    string
	// needsAdmit is true when the peer joined without an SFU token and will
	// knock. hub.join skips peer-joined broadcast until knockAdmit clears this.
	needsAdmit bool
}

func (c *Client) info() PeerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return PeerInfo{ID: c.id, Name: c.name, Audio: c.audio, Video: c.video, ScreenSharing: c.screenSharing, VideoHeld: c.videoHeld}
}

func (c *Client) setState(audio, video bool, screenSharing bool, videoHeld *bool) {
	c.mu.Lock()
	c.audio = audio
	c.video = video
	c.screenSharing = screenSharing
	if videoHeld != nil {
		c.videoHeld = *videoHeld
	}
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
	// Count every handled message. The label is the message type so we can
	// detect signaling storms, peer-state churn, or a missing client-metric
	// from a peer that should have completed setup.
	metrics.SignalsTotal.WithLabelValues(string(env.Type)).Inc()
	switch env.Type {
	case MsgJoin:
		if env.Room == "" {
			c.sendError("join requires room")
			return
		}
		if !roomIDPattern.MatchString(env.Room) {
			c.sendError("invalid room")
			return
		}
		if c.roomGate != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := c.roomGate(ctx, env.Room)
			cancel()
			if err != nil {
				var gateErr *RoomGateError
				if errors.As(err, &gateErr) {
					c.sendErrorCode(gateErr.Code, gateErr.Message)
				} else {
					c.sendErrorCode("ROOM_UNAVAILABLE", "This room is not available yet.")
				}
				return
			}
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
		c.needsAdmit = jd.NeedsAdmit
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
		slog.Info("ws_msg", "type", "peer-state", "peer_id", c.id, "room", c.room, "to", env.To, "audio", st.Audio, "video", st.Video, "speaking", st.Speaking, "screen_sharing", st.ScreenSharing, "video_held", st.VideoHeld)
		if env.To != "" {
			c.hub.forwardState(c, env.To, st)
			return
		}
		c.setState(st.Audio, st.Video, st.ScreenSharing, st.VideoHeld)
		c.hub.broadcastState(c, st)
	case MsgPing:
		c.sendJSON(&Envelope{Type: MsgPong})
	case MsgSync:
		c.hub.SendSnapshot(c)
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
	case MsgClientMetric:
		var m ClientMetricData
		if len(env.Data) > 0 {
			if err := json.Unmarshal(env.Data, &m); err != nil {
				return
			}
		}
		c.observeClientMetric(m)
	case MsgSfuAnnounce:
		var d SfuTracksData
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &d)
		}
		// Bound the payload: a peer publishes at most a handful of tracks
		// (audio, video, screen). Reject junk instead of storing it — the
		// stored set is replayed to every later joiner.
		if d.SessionID == "" || len(d.SessionID) > maxSfuIDLen ||
			len(d.Tracks) == 0 || len(d.Tracks) > maxAnnouncedTracks {
			c.sendError("invalid sfu-announce")
			return
		}
		for _, tr := range d.Tracks {
			if tr.TrackName == "" || len(tr.TrackName) > maxSfuIDLen {
				c.sendError("invalid sfu-announce")
				return
			}
		}
		slog.Info("ws_msg", "type", "sfu-announce", "peer_id", c.id, "room", c.room, "session", d.SessionID, "tracks", len(d.Tracks))
		c.hub.AnnounceSfuTracks(c, d)
	case MsgKnock:
		c.hub.knock(c)
	case MsgKnockAdmit:
		var d KnockAdmitData
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &d)
		}
		if d.PeerID == "" {
			c.sendError("peerId required for knock-admit")
			return
		}
		c.hub.knockAdmit(c, d.PeerID)
	default:
		c.sendError("unknown message type: " + string(env.Type))
	}
}

// observeClientMetric translates one browser-side observation into a Prometheus
// observation. Validation is strict on purpose — a malformed value would
// otherwise pollute the histogram permanently. We bound values to a sane
// ceiling (60s) so a bug in the client clock can't blow out the buckets.
func (c *Client) observeClientMetric(m ClientMetricData) {
	const maxObservation = 60.0 // anything beyond this is a clock skew or bug
	v := m.Value
	if v < 0 || v != v { // negative or NaN
		return
	}
	if v > maxObservation {
		v = maxObservation
	}
	switch m.Name {
	case "time_to_first_media":
		metrics.TimeToFirstMedia.Observe(v)
		slog.Info("client_metric",
			"peer_id", c.id, "room", c.room,
			"name", m.Name, "value_s", v,
		)
	case "call_setup_phase":
		phase := m.Phase
		switch phase {
		case "ice_gather", "pub_connected", "sub_connected", "first_media":
			metrics.CallSetupPhase.WithLabelValues(phase).Observe(v)
		default:
			return
		}
		slog.Info("client_metric",
			"peer_id", c.id, "room", c.room,
			"name", m.Name, "phase", phase, "value_s", v,
		)
	case "call_attempt":
		result := m.Result
		switch result {
		// success and slow both mean the call worked; they differ only in
		// whether media beat the SLO ceiling. failed means it ended with no
		// media while a peer was publishing. abandoned means nothing was owed.
		//
		// "timeout" is legacy: the client stopped emitting it when the ceiling
		// became an observation rather than a verdict. Still accepted so
		// historical series stay readable and an older client is not silently
		// dropped mid-rollout.
		case "success", "slow", "failed", "error", "abandoned", "timeout":
			metrics.CallAttempts.WithLabelValues(result).Inc()
		default:
			return
		}
		slog.Info("client_metric",
			"peer_id", c.id, "room", c.room,
			"name", m.Name, "result", result,
		)
	case "call_setup_failure":
		// Whitelist the reason label — same cardinality guard as phase/result.
		// An unknown reason is dropped rather than counted, so a buggy client
		// can't mint new label series. Keep in sync with CallFailureReason in
		// the client protocol.ts.
		reason := m.Reason
		switch reason {
		case "no_tracks_announced", "tracks_announced_not_pulled", "pull_errored", "peers_present_none_publishing", "unknown":
			metrics.CallSetupFailures.WithLabelValues(reason).Inc()
		default:
			return
		}
		slog.Info("client_metric",
			"peer_id", c.id, "room", c.room,
			"name", m.Name, "reason", reason,
		)
	case "sfu_repair":
		// Same cardinality guard as everything else here: stage, rung and
		// outcome are all whitelisted so a buggy client cannot mint label
		// series. Rung is bounded to the ladder SfuSession actually implements.
		stage, outcome := m.Stage, m.Outcome
		if stage != "publish" && stage != "subscribe" {
			return
		}
		if outcome != "attempted" && outcome != "recovered" {
			return
		}
		if m.Rung < 1 || m.Rung > 2 {
			return
		}
		metrics.SfuRepairs.WithLabelValues(stage, strconv.Itoa(m.Rung), outcome).Inc()
		slog.Info("client_metric",
			"peer_id", c.id, "room", c.room,
			"name", m.Name, "stage", stage, "rung", m.Rung, "outcome", outcome,
		)
	default:
		// Unknown metric name. Logging at debug level avoids amplifying a buggy
		// client into a log flood — at info level a misbehaving browser could
		// emit thousands of lines/s.
		slog.Debug("client_metric_unknown",
			"peer_id", c.id, "name", m.Name,
		)
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
	c.sendErrorCode("", msg)
}

func (c *Client) sendErrorCode(code, msg string) {
	data, _ := json.Marshal(ErrorData{Message: msg, Code: code})
	c.sendJSON(&Envelope{Type: MsgError, Data: data})
}
