package signaling

import "encoding/json"

type MsgType string

const (
	MsgWelcome    MsgType = "welcome"
	MsgJoin       MsgType = "join"
	MsgJoined     MsgType = "joined"
	MsgLeave      MsgType = "leave"
	MsgPeerJoined MsgType = "peer-joined"
	MsgPeerLeft   MsgType = "peer-left"
	MsgSignal     MsgType = "signal"
	MsgError      MsgType = "error"
)

type Envelope struct {
	Type MsgType         `json:"type"`
	Room string          `json:"room,omitempty"`
	From string          `json:"from,omitempty"`
	To   string          `json:"to,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type JoinedData struct {
	Peers []string `json:"peers"`
}

type PeerEventData struct {
	PeerID string `json:"peerId"`
}

type ErrorData struct {
	Message string `json:"message"`
}
