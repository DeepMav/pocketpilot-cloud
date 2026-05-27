// SPDX-License-Identifier: Apache-2.0

// testpeer is a stub drone client. It logs in to cmd/auth, opens a WebSocket
// to cmd/signal as the WebRTC answerer, accepts the initiator's offer, and
// echoes any DataChannel messages back. Use it during client development
// when you don't have a real PX4 + cmd/relay running.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"

	sig "github.com/deepmav/pocketpilot-cloud/internal/signal"
)

func main() {
	var (
		authURL   = flag.String("auth-url", "http://localhost:8081", "auth service base URL")
		signalURL = flag.String("signal-url", "ws://localhost:8080/v1/signal", "signaling WebSocket URL")
		user      = flag.String("user", "drone-42", "username")
		pass      = flag.String("pass", "drone-42-dev", "password")
	)
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tok, err := login(ctx, *authURL, *user, *pass)
	if err != nil {
		slog.Error("login failed", "err", err)
		os.Exit(1)
	}
	slog.Info("logged in", "user", *user)

	conn, _, err := websocket.Dial(ctx, *signalURL, nil)
	if err != nil {
		slog.Error("ws dial failed", "url", *signalURL, "err", err)
		os.Exit(1)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(128 * 1024)

	if err := wsjson.Write(ctx, conn, sig.HelloMsg{Kind: sig.KindHello, Token: tok}); err != nil {
		slog.Error("hello write failed", "err", err)
		os.Exit(1)
	}

	if err := runLoop(ctx, conn); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("loop ended", "err", err)
	}
}

func runLoop(ctx context.Context, ws *websocket.Conn) error {
	var (
		pc        *webrtc.PeerConnection
		sessionID string
	)
	defer func() {
		if pc != nil {
			_ = pc.Close()
		}
	}()

	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, ws, &raw); err != nil {
			return err
		}
		var env sig.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			slog.Warn("bad envelope", "err", err)
			continue
		}

		switch env.Kind {
		case sig.KindHelloOK:
			var m sig.HelloOKMsg
			_ = json.Unmarshal(raw, &m)
			slog.Info("hello.ok", "self", m.Self)

		case sig.KindSessionAck:
			var m sig.SessionAckMsg
			_ = json.Unmarshal(raw, &m)
			sessionID = m.Session
			slog.Info("session.ack",
				"session", m.Session, "peer", m.Peer,
				"initiator", m.Initiator, "ice_servers", len(m.IceServers))
			if m.Initiator {
				slog.Warn("received initiator=true; testpeer is answerer only")
				continue
			}
			var err error
			pc, err = newPeerConn(ctx, ws, sessionID, m.IceServers)
			if err != nil {
				return fmt.Errorf("pc setup: %w", err)
			}

		case sig.KindPeerSDP:
			var m sig.PeerSDPMsg
			_ = json.Unmarshal(raw, &m)
			if pc == nil {
				slog.Warn("peer.sdp before session.ack, ignoring")
				continue
			}
			if m.Role != "offer" {
				slog.Warn("expected offer", "role", m.Role)
				continue
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer, SDP: m.SDP,
			}); err != nil {
				slog.Error("set remote", "err", err)
				continue
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				slog.Error("create answer", "err", err)
				continue
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				slog.Error("set local", "err", err)
				continue
			}
			if err := wsjson.Write(ctx, ws, sig.SDPMsg{
				Kind: sig.KindSDP, Session: sessionID, Role: "answer", SDP: answer.SDP,
			}); err != nil {
				slog.Error("send answer", "err", err)
			} else {
				slog.Info("answer sent")
			}

		case sig.KindPeerICE:
			var m sig.PeerICEMsg
			_ = json.Unmarshal(raw, &m)
			if pc == nil {
				continue
			}
			var c webrtc.ICECandidateInit
			if err := json.Unmarshal(m.Cand, &c); err != nil {
				slog.Warn("ice unmarshal", "err", err)
				continue
			}
			if err := pc.AddICECandidate(c); err != nil {
				slog.Warn("add ice", "err", err)
			}

		case sig.KindPeerGone:
			var m sig.PeerGoneMsg
			_ = json.Unmarshal(raw, &m)
			slog.Info("peer.gone", "reason", m.Reason)
			if pc != nil {
				_ = pc.Close()
				pc = nil
			}

		case sig.KindErr:
			var m sig.ErrMsg
			_ = json.Unmarshal(raw, &m)
			slog.Warn("server err", "code", m.Code, "msg", m.Message)
		}
	}
}

func newPeerConn(ctx context.Context, ws *websocket.Conn, session string, iceServers []sig.IceServer) (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{}
	for _, s := range iceServers {
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	pc, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		return nil, err
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		b, _ := json.Marshal(init)
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := wsjson.Write(wctx, ws, sig.ICEMsg{
			Kind: sig.KindICE, Session: session, Cand: b,
		}); err != nil {
			slog.Warn("ice send", "err", err)
		}
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		slog.Info("pc state", "state", s.String())
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		label := dc.Label()
		slog.Info("data channel arrived", "label", label, "ordered", dc.Ordered())
		dc.OnOpen(func() { slog.Info("dc open", "label", label) })
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			slog.Info("dc msg", "label", label, "len", len(msg.Data), "text", msg.IsString)
			if err := dc.Send(msg.Data); err != nil {
				slog.Warn("echo failed", "label", label, "err", err)
			}
		})
		dc.OnClose(func() { slog.Info("dc closed", "label", label) })
	})

	return pc, nil
}

func login(ctx context.Context, baseURL, user, pass string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("login %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("login: empty access_token")
	}
	return out.AccessToken, nil
}
