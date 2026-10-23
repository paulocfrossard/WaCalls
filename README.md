<div align="center">

# 📞 WaCalls (Go)

**Native WhatsApp voice calls in pure Go, straight from the browser.**
Built for native VoIP media, multi-account (multi-session) operation, and a modern browser client.

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![React](https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=black)](https://react.dev)
[![whatsmeow](https://img.shields.io/badge/whatsmeow-VoIP-25D366?logo=whatsapp&logoColor=white)](https://github.com/tulir/whatsmeow)
[![pion](https://img.shields.io/badge/pion-WebRTC-FF6B6B)](https://github.com/pion/webrtc)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](#license)
[![Docker](https://img.shields.io/badge/Docker-2496ED?style=flat&logo=docker&logoColor=white)](https://hub.docker.com/r/caedpaulofrossard/wacalls)

[Overview](#overview) · [Architecture](#architecture) · [Quick Start](#quick-start) · [API](#api) · [Security](#security) · [Documentation](#documentation)

</div>

---

## Overview

WaCalls pairs one or more WhatsApp accounts via **QR code** and lets you **place and
receive N:N voice calls** from any browser on the LAN.

The browser microphone is sent
as **raw 16 kHz PCM over a WebRTC data channel** to the Go server, which encodes it with
Meta's **MLow** codec and injects the media into WhatsApp's **SRTP relay** mesh — and the
reverse path brings the peer's audio back to the browser.

The entire VoIP stack runs **natively in pure Go**: the MLow voice codec, **RTP/SRTP**
packetization, **STUN**, the **WebRTC/SCTP relay** transport and the `<call>` signaling,
integrated with [**whatsmeow**](https://github.com/tulir/whatsmeow) and served to a
**React 19** client. There is **no cgo and no native DLL** — the MLow codec is a vendored
pure-Go package, so a plain `go build` produces a self-contained binary with live audio.

Multiple WhatsApp accounts can be paired and operated side by side, each with its own
pairing QR, connection status, and history.

A single account can also run **several
concurrent N:N calls** at once — one per browser operator — routed independently by call ID.

In local tests, it is possible to make one account number make N calls in parallel, in my test, 4:4 works.  

> **Status:** stable. Outgoing and incoming 4:4 calls reach `ACTIVE` with bidirectional in same time. audio, and a single account can hold several of them concurrently. Sessions persist in `wacalls.db` (pure-Go SQLite).

---

## Docker 
Simple version of the app in single docker file.

Verions: Alpine and Debian

[![Docker](https://img.shields.io/badge/Docker-2496ED?style=flat&logo=docker&logoColor=white)](https://hub.docker.com/r/caedpaulofrossard/wacalls)


**Quick run:**

```bash
docker image pull caedpaulofrossard/wacalls:debian
docker run -d -p 5176:8080 --name wecalls --restart=always caedpaulofrossard/wacalls:debian
firefox 0.0.0.0:5176
```

Done!

---

## Fork data

> Problems in original project and Reason for fork

### Problems in original project
- Voice return in 20-30% of the calls in my tests;
- The quality of audio is bad;
- Debug is not easy;
- Frontend minor errors;

### Reason of fork

Due legal rules and contract rules this fork it is necesary.

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────────────┐
│                          BROWSER (React client)                          │
│   mic + speaker  ·  WebRTC data channel (16 kHz PCM)  ·  HTTP + SSE      │
└───────────────────────────────┬──────────────────────────────────────────┘
                                │  POST /api/sessions/{sid}/calls/{id}/webrtc  (SDP)
                                │  GET  /api/events                            (SSE)
                                ▼
┌──────────────────────────── GO SERVER (cmd/server) ────────────────────────┐
│  SessionManager   registry of accounts (client + CallManager + bridge)     │
│  Broker           SSE hub (sessions, auth, call lifecycle fan-out)         │
│  Bridge           pion WebRTC bridge (16 kHz PCM data channel ⇄ call core) │
│                                                                            │
│  internal/wa      VoipSocket adapter over whatsmeow                        │
│  internal/voip    call · signaling · media · transport · core · wanode     │
└───────────────┬──────────────────────────────────────┬─────────────────────┘
                │ <call> signaling (Signal/USync)      │ SRTP media
                ▼                                      ▼
        ┌───────────────┐                    ┌──────────────────────┐
        │  WhatsApp WS  │                    │   WhatsApp relay     │
        │  (whatsmeow)  │                    │  (SRTP over SCTP/DC) │
        └───────────────┘                    └──────────────────────┘
```

### Layout

| Path | Responsibility |
|---|---|
| `cmd/server` | HTTP/SSE broker, session manager + store, WebRTC bridge, process lifecycle |
| `internal/wa` | `VoipSocket` — sends/receives `<call>` stanzas via whatsmeow |
| `internal/voip/core` | Domain types, constants, the `VoipSocket` interface |
| `internal/voip/wanode` | Shared WhatsApp-node and JID helpers |
| `internal/voip/media` | MLow codec (vendored pure-Go `mlow/`), RTP, SRTP, SSRC, PCM helpers, key derivation |
| `internal/voip/transport` | SCTP relay, STUN, subscription encoding |
| `internal/voip/signaling` | `<call>` stanza build/parse, call-key crypto, relay-ack parsing |
| `internal/voip/call` | `CallManager` — orchestrates a single call end to end |
| `client/` | React 19 + Vite + Tailwind v4 + shadcn/ui (dialer, call cards, sessions, history) |

---

## How a call flows

The core is `internal/voip/call.CallManager`, which drives a call end to end. Outgoing
call sequence:

```
1. POST .../calls            → CallManager.StartCall(peerJid)
                               generates a callID, builds the <call> offer, sends it

2. Browser opens WebRTC      → POST .../calls/{id}/webrtc (SDP offer)
                               the bridge answers with an SDP answer (pion)

3. Peer accepts              → events.CallAccept → HandleCallAccept
                               server receives <relay> + hop-by-hop keys

4. Relay transport           → STUN binding/allocate on WhatsApp relays
                               ICE + DTLS + SCTP DataChannel connect (pion)

5. SRTP media flowing        → state goes ACTIVE
   ├── uplink   (you → peer): browser 16 kHz PCM (data channel) → MLow encode → SRTP → relay
   └── downlink (peer → you): relay → SRTP → MLow decode → 16 kHz PCM (data channel) → browser

6. Teardown                  → DELETE .../calls/{id} or events.CallTerminate
                               CallManager.EndCall + bridge cleanup
```

Each protocol step (hop-by-hop SRTP key derivation, RTP packetization at `PT=120`/16 kHz,
STUN relay registration, relay-ack and `<call>` stanza parsing) is implemented and covered
by tests in `internal/voip` (`go test ./...`).

---

## Requirements

- **Go 1.26+**
- **Node 22+** and **npm** (only to build/run the React client)

No C compiler, cgo, or native libraries are required — the MLow codec is vendored
pure Go (`internal/voip/media/mlow`).

---

## Quick Start

```bash
# clone and enter the project
git clone <repo-url> wacalls-go
cd wacalls-go

# Go dependencies
go mod download

# React client dependencies
cd client && npm install && cd ..
```

### Run

```bash
go run ./cmd/server -addr :8080          # add -debug for verbose logs
```

Live audio works out of the box — the MLow codec is pure Go, so a plain build
includes it. No build tags, no `CGO_ENABLED`, no DLLs.

Open `http://localhost:8080`, click **New session**, and scan the QR shown in the browser
(it is also printed in the terminal) with **WhatsApp → Linked devices**. Add more accounts
the same way and switch between them in the sidebar.

### React client in dev mode

```bash
# the Vite dev proxy targets :3001 — start the backend on that port
go run ./cmd/server -addr :3001

cd client
npm run dev      # Vite on :5173, proxies /api → http://localhost:3001
```

For production, build the static client and serve it from the Go server:

```bash
cd client && npm run build && cd ..
go run ./cmd/server -static client/dist -addr :8080
```

### Server flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | HTTP listen address |
| `-db` | `wacalls.db` | SQLite session database path |
| `-static` | `client/dist` | Static client directory (optional) |
| `-debug` | `false` | Verbose logging (includes whatsmeow's internal log) |
| `-debug-log` | `true` | Emit audio diagnostic events to the ring behind `GET /api/debug/logs` |
| `-max-calls-per-session` | `8` | Max concurrent calls per session (`0` = unlimited) |
| `-srtp-auth` | `true` | Verify the SRTP HMAC tag on receive (`false` = legacy no-verify) |

---

## API

All routes are session-scoped. Events stream over a single SSE channel, tagged with the
originating `sessionId`.

| Method | Route | Purpose |
|---|---|---|
| `GET` | `/api/sessions` | List accounts (id, name, jid, status, paired) |
| `POST` | `/api/sessions` | Create an account and begin QR pairing |
| `DELETE` | `/api/sessions/{sid}` | Log out and remove an account |
| `POST` | `/api/sessions/{sid}/logout` | Disconnect an account (keep it for re-pairing) |
| `POST` | `/api/sessions/{sid}/pair` | Re-pair an account (emit a fresh QR) |
| `POST` | `/api/sessions/{sid}/calls` | Start an outgoing call (`{ phone, duration_ms?, record? }`) |
| `POST` | `/api/sessions/{sid}/calls/{id}/webrtc` | Exchange the browser WebRTC SDP |
| `POST` | `/api/sessions/{sid}/calls/{id}/accept` | Accept an incoming call |
| `POST` | `/api/sessions/{sid}/calls/{id}/reject` | Reject an incoming call |
| `DELETE` | `/api/sessions/{sid}/calls/{id}` | End an active call |
| `GET` | `/api/sessions/{sid}/history` | Recent call history (up to 50 records) |
| `GET` | `/api/events` | Server-sent events (sessions, auth, call lifecycle) |
| `GET` | `/api/debug/logs` | In-memory diagnostic ring (`?since=&limit=`, limit cap 500) |

---

## Documentation

Expanded technical + practical docs live in [`docs/`](./docs/README.md) —
[getting started](./docs/getting-started.md), [architecture](./docs/architecture.md),
[API reference](./docs/api.md), [media pipeline](./docs/media-pipeline.md),
[development](./docs/development.md), [security](./docs/security.md).

---

## Tests

```bash
go test ./...                 # media stack: SRTP, STUN, RTP, relay-ack, codec, state
cd client && npm run build    # client type-check + production build
```

---

## Security

The API has **no authentication** — anyone with HTTP access can create accounts, place
calls, and read history. **Run it only on a trusted LAN.** `wacalls.db` holds WhatsApp
session credentials (secrets): **do not commit it** and keep it protected.

---

## Contributors of the original project

This project builds on the work of:

<div align="center">
  
[**@jotadev66**](https://github.com/jotadev66) · [**@jobasfernandes**](https://github.com/jobasfernandes) · [**@edgardmessias**](https://github.com/edgardmessias) · [**@w3nder**](https://github.com/w3nder)

</div>

---

## Acknowledgements

- [**whatsmeow**](https://github.com/tulir/whatsmeow) — Go WhatsApp Web protocol library
- [**pion/webrtc**](https://github.com/pion/webrtc) — pure-Go WebRTC stack (ICE + DTLS + SCTP)
- [**whatsapp-rust**](https://github.com/oxidezap/whatsapp-rust) — reference MLow codec implementation (ported to the vendored pure-Go `internal/voip/media/mlow`)
- [**zapo**](https://github.com/w3nder/zapo) — VoIP media-stack reference

---

## License

[MIT](./LICENSE)
