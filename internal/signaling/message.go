package signaling

import "encoding/json"

type MsgType string

const (
	MsgWelcome     MsgType = "welcome"
	MsgJoin        MsgType = "join"
	MsgJoined      MsgType = "joined"
	MsgLeave       MsgType = "leave"
	MsgPeerJoined  MsgType = "peer-joined"
	MsgPeerLeft    MsgType = "peer-left"
	MsgPeerState   MsgType = "peer-state"
	MsgSignal      MsgType = "signal"
	MsgError       MsgType = "error"
	MsgPing        MsgType = "ping"
	MsgPong        MsgType = "pong"
	MsgStatsReport MsgType = "stats-report"
)

type Envelope struct {
	Type MsgType         `json:"type"`
	Room string          `json:"room,omitempty"`
	From string          `json:"from,omitempty"`
	To   string          `json:"to,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type PeerInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Audio         bool   `json:"audio"`
	Video         bool   `json:"video"`
	ScreenSharing bool   `json:"screenSharing"`
}

type JoinData struct {
	Name  string `json:"name"`
	Audio bool   `json:"audio"`
	Video bool   `json:"video"`
}

type JoinedData struct {
	Peers []PeerInfo `json:"peers"`
}

type PeerJoinedData struct {
	PeerID        string `json:"peerId"`
	Name          string `json:"name"`
	Audio         bool   `json:"audio"`
	Video         bool   `json:"video"`
	ScreenSharing bool   `json:"screenSharing"`
}

type PeerLeftData struct {
	PeerID string `json:"peerId"`
}

type PeerStateData struct {
	Audio         bool `json:"audio"`
	Video         bool `json:"video"`
	Speaking      bool `json:"speaking"`
	ScreenSharing bool `json:"screenSharing"`
}

type ErrorData struct {
	Message string `json:"message"`
}

type StatsReportPeer struct {
	PeerID              string  `json:"peerId"`
	Quality             string  `json:"quality"`
	RoundTripTimeMs     float64 `json:"roundTripTimeMs"`
	PacketLossPercent   float64 `json:"packetLossPercent"`
	OutboundBitrateKbps int     `json:"outboundBitrateKbps"`
	InboundBitrateKbps  int     `json:"inboundBitrateKbps"`
	CandidateType       string  `json:"candidateType"`
	JitterMs            float64 `json:"jitterMs"`
	FrameWidth          *int    `json:"frameWidth,omitempty"`
	FrameHeight         *int    `json:"frameHeight,omitempty"`
	FramesPerSecond     *int    `json:"framesPerSecond,omitempty"`
}

type StatsReportData struct {
	Peers []StatsReportPeer `json:"peers"`
}
