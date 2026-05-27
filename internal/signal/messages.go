// SPDX-License-Identifier: Apache-2.0
package signal

import "encoding/json"

// Wire vocabulary. Keep this stable — clients depend on these strings.
const (
	// Client → server
	KindHello      = "hello"
	KindSessionReq = "session.req"
	KindSDP        = "sdp"
	KindICE        = "ice"
	KindBye        = "bye"
	KindPing       = "ping"

	// Server → client
	KindHelloOK    = "hello.ok"
	KindSessionAck = "session.ack"
	KindPeerSDP    = "peer.sdp"
	KindPeerICE    = "peer.ice"
	KindPeerGone   = "peer.gone"
	KindPong       = "pong"
	KindErr        = "err"
)

type Envelope struct {
	Kind string `json:"kind"`
}

type IceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// --- client → server ---

type HelloMsg struct {
	Kind  string `json:"kind"`
	Token string `json:"token"`
}

type SessionReqMsg struct {
	Kind string `json:"kind"`
	Peer string `json:"peer"`
}

type SDPMsg struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Role    string `json:"role"` // "offer" or "answer"
	SDP     string `json:"sdp"`
}

type ICEMsg struct {
	Kind    string          `json:"kind"`
	Session string          `json:"session"`
	Cand    json.RawMessage `json:"cand"`
}

type ByeMsg struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Reason  string `json:"reason,omitempty"`
}

// --- server → client ---

type HelloOKMsg struct {
	Kind string `json:"kind"`
	Self string `json:"self"`
}

type SessionAckMsg struct {
	Kind       string      `json:"kind"`
	Session    string      `json:"session"`
	Peer       string      `json:"peer"` // the other party
	IceServers []IceServer `json:"ice_servers"`
	Initiator  bool        `json:"initiator"`
}

type PeerSDPMsg struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Role    string `json:"role"`
	SDP     string `json:"sdp"`
}

type PeerICEMsg struct {
	Kind    string          `json:"kind"`
	Session string          `json:"session"`
	Cand    json.RawMessage `json:"cand"`
}

type PeerGoneMsg struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Reason  string `json:"reason"`
}

type ErrMsg struct {
	Kind    string `json:"kind"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
