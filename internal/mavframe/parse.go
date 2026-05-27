// SPDX-License-Identifier: Apache-2.0

// Package mavframe parses MAVLink v1/v2 frame headers and maps common
// message IDs to canonical names. Used by relay and testpeer for debug
// logging — it does not validate CRCs or decode payloads.
package mavframe

import (
	"fmt"
	"log/slog"
	"sync/atomic"
)

// ParseHeader extracts the message ID, system ID, component ID, and
// sequence from a MAVLink v1 or v2 frame. Returns ok=false for non-MAVLink
// datagrams or frames too short to be valid.
func ParseHeader(pkt []byte) (msgid uint32, sysid, compid, seq byte, ok bool) {
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

// MsgName returns the canonical name for the given MAVLink message ID,
// or "MSGID_<n>" for unknown IDs. The map is not exhaustive — additions
// are welcome whenever an unknown name shows up in logs.
func MsgName(id uint32) string {
	if n, ok := msgNames[id]; ok {
		return n
	}
	return fmt.Sprintf("MSGID_%d", id)
}

var msgNames = map[uint32]string{
	0:   "HEARTBEAT",
	1:   "SYS_STATUS",
	2:   "SYSTEM_TIME",
	22:  "PARAM_VALUE",
	24:  "GPS_RAW_INT",
	30:  "ATTITUDE",
	31:  "ATTITUDE_QUATERNION",
	32:  "LOCAL_POSITION_NED",
	33:  "GLOBAL_POSITION_INT",
	36:  "SERVO_OUTPUT_RAW",
	42:  "MISSION_REQUEST_INT",
	74:  "VFR_HUD",
	76:  "COMMAND_LONG",
	77:  "COMMAND_ACK",
	83:  "ATTITUDE_TARGET",
	85:  "POSITION_TARGET_LOCAL_NED",
	87:  "POSITION_TARGET_GLOBAL_INT",
	111: "TIMESYNC",
	141: "ALTITUDE",
	230: "ESTIMATOR_STATUS",
	241: "VIBRATION",
	242: "HOME_POSITION",
	245: "EXTENDED_SYS_STATE",
	253: "STATUSTEXT",
}

// NewDebugLogger returns a packet logger that prints the first 10 frames
// and every 100th thereafter, keeping the noise floor manageable at
// 10–50 Hz telemetry rates. The label appears in each log line so
// multiple loggers (e.g., "udp" for relay's UDP side, "dc" for testpeer's
// DataChannel side) can be distinguished.
func NewDebugLogger(label string) func([]byte) {
	var count atomic.Uint64
	return func(pkt []byte) {
		n := count.Add(1)
		if n > 10 && n%100 != 0 {
			return
		}
		msgid, sysid, compid, seq, ok := ParseHeader(pkt)
		if !ok {
			slog.Info("mav recv (unparsed)",
				"label", label, "n", n, "len", len(pkt),
				"first", fmt.Sprintf("%02x", pkt[0]))
			return
		}
		slog.Info("mav recv",
			"label", label, "n", n, "len", len(pkt),
			"msg", MsgName(msgid),
			"msgid", msgid, "sys", sysid, "comp", compid, "seq", seq)
	}
}
