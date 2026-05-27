// SPDX-License-Identifier: Apache-2.0

// testpeer is a stub client that talks the SIGNALING.md protocol end to
// end. It runs in one of two roles:
//
//	-role drone   Answerer. Logs in as drone-42, accepts the initiator's
//	              WebRTC offer, echoes any DataChannel bytes. Useful when
//	              developing the phone-side client without PX4 + cmd/relay.
//
//	-role pilot   Initiator. Logs in as pilot1, sends session.req with
//	              -target-peer (default drone-42), creates the three
//	              DataChannels per SIGNALING.md, generates an offer, and
//	              parses incoming MAVLink frames on tlm. Useful for T2-
//	              style full-chain tests when the real Android client
//	              isn't ready yet.
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

	"github.com/deepmav/pocketpilot-cloud/internal/mavframe"
	sig "github.com/deepmav/pocketpilot-cloud/internal/signal"
)

type peerRole string

const (
	roleDrone peerRole = "drone"
	rolePilot peerRole = "pilot"
)

func main() {
	var (
		authURL    = flag.String("auth-url", "http://localhost:8081", "auth service base URL")
		signalURL  = flag.String("signal-url", "ws://localhost:8080/v1/signal", "signaling WebSocket URL")
		roleStr    = flag.String("role", "drone", "drone (answerer/echo) | pilot (initiator/MAVLink sink)")
		user       = flag.String("user", "", "username (default: drone-42 or pilot1 by role)")
		pass       = flag.String("pass", "", "password (default: matching dev creds)")
		targetPeer = flag.String("target-peer", "drone-42", "(pilot only) peer to open session with")
	)
	flag.Parse()

	role := peerRole(*roleStr)
	switch role {
	case roleDrone, rolePilot:
	default:
		slog.Error("invalid -role", "value", *roleStr)
		os.Exit(2)
	}

	if *user == "" {
		if role == rolePilot {
			*user = "pilot1"
		} else {
			*user = "drone-42"
		}
	}
	if *pass == "" {
		if role == rolePilot {
			*pass = "pilot1-dev"
		} else {
			*pass = "drone-42-dev"
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tok, err := login(ctx, *authURL, *user, *pass)
	if err != nil {
		slog.Error("login failed", "err", err)
		os.Exit(1)
	}
	slog.Info("logged in", "user", *user, "role", role)

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

	if err := runLoop(ctx, conn, role, *targetPeer); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("loop ended", "err", err)
	}
}

func runLoop(ctx context.Context, ws *websocket.Conn, role peerRole, targetPeer string) error {
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
			if role == rolePilot {
				if err := wsjson.Write(ctx, ws, sig.SessionReqMsg{
					Kind: sig.KindSessionReq, Peer: targetPeer,
				}); err != nil {
					return fmt.Errorf("session.req: %w", err)
				}
				slog.Info("session.req sent", "peer", targetPeer)
			}

		case sig.KindSessionAck:
			var m sig.SessionAckMsg
			_ = json.Unmarshal(raw, &m)
			slog.Info("session.ack",
				"session", m.Session, "peer", m.Peer,
				"initiator", m.Initiator, "ice_servers", len(m.IceServers))
			if (role == rolePilot) != m.Initiator {
				slog.Warn("role/initiator mismatch", "role", role, "initiator", m.Initiator)
				continue
			}
			sessionID = m.Session
			newPC, err := newPeerConn(ctx, ws, sessionID, m.IceServers, role)
			if err != nil {
				slog.Error("pc setup", "err", err)
				continue
			}
			pc = newPC
			if role == rolePilot {
				if err := setupPilotChannelsAndOffer(ctx, ws, pc, sessionID); err != nil {
					slog.Error("pilot setup/offer", "err", err)
					continue
				}
			}

		case sig.KindPeerSDP:
			var m sig.PeerSDPMsg
			_ = json.Unmarshal(raw, &m)
			if pc == nil {
				slog.Warn("peer.sdp before session.ack, ignoring")
				continue
			}
			switch role {
			case roleDrone:
				if m.Role != "offer" {
					slog.Warn("drone expected offer", "role", m.Role)
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
			case rolePilot:
				if m.Role != "answer" {
					slog.Warn("pilot expected answer", "role", m.Role)
					continue
				}
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{
					Type: webrtc.SDPTypeAnswer, SDP: m.SDP,
				}); err != nil {
					slog.Error("set remote", "err", err)
					continue
				}
				slog.Info("answer applied")
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

func newPeerConn(ctx context.Context, ws *websocket.Conn, session string, iceServers []sig.IceServer, role peerRole) (*webrtc.PeerConnection, error) {
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

	if role == roleDrone {
		// Answerer: DataChannels arrive from the initiator. Echo bytes back.
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
	}

	return pc, nil
}

// setupPilotChannelsAndOffer creates the three SIGNALING.md DataChannels
// from the initiator side, generates an SDP offer, and sends it.
func setupPilotChannelsAndOffer(ctx context.Context, ws *websocket.Conn, pc *webrtc.PeerConnection, sessionID string) error {
	falseV := false
	zeroR := uint16(0)
	tlm, err := pc.CreateDataChannel("tlm", &webrtc.DataChannelInit{
		Ordered:        &falseV,
		MaxRetransmits: &zeroR,
	})
	if err != nil {
		return fmt.Errorf("tlm: %w", err)
	}
	cmd, err := pc.CreateDataChannel("cmd", nil)
	if err != nil {
		return fmt.Errorf("cmd: %w", err)
	}
	evt, err := pc.CreateDataChannel("evt", nil)
	if err != nil {
		return fmt.Errorf("evt: %w", err)
	}

	tlmLog := mavframe.NewDebugLogger("dc")
	tlm.OnOpen(func() { slog.Info("tlm open (pilot)", "ordered", tlm.Ordered()) })
	tlm.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			slog.Warn("tlm got text frame, ignoring")
			return
		}
		tlmLog(msg.Data)
	})
	tlm.OnClose(func() { slog.Info("tlm closed (pilot)") })

	cmd.OnOpen(func() { slog.Info("cmd open (pilot)") })
	cmd.OnMessage(func(msg webrtc.DataChannelMessage) {
		slog.Info("cmd recv (unexpected; pilot is sender)", "len", len(msg.Data))
	})
	cmd.OnClose(func() { slog.Info("cmd closed (pilot)") })

	evt.OnOpen(func() { slog.Info("evt open (pilot)") })
	evt.OnClose(func() { slog.Info("evt closed (pilot)") })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local: %w", err)
	}
	if err := wsjson.Write(ctx, ws, sig.SDPMsg{
		Kind: sig.KindSDP, Session: sessionID, Role: "offer", SDP: offer.SDP,
	}); err != nil {
		return fmt.Errorf("send offer: %w", err)
	}
	slog.Info("offer sent")
	return nil
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
