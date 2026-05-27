// SPDX-License-Identifier: Apache-2.0
package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	sendQueueSize = 32
	writeTimeout  = 5 * time.Second
	pingInterval  = 25 * time.Second
)

type Peer struct {
	ID   PeerID
	Role string

	conn *websocket.Conn
	send chan any

	closeOnce sync.Once
	closeCh   chan struct{}
}

func newPeer(id PeerID, role string, conn *websocket.Conn) *Peer {
	return &Peer{
		ID:      id,
		Role:    role,
		conn:    conn,
		send:    make(chan any, sendQueueSize),
		closeCh: make(chan struct{}),
	}
}

// Send queues an outbound message. If the queue is full the peer is closed —
// a stuck client must not back-pressure the hub.
func (p *Peer) Send(msg any) {
	select {
	case p.send <- msg:
	default:
		slog.Warn("peer send queue full, closing", "peer", p.ID)
		p.close()
	}
}

func (p *Peer) close() {
	p.closeOnce.Do(func() { close(p.closeCh) })
}

func (p *Peer) run(ctx context.Context, hub *Hub) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.writeLoop(ctx) }()
	go func() { defer wg.Done(); p.readLoop(ctx, hub); cancel() }()

	select {
	case <-ctx.Done():
	case <-p.closeCh:
		cancel()
	}
	_ = p.conn.Close(websocket.StatusNormalClosure, "")
	wg.Wait()
	hub.Unregister(p.ID)
}

func (p *Peer) writeLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.closeCh:
			return
		case msg := <-p.send:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := wsjson.Write(wctx, p.conn, msg)
			cancel()
			if err != nil {
				slog.Debug("ws write failed", "peer", p.ID, "err", err)
				p.close()
				return
			}
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := p.conn.Ping(pctx)
			cancel()
			if err != nil {
				slog.Debug("ws ping failed", "peer", p.ID, "err", err)
				p.close()
				return
			}
		}
	}
}

func (p *Peer) readLoop(ctx context.Context, hub *Hub) {
	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, p.conn, &raw); err != nil {
			slog.Debug("ws read ended", "peer", p.ID, "err", err)
			return
		}
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			p.Send(ErrMsg{Kind: KindErr, Code: "bad_json", Message: err.Error()})
			continue
		}
		if err := p.dispatch(hub, env.Kind, raw); err != nil {
			p.Send(ErrMsg{Kind: KindErr, Code: "dispatch", Message: err.Error()})
		}
	}
}

func (p *Peer) dispatch(hub *Hub, kind string, raw json.RawMessage) error {
	switch kind {
	case KindPing:
		p.Send(map[string]string{"kind": KindPong})
		return nil

	case KindSessionReq:
		var m SessionReqMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		sid, target, err := hub.OpenSession(p.ID, PeerID(m.Peer))
		if err != nil {
			return err
		}
		p.Send(SessionAckMsg{
			Kind:       KindSessionAck,
			Session:    string(sid),
			Peer:       m.Peer,
			IceServers: hub.IceServers(),
			Initiator:  true,
		})
		target.Send(SessionAckMsg{
			Kind:       KindSessionAck,
			Session:    string(sid),
			Peer:       string(p.ID),
			IceServers: hub.IceServers(),
			Initiator:  false,
		})
		return nil

	case KindSDP:
		var m SDPMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		other, err := hub.OtherPeer(SessionID(m.Session), p.ID)
		if err != nil {
			return err
		}
		other.Send(PeerSDPMsg{Kind: KindPeerSDP, Session: m.Session, Role: m.Role, SDP: m.SDP})
		return nil

	case KindICE:
		var m ICEMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		other, err := hub.OtherPeer(SessionID(m.Session), p.ID)
		if err != nil {
			return err
		}
		other.Send(PeerICEMsg{Kind: KindPeerICE, Session: m.Session, Cand: m.Cand})
		return nil

	case KindBye:
		var m ByeMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		hub.CloseSession(SessionID(m.Session), p.ID, m.Reason)
		return nil

	case KindHello:
		return errors.New("hello already processed")

	default:
		return fmt.Errorf("unknown kind: %s", kind)
	}
}
