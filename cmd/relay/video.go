// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// videoSource encapsulates one camera → ffmpeg → RTP → pion track pipeline.
//
// Lifecycle:
//
//	startVideoSource()  — bind a loopback UDP port, spawn ffmpeg, return the
//	                      pion track ready to be added to a PeerConnection.
//	(read loop, internal) — drain RTP from UDP and forward to track.WriteRTP.
//	Close()              — kill ffmpeg, close UDP socket, cancel goroutines.
//
// The track is a TrackLocalStaticRTP. We feed it pre-packetized RTP from
// ffmpeg directly; pion does not re-packetize. That keeps the data path
// simple and means the keyframe / timestamp / SSRC details are whatever
// ffmpeg emits.
type videoSource struct {
	track  *webrtc.TrackLocalStaticRTP
	conn   *net.UDPConn
	ffmpeg *exec.Cmd
	cancel context.CancelFunc
}

// startVideoSource starts ffmpeg against [cameraDev] and returns a track
// ready to add to a PeerConnection. The ffmpeg subprocess is killed when
// Close is called or when the parent context is cancelled.
//
// resolution: e.g. "1280x720"; framerate is fixed at 30 fps (matches the
// USB camera's native MJPG mode).
func startVideoSource(parent context.Context, cameraDev, resolution string) (*videoSource, error) {
	// Match one of the H.264 profiles RegisterDefaultCodecs adds (PT 102),
	// otherwise codecParametersFuzzySearch fails to bind the sender to a
	// MediaEngine codec entry and CreateAnswer bails with
	// "RTPSender created with no codecs".
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		"video", "pi-camera",
	)
	if err != nil {
		return nil, fmt.Errorf("new track: %w", err)
	}

	const loopbackPort = 5004
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: loopbackPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp 127.0.0.1:%d: %w", loopbackPort, err)
	}

	ctx, cancel := context.WithCancel(parent)

	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-f", "v4l2",
		"-input_format", "mjpeg",
		"-framerate", "30",
		"-video_size", resolution,
		"-i", cameraDev,
		// Encode to H.264 baseline. Constrained Baseline / Level 3.1 is
		// what WebRTC clients almost always negotiate; baseline+yuv420p
		// keeps libwebrtc-android happy.
		"-c:v", "libx264",
		"-tune", "zerolatency",
		"-preset", "ultrafast",
		"-profile:v", "baseline",
		"-pix_fmt", "yuv420p",
		// Reasonable bitrate ceiling for 720p; ffmpeg's libx264 picks a
		// rate-control mode based on tune/preset.
		"-b:v", "2000k",
		"-maxrate", "2500k",
		"-bufsize", "2500k",
		"-g", "60", // keyframe every 2s at 30 fps
		"-an",
		"-f", "rtp",
		"-payload_type", "96",
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", loopbackPort),
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	// Stream ffmpeg's stderr into our slog as warnings — it's chatty but
	// invaluable when the camera doesn't open or the encoder bails.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		conn.Close()
		cancel()
		return nil, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		conn.Close()
		cancel()
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}
	slog.Info("ffmpeg started",
		"pid", cmd.Process.Pid,
		"device", cameraDev,
		"resolution", resolution)

	go logFfmpegStderr(stderr)
	go readRTP(ctx, conn, track)

	vs := &videoSource{
		track:  track,
		conn:   conn,
		ffmpeg: cmd,
		cancel: cancel,
	}
	return vs, nil
}

// Close kills ffmpeg, closes the UDP socket, and cancels the read loop.
// Idempotent.
func (vs *videoSource) Close() {
	if vs == nil {
		return
	}
	if vs.cancel != nil {
		vs.cancel()
	}
	if vs.ffmpeg != nil && vs.ffmpeg.Process != nil {
		_ = vs.ffmpeg.Process.Kill()
		_ = vs.ffmpeg.Wait()
	}
	if vs.conn != nil {
		_ = vs.conn.Close()
	}
}

func readRTP(ctx context.Context, conn *net.UDPConn, track *webrtc.TrackLocalStaticRTP) {
	buf := make([]byte, 1600)
	for {
		if ctx.Err() != nil {
			return
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("rtp read ended", "err", err)
			return
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			slog.Debug("rtp unmarshal", "err", err)
			continue
		}
		if err := track.WriteRTP(pkt); err != nil {
			// ErrClosedPipe / similar when the track is no longer being
			// consumed (peer disconnected). Stop quietly.
			slog.Debug("track write ended", "err", err)
			return
		}
	}
}

func logFfmpegStderr(r interface{ Read(p []byte) (int, error) }) {
	buf := make([]byte, 4096)
	var carry string
	for {
		n, err := r.Read(buf)
		if n > 0 {
			lines := strings.Split(carry+string(buf[:n]), "\n")
			carry = lines[len(lines)-1]
			for _, line := range lines[:len(lines)-1] {
				if strings.TrimSpace(line) == "" {
					continue
				}
				slog.Info("ffmpeg", "msg", line)
			}
		}
		if err != nil {
			if carry != "" {
				slog.Info("ffmpeg", "msg", carry)
			}
			return
		}
	}
}
