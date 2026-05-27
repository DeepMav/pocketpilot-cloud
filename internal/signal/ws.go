// SPDX-License-Identifier: Apache-2.0
package signal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/deepmav/pocketpilot-cloud/internal/token"
)

const (
	helloTimeout = 5 * time.Second
	readLimit    = 128 * 1024 // SDP can run a few KB
)

type WSHandler struct {
	hub      *Hub
	verifier *token.Verifier
}

func NewWSHandler(hub *Hub, v *token.Verifier) *WSHandler {
	return &WSHandler{hub: hub, verifier: v}
}

func (h *WSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// PoC — production should set OriginPatterns and drop this flag.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Warn("ws accept failed", "err", err)
		return
	}
	conn.SetReadLimit(readLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	peer, err := h.acceptHello(ctx, conn)
	if err != nil {
		slog.Info("hello failed", "err", err)
		_ = conn.Close(websocket.StatusPolicyViolation, "hello failed")
		return
	}

	if err := h.hub.Register(peer); err != nil {
		_ = wsjson.Write(ctx, conn, ErrMsg{Kind: KindErr, Code: "duplicate_peer", Message: err.Error()})
		_ = conn.Close(websocket.StatusPolicyViolation, "duplicate peer")
		return
	}

	peer.Send(HelloOKMsg{Kind: KindHelloOK, Self: string(peer.ID)})
	peer.run(ctx, h.hub)
}

func (h *WSHandler) acceptHello(ctx context.Context, conn *websocket.Conn) (*Peer, error) {
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()

	var raw json.RawMessage
	if err := wsjson.Read(hctx, conn, &raw); err != nil {
		return nil, err
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Kind != KindHello {
		return nil, errors.New("first message must be 'hello'")
	}
	var m HelloMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	claims, err := h.verifier.Verify(m.Token)
	if err != nil {
		return nil, err
	}
	return newPeer(PeerID(claims.Subject), string(claims.Role), conn), nil
}
