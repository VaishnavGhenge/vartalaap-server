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
	MsgError       MsgType = "error"
	MsgPing        MsgType = "ping"
	MsgPong        MsgType = "pong"
	MsgStatsReport MsgType = "stats-report"
	// MsgSfuTracks is broadcast by the server after a peer publishes local tracks
	// to the Cloudflare Realtime SFU. Receivers should subscribe to the announced tracks.
	// Sender: HTTP SFU handler. Receiver: all other peers in the room.
	MsgSfuTracks MsgType = "sfu-tracks"
	// MsgClientMetric carries a single observation from the browser into the
	// server-side Prometheus histograms. Used for measurements only the client
	// can take — time-to-first-media, ICE gather time, etc. The server is the
	// sole writer of the underlying Prom metrics; clients only emit observations.
	// Sender: browser. Receiver: signaling client handler.
	MsgClientMetric MsgType = "client-metric"
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
	VideoHeld     bool   `json:"videoHeld"`
}

type JoinData struct {
	Name       string `json:"name"`
	Audio      bool   `json:"audio"`
	Video      bool   `json:"video"`
	PresenceID string `json:"presenceId,omitempty"`
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
	VideoHeld     bool   `json:"videoHeld"`
}

type PeerLeftData struct {
	PeerID string `json:"peerId"`
}

type PeerStateData struct {
	Audio         bool  `json:"audio"`
	Video         bool  `json:"video"`
	Speaking      bool  `json:"speaking"`
	ScreenSharing bool  `json:"screenSharing"`
	VideoHeld     *bool `json:"videoHeld,omitempty"`
}

type ErrorData struct {
	Message string `json:"message"`
}

type StatsReportPeer struct {
	PeerID              string  `json:"peerId"`
	Quality             string  `json:"quality"`
	NetworkPressure     string  `json:"networkPressure"`
	RoundTripTimeMs     float64 `json:"roundTripTimeMs"`
	PacketLossPercent   float64 `json:"packetLossPercent"`
	OutboundBitrateKbps int     `json:"outboundBitrateKbps"`
	InboundBitrateKbps  int     `json:"inboundBitrateKbps"`
	CandidateType       string  `json:"candidateType"`
	JitterMs            float64 `json:"jitterMs"`
	EncodingLevel       int     `json:"encodingLevel"`
	VideoHeld           bool    `json:"videoHeld"`
	FrameWidth          *int    `json:"frameWidth,omitempty"`
	FrameHeight         *int    `json:"frameHeight,omitempty"`
	FramesPerSecond     *int    `json:"framesPerSecond,omitempty"`
}

type StatsReportData struct {
	Peers []StatsReportPeer `json:"peers"`
}

// ClientMetricData is a single observation emitted by the browser. Only the
// name and value are required; phase narrows call-setup sub-phases and result
// labels the outcome of a setup attempt.
//
// Supported names:
//
//	time_to_first_media   value = seconds from join sent to first remote frame
//	call_setup_phase      value = seconds; phase ∈ {ice_gather, pub_connected,
//	                      sub_connected, first_media}
//	call_attempt          value ignored; result ∈ {success, timeout, error, abandoned}
//
// Unknown names are dropped with a debug log so a buggy client can't pollute
// the histogram registry.
type ClientMetricData struct {
	Name    string  `json:"name"`
	Value   float64 `json:"value"`
	Phase   string  `json:"phase,omitempty"`
	Result  string  `json:"result,omitempty"`
}

type SfuTrackInfo struct {
	TrackName string `json:"trackName"`
	Mid       string `json:"mid,omitempty"`
}

type SfuTracksData struct {
	SessionID string         `json:"sessionId"`
	Tracks    []SfuTrackInfo `json:"tracks"`
}
