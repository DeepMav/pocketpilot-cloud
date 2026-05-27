// SPDX-License-Identifier: Apache-2.0

// relay is the drone-side companion. It bridges local MAVLink (UDP, from
// PX4 / ArduPilot / SITL) to a phone client via WebRTC over the
// pocketpilot-cloud signaling service.
//
// The relay is always the WebRTC answerer. The phone is the initiator and
// creates the DataChannels per SIGNALING.md:
//
//   - "tlm" (unordered, lossy):  relay -> phone   telemetry
//   - "cmd" (ordered, reliable): phone -> relay   commands
//   - "evt" (ordered):            either way, app events
//
// The signaling WebSocket is reconnected with exponential backoff when it
// drops. While the WS is healthy, sequential WebRTC sessions are handled
// (one phone at a time for now).
//
// Test modes:
//
//	-debug-mavlink   Log parsed MAVLink frame headers as they arrive.
//	                 Useful regardless of whether a phone is connected.
//	-skip-signaling  Run only the mavbridge (no auth, no signaling, no
//	                 WebRTC). For T1-style "is PX4 reaching us?" checks.
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

	"github.com/deepmav/pocketpilot-cloud/internal/mavbridge"
	"github.com/deepmav/pocketpilot-cloud/internal/mavframe"
	sig "github.com/deepmav/pocketpilot-cloud/internal/signal"
)

type config struct {
	authURL       string
	signalURL     string
	user, pass    string
	mavlinkListen string
	minDelay      time.Duration
	maxDelay      time.Duration
	debugMavlink  bool
	skipSignaling bool
	cameraDev     string
	cameraSize    string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.authURL, "auth-url", "http://localhost:8081", "auth service base URL")
	flag.StringVar(&cfg.signalURL, "signal-url", "ws://localhost:8080/v1/signal", "signaling WebSocket URL")
	flag.StringVar(&cfg.user, "user", "drone-42", "drone account username")
	flag.StringVar(&cfg.pass, "pass", "drone-42-dev", "drone account password (TODO: replace with device cert)")
	flag.StringVar(&cfg.mavlinkListen, "mavlink-listen", ":14550", "local UDP address for MAVLink (autopilot sends here)")
	flag.DurationVar(&cfg.minDelay, "reconnect-min", time.Second, "min backoff before reconnecting to signal")
	flag.DurationVar(&cfg.maxDelay, "reconnect-max", 30*time.Second, "max backoff before reconnecting to signal")
	flag.BoolVar(&cfg.debugMavlink, "debug-mavlink", false, "log parsed MAVLink frame headers (HEARTBEAT, etc.) as they arrive")
	flag.BoolVar(&cfg.skipSignaling, "skip-signaling", false, "skip signaling client and run mavbridge alone (T1 testing)")
	flag.StringVar(&cfg.cameraDev, "camera", "", "v4l2 device for the H.264 video track (e.g. /dev/video0). empty = no video")
	flag.StringVar(&cfg.cameraSize, "camera-size", "1280x720", "video resolution passed to ffmpeg -video_size")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	bridge, err := mavbridge.Listen(cfg.mavlinkListen)
	if err != nil {
		slog.Error("mavbridge listen", "err", err)
		os.Exit(1)
	}
	defer bridge.Close()
	go func() {
		if err := bridge.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("mavbridge run ended", "err", err)
		}
	}()

	if cfg.debugMavlink {
		installTelemetryHandler(bridge, nil, true)
		slog.Info("debug-mavlink enabled; logging frame headers as they arrive")
	}

	if cfg.skipSignaling {
		slog.Info("skip-signaling mode; mavbridge alone (Ctrl-C to exit)")
		<-ctx.Done()
		slog.Info("relay exited")
		return
	}

	runWithBackoff(ctx, cfg.minDelay, cfg.maxDelay, func(ctx context.Context) error {
		return sessionCycle(ctx, cfg, bridge)
	})
	slog.Info("relay exited")
}

