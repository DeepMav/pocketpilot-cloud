// SPDX-License-Identifier: Apache-2.0
package signal

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/google/uuid"
)

type PeerID string
type SessionID string

type Hub struct {
	iceServers []IceServer

	mu       sync.RWMutex
	peers    map[PeerID]*Peer
	sessions map[SessionID]*session
}

type session struct {
	id        SessionID
	initiator PeerID
	target    PeerID
}

func NewHub(ice []IceServer) *Hub {
	return &Hub{
		iceServers: ice,
		peers:      map[PeerID]*Peer{},
		sessions:   map[SessionID]*session{},
	}
}

func (h *Hub) IceServers() []IceServer { return h.iceServers }

var (
	ErrPeerAlreadyConnected = errors.New("peer already connected")
	ErrTargetUnknown        = errors.New("target peer not connected")
	ErrSessionUnknown       = errors.New("session not found")
	ErrNotMember            = errors.New("peer not in session")
)

func (h *Hub) Register(p *Peer) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.peers[p.ID]; exists {
		// TODO: kick old conn instead of rejecting new one (reconnect after net drop).
		return ErrPeerAlreadyConnected
	}
	h.peers[p.ID] = p
	slog.Info("peer registered", "peer", p.ID, "role", p.Role)
	return nil
}

func (h *Hub) Unregister(id PeerID) {
	type notice struct {
		peer *Peer
		sid  SessionID
	}
	var notify []notice

	h.mu.Lock()
	delete(h.peers, id)
	for sid, s := range h.sessions {
		if s.initiator != id && s.target != id {
			continue
		}
		otherID := s.initiator
		if otherID == id {
			otherID = s.target
		}
		if p, ok := h.peers[otherID]; ok {
			notify = append(notify, notice{p, sid})
		}
		delete(h.sessions, sid)
	}
	h.mu.Unlock()

	// Notify *outside* the lock — Send may close peers as a side effect.
	for _, n := range notify {
		n.peer.Send(PeerGoneMsg{Kind: KindPeerGone, Session: string(n.sid), Reason: "peer_disconnected"})
	}
	slog.Info("peer unregistered", "peer", id)
}

// OpenSession creates a session pairing initiator with target. The caller is
// responsible for delivering session.ack to both parties.
func (h *Hub) OpenSession(initiator, target PeerID) (SessionID, *Peer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	targetPeer, ok := h.peers[target]
	if !ok {
		return "", nil, ErrTargetUnknown
	}
	sid := SessionID("s_" + uuid.NewString())
	h.sessions[sid] = &session{id: sid, initiator: initiator, target: target}
	slog.Info("session opened", "session", sid, "initiator", initiator, "target", target)
	return sid, targetPeer, nil
}

// OtherPeer returns the counterparty in a session, validating membership.
func (h *Hub) OtherPeer(sid SessionID, self PeerID) (*Peer, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.sessions[sid]
	if !ok {
		return nil, ErrSessionUnknown
	}
	var otherID PeerID
	switch self {
	case s.initiator:
		otherID = s.target
	case s.target:
		otherID = s.initiator
	default:
		return nil, ErrNotMember
	}
	p, ok := h.peers[otherID]
	if !ok {
		return nil, ErrTargetUnknown
	}
	return p, nil
}

func (h *Hub) CloseSession(sid SessionID, by PeerID, reason string) {
	h.mu.Lock()
	s, ok := h.sessions[sid]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.sessions, sid)
	var otherID PeerID
	switch by {
	case s.initiator:
		otherID = s.target
	case s.target:
		otherID = s.initiator
	default:
		h.mu.Unlock()
		return
	}
	other := h.peers[otherID]
	h.mu.Unlock()
	if other != nil {
		other.Send(PeerGoneMsg{Kind: KindPeerGone, Session: string(sid), Reason: reason})
	}
}
