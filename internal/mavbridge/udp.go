// SPDX-License-Identifier: Apache-2.0

// Package mavbridge bridges MAVLink frames between a local UDP socket
// (the autopilot or SITL) and an external transport via callback.
//
// Frames are not parsed — MAVLink2 frames are self-delimiting and one
// UDP datagram contains exactly one frame, so packets are passed through
// as opaque bytes.
package mavbridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// UDP is a bidirectional MAVLink UDP bridge. It listens on a local port,
// remembers the first source address it sees as the active endpoint, and
// exposes Write() for sending packets back to that endpoint.
type UDP struct {
	conn     *net.UDPConn
	endpoint atomic.Pointer[net.UDPAddr]

	mu    sync.RWMutex
	onPkt func([]byte)

	closeOnce sync.Once
}

func Listen(addr string) (*UDP, error) {
	laddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", addr, err)
	}
	slog.Info("mavbridge listening", "addr", conn.LocalAddr().String())
	return &UDP{conn: conn}, nil
}

// SetOnPacket installs the upstream sink. Pass nil to detach (incoming
// packets are then dropped). Safe to call concurrently with Run.
func (u *UDP) SetOnPacket(fn func([]byte)) {
	u.mu.Lock()
	u.onPkt = fn
	u.mu.Unlock()
}

// Run loops until ctx is cancelled or the socket dies. Each received
// datagram is delivered to the current onPacket callback (if any).
func (u *UDP) Run(ctx context.Context) error {
	// Force ReadFromUDP to unblock when ctx is cancelled.
	stop := context.AfterFunc(ctx, func() {
		_ = u.conn.SetReadDeadline(time.Unix(1, 0))
	})
	defer stop()

	buf := make([]byte, 64*1024)
	for {
		n, src, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		cur := u.endpoint.Load()
		if cur == nil || !udpAddrEqual(cur, src) {
			// First packet, or PX4 restarted on a new ephemeral port.
			u.endpoint.Store(src)
			slog.Info("mavbridge endpoint learned", "addr", src.String())
		}

		// Copy: buf is reused on the next iteration.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		u.mu.RLock()
		cb := u.onPkt
		u.mu.RUnlock()
		if cb != nil {
			cb(pkt)
		}
	}
}

// ErrNoEndpoint is returned by Write when the bridge has not yet received
// any packet and therefore doesn't know where to send.
var ErrNoEndpoint = errors.New("mavbridge: no endpoint learned yet")

// Write sends a packet to the currently learned endpoint (the autopilot).
func (u *UDP) Write(p []byte) (int, error) {
	addr := u.endpoint.Load()
	if addr == nil {
		return 0, ErrNoEndpoint
	}
	return u.conn.WriteToUDP(p, addr)
}

// Endpoint returns the currently learned autopilot address, or nil.
func (u *UDP) Endpoint() *net.UDPAddr {
	return u.endpoint.Load()
}

func (u *UDP) Close() error {
	var err error
	u.closeOnce.Do(func() {
		err = u.conn.Close()
	})
	return err
}

func udpAddrEqual(a, b *net.UDPAddr) bool {
	return a.IP.Equal(b.IP) && a.Port == b.Port && a.Zone == b.Zone
}
