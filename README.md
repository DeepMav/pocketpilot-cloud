# pocketpilot-cloud

[![CI](https://github.com/DeepMav/pocketpilot-cloud/actions/workflows/ci.yml/badge.svg)](https://github.com/DeepMav/pocketpilot-cloud/actions/workflows/ci.yml)

Cloud services for PocketPilot WebRTC rendezvous over mobile networks.

## Components

| Binary | Purpose | Default port |
|---|---|---|
| `cmd/auth` | Authentication: issues JWTs, owns user/drone accounts | `:8081` |
| `cmd/signal` | Signaling: WebSocket hub, SDP/ICE routing, ICE server hand-out | `:8080` |
| `cmd/relay` | Drone-side companion: PX4 UDP ↔ WebRTC DataChannel via pion | — (egress only) |
| `cmd/testpeer` | Stub answerer for client development (no PX4 required) | — |

The TURN relay is a separate deployment — the user-owned coturn image
on Docker Hub.

## Prerequisites

- Go 1.23 or newer — https://go.dev/dl/
- `wscat` for smoke testing — `npm i -g wscat`

## Build

    go mod tidy
    go build ./...

Or run directly:

    go run ./cmd/auth -addr :8081
    go run ./cmd/signal -addr :8080

## Run locally (PoC)

`auth` mints tokens, `signal` verifies them — both must use the **same**
`JWT_SECRET` in dev (shared HMAC). Production will move to RS256 + JWKS so
`signal` only holds a public key.

    # Terminal 1 — auth service
    $env:JWT_SECRET = "$(openssl rand -hex 32)"      # PowerShell
    # export JWT_SECRET=$(openssl rand -hex 32)      # bash
    go run ./cmd/auth -addr :8081

    # Terminal 2 — signal service (same JWT_SECRET)
    go run ./cmd/signal -addr :8080 `
      -turn-uri 'turn:<your-turn-host>:3478?transport=udp' `
      -turn-user '<your-username>' `
      -turn-pass '<your-credential>'

    # Terminal 3 — drone-side bridge (PX4 SITL must send MAVLink to 127.0.0.1:14550)
    go run ./cmd/relay -mavlink-listen :14550

    # …or, with no PX4 on hand, use testpeer (echoes DataChannel bytes)
    go run ./cmd/testpeer

## Smoke test

`cmd/auth` seeds two dev accounts on start:
`pilot1 / pilot1-dev` (role=user) and `drone-42 / drone-42-dev` (role=drone).

    # 1. Get a token
    curl -s -H 'Content-Type: application/json' `
      -d '{\"username\":\"pilot1\",\"password\":\"pilot1-dev\"}' `
      http://localhost:8081/v1/auth/login

    # 2. WS as pilot1
    wscat -c ws://localhost:8080/v1/signal
    > {"kind":"hello","token":"<paste access_token>"}
    < {"kind":"hello.ok","self":"pilot1"}

    # 3. WS as drone-42 in another shell (use that drone's token)
    > {"kind":"hello","token":"..."}
    < {"kind":"hello.ok","self":"drone-42"}

    # 4. Pilot opens a session
    > {"kind":"session.req","peer":"drone-42"}
    < {"kind":"session.ack","session":"s_...","peer":"drone-42","initiator":true,"ice_servers":[...]}
    # drone-42 also receives session.ack with initiator:false

    # 5. SDP / ICE relay
    > {"kind":"sdp","session":"s_...","role":"offer","sdp":"..."}
    # drone-42 receives {"kind":"peer.sdp","session":"s_...","role":"offer","sdp":"..."}

## Wire protocol

JSON over WebSocket. Schema defined in `internal/signal/messages.go`. Client
must send `hello` (with JWT) as the first frame within 5 s or the connection
is dropped. Subsequent kinds: `session.req`, `sdp`, `ice`, `bye`, `ping`.

Server kinds: `hello.ok`, `session.ack`, `peer.sdp`, `peer.ice`, `peer.gone`,
`pong`, `err`.

## Layout

    cmd/
      auth/main.go            entrypoint — POST /v1/auth/login, GET /v1/me
      signal/main.go          entrypoint — GET /v1/signal (WebSocket)
    internal/
      token/                  JWT issue/verify + HTTP middleware (shared)
      auth/                   user store + login handlers (cmd/auth only)
      signal/                 hub, peer, ws upgrade, wire messages

## TODO (next iterations)

- TURN ephemeral credentials per RFC 7635 (replace static `-turn-user/-pass`)
- RS256 + JWKS endpoint on `cmd/auth`, drop shared HMAC
- Persistent user store (SQLite → Postgres)
- Refresh tokens
- TLS via `golang.org/x/crypto/acme/autocert` or fronted by Caddy
- Drone↔user ACL on `session.req`
- Multi-instance `signal` (Redis pub/sub for cross-node session routing)
- Tests

## License

Apache-2.0 — see `LICENSE`.
