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

	// MsgKnock is sent by a guest browser that holds no JWT and wants SFU access.
	// Sender: guest browser. Receiver: server. Requests SFU access when no JWT is held.
	MsgKnock MsgType = "knock"
	// MsgKnockRequest is forwarded by the server to all other peers in the room
	// so they can decide whether to admit the knocking guest.
	// Sender: server. Receiver: all other peers in room. Payload: KnockRequestData.
	MsgKnockRequest MsgType = "knock-request"
	// MsgKnockAdmit is sent by any peer in the room to grant the knocking guest access.
	// Sender: any peer in room. Receiver: server. Payload: KnockAdmitData.
	MsgKnockAdmit MsgType = "knock-admit"
	// MsgKnockGranted is sent by the server to the knocking guest after a peer admits them.
	// Sender: server. Receiver: knocking guest only. Payload: KnockGrantedData.
	MsgKnockGranted MsgType = "knock-granted"
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
	// NeedsAdmit signals that this peer has no SFU token and will knock
	// immediately after joining. The hub defers peer-joined until knockAdmit
	// so the host never sees an un-admitted guest tile.
	NeedsAdmit bool `json:"needsAdmit,omitempty"`
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
	Code    string `json:"code,omitempty"`
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
//	call_setup_failure    value ignored; reason ∈ {no_tracks_announced,
//	                      tracks_announced_not_pulled, pull_errored,
//	                      peers_present_none_publishing, unknown} — the
//	                      errors-by-type breakdown of a setup timeout
//
// Unknown names are dropped with a debug log so a buggy client can't pollute
// the histogram registry.
type ClientMetricData struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Phase  string  `json:"phase,omitempty"`
	Result string  `json:"result,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

type KnockRequestData struct {
	PeerID string `json:"peerId"`
	Name   string `json:"name"`
}

type KnockAdmitData struct {
	PeerID string `json:"peerId"` // the knocking peer to admit
}

type KnockGrantedData struct {
	SfuToken string `json:"sfuToken"`
}

type SfuTrackInfo struct {
	TrackName string `json:"trackName"`
	Mid       string `json:"mid,omitempty"`
}

type SfuTracksData struct {
	SessionID string         `json:"sessionId"`
	Tracks    []SfuTrackInfo `json:"tracks"`
}
