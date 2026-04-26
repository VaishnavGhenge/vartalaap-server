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
	MsgPeerState  MsgType = "peer-state"
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

type PeerInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Audio bool   `json:"audio"`
	Video bool   `json:"video"`
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
	PeerID string `json:"peerId"`
	Name   string `json:"name"`
	Audio  bool   `json:"audio"`
	Video  bool   `json:"video"`
}

type PeerLeftData struct {
	PeerID string `json:"peerId"`
}

type PeerStateData struct {
	Audio   bool `json:"audio"`
	Video   bool `json:"video"`
	Speaking bool `json:"speaking,omitempty"`
}

type ErrorData struct {
	Message string `json:"message"`
}