// runWithBackoff calls do repeatedly with exponential backoff between
// failed attempts. Returns when ctx is cancelled.
func runWithBackoff(ctx context.Context, minDelay, maxDelay time.Duration, do func(context.Context) error) {
	delay := minDelay
	for {
		err := do(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			delay = minDelay
			continue
		}
		slog.Warn("attempt failed", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

// sessionCycle: login → connect WS → hello → loop. Returns whenever the
// WS dies or auth fails; the outer backoff loop will retry.
func sessionCycle(ctx context.Context, cfg config, bridge *mavbridge.UDP) error {
	tok, err := login(ctx, cfg.authURL, cfg.user, cfg.pass)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	slog.Info("logged in", "user", cfg.user)

	conn, _, err := websocket.Dial(ctx, cfg.signalURL, nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(128 * 1024)

	if err := wsjson.Write(ctx, conn, sig.HelloMsg{Kind: sig.KindHello, Token: tok}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}

	return runWSLoop(ctx, conn, bridge, cfg)
}

// runWSLoop drives the signaling WS until it dies. Multiple sequential
// WebRTC sessions may come and go on this same WS.
func runWSLoop(ctx context.Context, ws *websocket.Conn, bridge *mavbridge.UDP, cfg config) error {
	var (
		pc        *webrtc.PeerConnection
		sessionID string
		camera    *videoSource
	)
	debug := cfg.debugMavlink
	defer func() {
		camera.Close()
		if pc != nil {
			_ = pc.Close()
		}
		installTelemetryHandler(bridge, nil, debug)
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
			if m.Initiator {
				slog.Warn("relay got initiator=true; relay is answerer only", "session", m.Session)
				continue
			}
			if pc != nil {
				_ = pc.Close()
				pc = nil
				installTelemetryHandler(bridge, nil, debug)
			}
			camera.Close()
			camera = nil
			sessionID = m.Session
			slog.Info("session.ack",
				"session", m.Session, "peer", m.Peer, "ice_servers", len(m.IceServers))
			newPC, err := newPeerConn(ctx, ws, sessionID, m.IceServers, bridge, debug)
			if err != nil {
				slog.Error("pc setup", "err", err)
				continue
			}
			pc = newPC

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
			// If the operator passed -camera, start ffmpeg + RTP track and
			// add it to the PeerConnection before generating the answer so
			// the answer SDP advertises the video media. If the remote
			// offer didn't include a video transceiver, AddTrack still
			// works (pion creates a new transceiver), but the client won't
			// receive video unless its offer asked for it.
			if cfg.cameraDev != "" && camera == nil {
				vs, vsErr := startVideoSource(ctx, cfg.cameraDev, cfg.cameraSize)
				if vsErr != nil {
					slog.Error("camera start failed (continuing without video)", "err", vsErr)
				} else {
					camera = vs
					if _, addErr := pc.AddTrack(vs.track); addErr != nil {
						slog.Error("AddTrack failed", "err", addErr)
						camera.Close()
						camera = nil
					} else {
						slog.Info("video track added", "codec", "H264", "resolution", cfg.cameraSize)
					}
				}
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
			camera.Close()
			camera = nil
			installTelemetryHandler(bridge, nil, debug)

		case sig.KindErr:
			var m sig.ErrMsg
			_ = json.Unmarshal(raw, &m)
			slog.Warn("server err", "code", m.Code, "msg", m.Message)
		}
	}
}

func newPeerConn(ctx context.Context, ws *websocket.Conn, session string, iceServers []sig.IceServer, bridge *mavbridge.UDP, debug bool) (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{}
	for _, s := range iceServers {
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	// MediaEngine carries the codec table that gets advertised in SDP.
	// pion's NewPeerConnection() does NOT register any codecs by default —
	// CreateAnswer then bails with "RTPSender created with no codecs"
	// when we try to add an H.264 video track. RegisterDefaultCodecs adds
	// VP8/VP9/H.264/Opus, which matches what libwebrtc-android negotiates.
	mediaEngine := &webrtc.MediaEngine{}
	if rcErr := mediaEngine.RegisterDefaultCodecs(); rcErr != nil {
		return nil, fmt.Errorf("register default codecs: %w", rcErr)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(cfg)
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
		switch s {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			installTelemetryHandler(bridge, nil, debug)
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		label := dc.Label()
		slog.Info("data channel arrived", "label", label, "ordered", dc.Ordered())
		switch label {
		case "tlm":
			wireTelemetry(dc, bridge, debug)
		case "cmd":
			wireCommand(dc, bridge)
		case "evt":
			wireEvent(dc)
		default:
			slog.Warn("unknown data channel label", "label", label)
		}
	})

	return pc, nil
}

// wireTelemetry: autopilot UDP -> DataChannel.
func wireTelemetry(dc *webrtc.DataChannel, bridge *mavbridge.UDP, debug bool) {
	dc.OnOpen(func() {
		slog.Info("tlm open; forwarding UDP -> DC")
		installTelemetryHandler(bridge, dc, debug)
	})
	dc.OnClose(func() {
		slog.Info("tlm closed")
		installTelemetryHandler(bridge, nil, debug)
	})
}

// installTelemetryHandler attaches an onPacket on the mavbridge that
// (optionally) logs each frame and (optionally) forwards to a DataChannel.
// Both off → bridge callback cleared.
func installTelemetryHandler(bridge *mavbridge.UDP, dc *webrtc.DataChannel, debug bool) {
	var dbg func([]byte)
	if debug {
		dbg = mavframe.NewDebugLogger("udp")
	}
	if dc == nil && dbg == nil {
		bridge.SetOnPacket(nil)
		return
	}
	bridge.SetOnPacket(func(pkt []byte) {
		if dbg != nil {
			dbg(pkt)
		}
		if dc != nil {
			if err := dc.Send(pkt); err != nil {
				slog.Warn("tlm dc send", "err", err)
			}
		}
	})
}

// wireCommand: DataChannel -> autopilot UDP.
func wireCommand(dc *webrtc.DataChannel, bridge *mavbridge.UDP) {
	dc.OnOpen(func() { slog.Info("cmd open") })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString {
			slog.Warn("cmd: unexpected text frame, dropping")
			return
		}
		if _, err := bridge.Write(msg.Data); err != nil {
			slog.Warn("cmd -> mavbridge", "err", err)
		}
	})
	dc.OnClose(func() { slog.Info("cmd closed") })
}

func wireEvent(dc *webrtc.DataChannel) {
	dc.OnOpen(func() { slog.Info("evt open") })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		slog.Info("evt", "data", string(msg.Data))
	})
	dc.OnClose(func() { slog.Info("evt closed") })
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
