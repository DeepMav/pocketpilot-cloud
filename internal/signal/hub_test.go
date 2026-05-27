// SPDX-License-Identifier: Apache-2.0
package signal

import (
	"encoding/json"
	"strings"
	"testing"
)

// NewHub must normalize a nil ice slice to an empty one so the
// session.ack frame the server marshals always carries "ice_servers": []
// — never "ice_servers": null. Tracking issue: clients that decoded the
// field into a non-nullable list (kotlinx-serialization without
// coerceInputValues) were silently tearing down the WebSocket on
// session.ack.
func TestNewHubNormalizesNilIceServers(t *testing.T) {
	h := NewHub(nil)
	if h.IceServers() == nil {
		t.Fatalf("IceServers() returned nil; expected empty slice")
	}
	if got := len(h.IceServers()); got != 0 {
		t.Fatalf("IceServers() length = %d; want 0", got)
	}

	// Round-trip through encoding/json so we catch regression at the wire,
	// not just in the slice header.
	ack := SessionAckMsg{
		Kind:       KindSessionAck,
		Session:    "s_test",
		Peer:       "drone-1",
		IceServers: h.IceServers(),
		Initiator:  true,
	}
	b, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"ice_servers":[]`) {
		t.Fatalf("ice_servers should serialize as [] (not null), got: %s", got)
	}
	if strings.Contains(got, `"ice_servers":null`) {
		t.Fatalf("ice_servers serialized as null: %s", got)
	}
}

func TestNewHubPreservesProvidedIceServers(t *testing.T) {
	servers := []IceServer{
		{URLs: []string{"turn:host:3478?transport=udp"}, Username: "u", Credential: "c"},
		{URLs: []string{"stun:stun.example.org:3478"}},
	}
	h := NewHub(servers)
	if got := len(h.IceServers()); got != 2 {
		t.Fatalf("len = %d; want 2", got)
	}
	if h.IceServers()[0].Username != "u" {
		t.Fatalf("Username = %q; want %q", h.IceServers()[0].Username, "u")
	}
}
