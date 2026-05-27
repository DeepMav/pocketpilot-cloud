# Signaling protocol

Single source of truth for the JSON-over-WebSocket protocol spoken between
`cmd/signal` (server) and its clients (Android `PocketPilot`, drone-side
`cmd/relay`, the stub `cmd/testpeer`, or anything else).

The Go structs in `internal/signal/messages.go` are the *implementation*.
This document is the *contract*. If they disagree, this document is wrong —
file an issue. If you need to change the contract, see the last section.

## Endpoints

| Service | Path | Notes |
|---|---|---|
| Auth | `POST /v1/auth/login` | username/password → JWT (HS256, 1 h default TTL) |
| Auth | `GET  /v1/me` | requires `Authorization: Bearer <jwt>` |
| Signal | `GET  /v1/signal` | WebSocket upgrade; auth via first message |

Default ports: auth `:8081`, signal `:8080`. Production targets `https://` and
`wss://`.

## Auth: POST /v1/auth/login

Request:
```json
{ "username": "pilot1", "password": "pilot1-dev" }
```

Response 200:
```json
{
  "access_token": "<JWT>",
  "expires_at": 1748390400,
  "sub": "pilot1",
  "role": "user"
}
```

- `role` is `"user"` (human pilot) or `"drone"` (autonomous peer).
- `sub` (the JWT subject) doubles as the peer ID used in signaling.

Error responses share the shape `{"code":"...", "message":"..."}`:

| HTTP | `code` | Meaning |
|---|---|---|
| 400 | `bad_request` | Body parse failure |
| 401 | `invalid_credentials` | Username/password mismatch |
| 500 | `issue_failed` | Token signing failed |

## Signaling sequence

```
Phone (pilot1)                   Signal Server                  Drone (drone-42)
     |                                  |                              |
     |---- WS connect /v1/signal ------>|                              |
     |                                  |<--- WS connect /v1/signal ---|
     |                                  |                              |
     |---- {kind:"hello",token:JWT} --->|                              |
     |<--- {kind:"hello.ok",self} ------|                              |
     |                                  |--- {kind:"hello",token} -----|
     |                                  |--> {kind:"hello.ok",self} ---|
     |                                  |                              |
     |--- {kind:"session.req",          |                              |
     |     peer:"drone-42"} ----------->|                              |
     |                                  |                              |
     |<-- {kind:"session.ack",          |--- {kind:"session.ack",      |
     |     session,peer,initiator:true, |     session,peer,            |
     |     ice_servers} ----------------|     initiator:false,         |
     |                                  |     ice_servers} ----------->|
     |                                  |                              |
     |--- {kind:"sdp",role:"offer",     |                              |
     |     session,sdp} --------------->|                              |
     |                                  |--- {kind:"peer.sdp",         |
     |                                  |     role:"offer",            |
     |                                  |     session,sdp} ----------->|
     |                                  |                              |
     |                                  |<-- {kind:"sdp",              |
     |<-- {kind:"peer.sdp",             |     role:"answer",           |
     |     role:"answer",               |     session,sdp} ------------|
     |     session,sdp} ----------------|                              |
     |                                  |                              |
     |--- {kind:"ice",session,cand} --->|                              |
     |                                  |--- {kind:"peer.ice",         |
     |                                  |     session,cand} ---------->|
     |                                  |                              |
     |     (trickle ICE both ways)      |                              |
     |                                  |                              |
     |===== WebRTC PeerConnection ESTABLISHED (direct or TURN-relay) =====|
     |    DataChannels 'tlm', 'cmd', 'evt' open — MAVLink2 frames flow    |
     |====================================================================|
     |                                  |                              |
     |--- {kind:"bye",session} -------->|                              |
     |                                  |--- {kind:"peer.gone",        |
     |                                  |     session,reason} -------->|
```

## Wire vocabulary

Every message is a JSON object with a `kind` string. Unknown kinds get an
`err` reply but the connection stays open. The first frame on a fresh
WebSocket MUST be `hello` within **5 seconds** or the server closes with
`StatusPolicyViolation`.

### Client → server

| `kind` | Fields | Notes |
|---|---|---|
| `hello` | `token: string` | JWT from `/v1/auth/login`. Required first frame. |
| `session.req` | `peer: string` | Open session with target peer (must be currently connected). Initiator role. |
| `sdp` | `session, role: "offer" \| "answer", sdp: string` | Forwarded verbatim to the other peer. |
| `ice` | `session, cand: object` | Trickle ICE candidate. `cand` is the WebRTC `RTCIceCandidateInit` shape — see below. |
| `bye` | `session, reason?: string` | Tear down session; other peer gets `peer.gone`. |
| `ping` | — | Liveness probe; server replies `pong`. |

