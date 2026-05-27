// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"sync/atomic"
)

// makeDebugLogger returns an onPacket callback that prints parsed MAVLink
// frame headers. The first 10 packets are always logged; thereafter every
// 100th, to keep the noise floor manageable when telemetry runs at
// 10–50 Hz.
func makeDebugLogger() func([]byte) {
	var count atomic.Uint64
	return func(pkt []byte) {
		n := count.Add(1)
		msgid, sysid, compid, seq, ok := parseMavHeader(pkt)
		shouldLog := n <= 10 || n%100 == 0
		if !shouldLog {
			return
		}
		if !ok {
			slog.Info("mav recv (unparsed)",
				"n", n, "len", len(pkt),
				"first", fmt.Sprintf("%02x", pkt[0]))
			return
		}
		slog.Info("mav recv",
			"n", n, "len", len(pkt),
			"msg", mavMsgName(msgid),
			"msgid", msgid, "sys", sysid, "comp", compid, "seq", seq)
	}
}

// parseMavHeader extracts MAVLink frame fields from the wire bytes.
// Returns ok=false for non-MAVLink datagrams or frames too short to be
// valid.
func parseMavHeader(pkt []byte) (msgid uint32, sysid, compid, seq byte, ok bool) {
	if len(pkt) == 0 {
		return 0, 0, 0, 0, false
	}
	switch pkt[0] {
	case 0xFD: // MAVLink v2
		if len(pkt) < 10 {
			return 0, 0, 0, 0, false
		}
		// bytes: 1=plen, 2=incompat, 3=compat, 4=seq, 5=sysid, 6=compid, 7-9=msgid (24-bit LE)
		return uint32(pkt[7]) | uint32(pkt[8])<<8 | uint32(pkt[9])<<16,
			pkt[5], pkt[6], pkt[4], true
	case 0xFE: // MAVLink v1
		if len(pkt) < 6 {
			return 0, 0, 0, 0, false
		}
		// bytes: 1=plen, 2=seq, 3=sysid, 4=compid, 5=msgid
		return uint32(pkt[5]), pkt[3], pkt[4], pkt[2], true
	}
	return 0, 0, 0, 0, false
}

// mavMsgNames maps common MAVLink message IDs to canonical names. Not
// exhaustive — anything unknown shows as MSGID_<n>.
var mavMsgNames = map[uint32]string{
	0:   "HEARTBEAT",
	1:   "SYS_STATUS",
	2:   "SYSTEM_TIME",
	24:  "GPS_RAW_INT",
	30:  "ATTITUDE",
	32:  "LOCAL_POSITION_NED",
	33:  "GLOBAL_POSITION_INT",
	74:  "VFR_HUD",
	76:  "COMMAND_LONG",
	77:  "COMMAND_ACK",
	83:  "ATTITUDE_TARGET",
	111: "TIMESYNC",
	241: "VIBRATION",
	253: "STATUSTEXT",
}

func mavMsgName(id uint32) string {
	if n, ok := mavMsgNames[id]; ok {
		return n
	}
	return fmt.Sprintf("MSGID_%d", id)
}