### Server → client

| `kind` | Fields | When |
|---|---|---|
| `hello.ok` | `self: string` | After successful `hello`. `self` echoes the JWT subject. |
| `session.ack` | `session, peer, initiator: bool, ice_servers: []IceServer` | Both parties get one each when a session opens. `peer` is the *other* party's ID; `initiator: true` for the side that called `session.req`. |
| `peer.sdp` | `session, role, sdp` | Counterparty's SDP relayed in. |
| `peer.ice` | `session, cand` | Counterparty's ICE candidate relayed in. |
| `peer.gone` | `session, reason` | Counterparty disconnected, sent `bye`, or the server tore down the session. |
| `pong` | — | Reply to `ping`. |
| `err` | `code, message` | Protocol error. Non-fatal — connection stays open unless the server explicitly closes it. |

### `IceServer` shape

Sent inside `session.ack`. Hand directly to WebRTC's `RTCConfiguration.iceServers`.

```json
{
  "urls": ["turn:43.203.28.242:3478?transport=udp"],
  "username": "<your-coturn-username>",
  "credential": "<your-coturn-credential>"
}
```

PoC uses the static credentials from `cmd/signal` flags. Production will
hand out RFC 7635 ephemeral creds in this **same shape** — clients don't
need to change anything.

### `cand` shape (ICE candidate)

Exactly the WebRTC `RTCIceCandidateInit` dictionary:

```json
{
  "candidate": "candidate:842163049 1 udp 1677729535 ...",
  "sdpMid": "0",
  "sdpMLineIndex": 0,
  "usernameFragment": "abcd"
}
```

End-of-candidates is signalled by *not* sending more `ice` messages; there
is no explicit terminator.

## DataChannel conventions (post-handshake)

Once the WebRTC PeerConnection is established, signaling is no longer used
for the application layer. The **initiator** (typically the phone) creates
the DataChannels with these labels and parameters:

| Label | `ordered` | `maxRetransmits` | Carries |
|---|---|---|---|
| `tlm` | `false` | `0` | MAVLink2 telemetry frames. Loss tolerated; latency wins. |
| `cmd` | `true` | (default) | MAVLink2 command frames (ARM, TAKEOFF, DO_REPOSITION, LAND, ...). |
| `evt` | `true` | (default) | App-level events (session metadata, user-visible notices). |

MAVLink2 frames are self-delimiting (STX + length + CRC), so DataChannel
message boundaries map 1:1 to MAVLink frame boundaries — no extra framing
is needed.

The answerer (drone / testpeer) does **not** create DataChannels; it
receives them via the `ondatachannel` callback after `setRemoteDescription`.

## Error codes

| Code | Where | Meaning |
|---|---|---|
| `missing_token` | HTTP | No `Authorization: Bearer` header. |
| `invalid_token` | HTTP, WS | JWT failed verification (expired, wrong signature, malformed). |
| `invalid_credentials` | HTTP `/login` | Username/password mismatch. |
| `bad_request` | HTTP | Body parse failure. |
| `bad_json` | WS | Message couldn't be parsed as JSON envelope. |
| `dispatch` | WS | Server failed to handle a known kind. Typical causes: target peer not online, session not found, peer not a member of the named session. |
| `duplicate_peer` | WS | Same peer ID already has a WebSocket connection. (TODO: switch to kick-old policy.) |
| `issue_failed` | HTTP | Token signing failed (server-side). |

## Implementation pointers

- Server: `internal/signal/messages.go` (wire types), `internal/signal/peer.go` (dispatcher), `internal/signal/hub.go` (registry/routing), `cmd/signal/main.go` (entry).
- Drone-side bridge (answerer, production): `cmd/relay/main.go` + `internal/mavbridge/` — bridges DataChannels to a local PX4/ArduPilot UDP socket. Reconnects to signal with exponential backoff; handles sequential phone sessions.
- Stub answerer (for client development, no PX4 needed): `cmd/testpeer/main.go` — echoes any DataChannel bytes.
- Android client (initiator): `PocketPilot/app/.../signaling/` (Kotlin DTOs to be written by the Android session) — implement against the shapes in this document.

## Changing this contract

Both server and clients depend on these names and shapes. To add or modify:

1. Update `internal/signal/messages.go` first.
2. Update this document in the same commit.
3. Coordinate with the Android session (or other client owners) before the
   change lands on `main` — see the `pocketpilot-cloud-reference` memory in
   `~/.claude/projects/.../memory/`.

Backward-incompatible changes (renaming fields, removing kinds, changing the
shape of `cand`) require bumping a `version` field that does not exist yet —
add it before the first breaking change.
