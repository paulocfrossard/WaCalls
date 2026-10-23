# Architecture

The root [README](../README.md) has the one-screen overview and the numbered call
sequence. This module goes one level deeper: process layout, module responsibilities,
the browser leg, and concurrency notes.

## System overview

Two components in one repo, one process for the backend:

```
Browser (client/)                        Go server (cmd/server)
┌──────────────────────────┐            ┌───────────────────────────────────┐
│ React 19 + Vite SPA      │  HTTP/SSE  │ httpapi.go  ── routes, no auth    │
│  pages/ components/      │◄──────────►│ broker.go   ── SSE fan-out        │
│  stores/ hooks/ services/│  /api/*    │ sessionmanager.go / sessionstore  │
│  sdk/src (webrtc, pcm)   │            │ callregistry.go / callrouting.go  │
│  sdk worklets (embedded)│  WebRTC    │ bridge.go   ── browser ↔ PCM leg  │
└──────────┬───────────────┘  (SDP)     └──────────────┬────────────────────┘
           │ datachannel "pcm" (Int16 LE, 16 kHz)      │
           │                                           ▼
           │                          internal/voip/call.CallManager
           │                          signaling ─ transport ─ media
           │                                  │
           │                                  ▼
           │                          internal/wa  (whatsmeow adapter)
           │                                  │
           └────────────────────  WhatsApp relays (STUN/SCTP/SRTP, MLow)
```

## Process layout (`cmd/server/`)

Entrypoint `main.go` parses the seven flags (see
[getting-started](./getting-started.md)), opens SQLite, wires a `server`, and serves
HTTP with graceful shutdown.

| File | Responsibility |
|---|---|
| `server.go` | `newServer`, lifecycle |
| `httpapi.go` | `ServeMux` route table (Go 1.22+ method patterns), JSON helpers, `clientID` resolution, debug-log handler |
| `broker.go` | In-memory SSE broker: session/call event fan-out, `CallStatus` states |
| `sessionmanager.go` / `sessionstore.go` | Account sessions + SQLite persistence (`sessions` table) |
| `callregistry.go` / `callrouting.go` | Active call bookkeeping and routing |
| `bridge.go` | WebRTC (pion) adapter: one data channel labeled **`pcm`**, Int16 LE PCM both ways |
| `session.go` / `whatsapp.go` | Per-account whatsmeow connection and pairing |

Tests are co-located `*_test.go` in `package main` (bridge, db, sessionstore,
callrouting, broker, sessionmanager, debuglogs, callregistry).

## `internal/` modules

| Package | Responsibility |
|---|---|
| `internal/wa` | `VoipSocket` adapter over whatsmeow — `<call>` stanza send/receive |
| `internal/voip/core` | Domain types, constants (e.g. `PayloadTypeWhatsAppOpus = 120`), `VoipSocket` interface |
| `internal/voip/wanode` | WhatsApp node + JID helpers |
| `internal/voip/signaling` | `<call>` stanza build/parse, call-key crypto, relay-ack parsing |
| `internal/voip/transport` | STUN binding/allocate against WhatsApp relays, SCTP relay, subscription encoding |
| `internal/voip/media` | MLow codec (vendored `mlow/`), RTP packetization, SRTP protect/unprotect, PCM helpers, key derivation |
| `internal/voip/call` | `CallManager` — one call end to end (offer → accept → media → teardown) |
| `internal/voip/logbuf` | In-memory diagnostic ring served by `GET /api/debug/logs` |

## Call lifecycle (server view)

Same six steps as the README sequence, with state names:

1. `POST …/calls` → `CallManager.StartCall` builds the `<call>` offer (`status: starting`).
2. `POST …/calls/{id}/webrtc` → `Bridge` answers the browser SDP offer (pion).
3. Peer accepts → signaling `CallAccept`, hop-by-hop keys received (`ringing` → media setup).
4. Relay transport: STUN + DTLS + SCTP on the data channel `pcm`.
5. SRTP media flowing → `connected` / ACTIVE:
   - **uplink:** browser 16 kHz PCM over datachannel → MLow encode → SRTP → relay
   - **downlink:** relay → SRTP unprotect → MLow decode → PCM over datachannel → browser
6. Teardown: `DELETE …/calls/{id}` or remote terminate → `ended`, bridge cleanup.

Call statuses in SSE events: `starting | ringing | connected | ended`
(`cmd/server/broker.go`).

## Browser leg (`client/`)

No client-side router — navigation is component state (`pages/CallsPage`,
`pages/DebugPage`). State is split deliberately:

- **Zustand stores** (`stores/`: calls, sessions, devices, ui, theme) — local/UI/device state.
- **TanStack Query** (`lib/query.ts` + `hooks/`) — server state (sessions, history, mutations).

Key libraries:

| File | Role |
|---|---|
| `lib/api.ts` | fetch helpers; attaches advisory `X-Client-Id` header |
| `sdk/src/index.ts` (SDK package) | `EventSource` on `/api/events?clientId=…`; typed `BrokerEvent` union in `client/src/types/broker.ts` |
| `sdk/src/core/webrtc.ts` (SDK package) | peer connection, SDP exchange, data channel `pcm` glue |
| `sdk/src/utils/pcm.ts` (SDK package) | Float32 ↔ Int16 LE conversion |
| `lib/debug-log.ts`, `lib/client-id.ts` | debug polling, stable client id |
| `components/domain/call/CallCard.tsx` | in-call VU/spectrum meters (AnalyserNode on remoteStream/mic) |
| `sdk/src/worklets/sources.ts` (SDK package) | embedded capture/playback processor sources, installed via Blob URL |

Imports use the `@/` alias → `client/src` (configured in `vite.config.ts`).

## Concurrency notes

- One goroutine model per call inside `CallManager`; the bridge pumps uplink/downlink
  over the data channel without the core knowing the transport (core only sees
  `[]float32` PCM).
- Per-session call limit enforced at the HTTP layer (`-max-calls-per-session`, default 8).
- Single-operator conflict: `X-Client-Id` (header or `?clientId=`) is **advisory**, used
  only to refuse a second concurrent operator on the same session (409) — not a credential.
- The SSE broker fans out per client id; clients reconnect with the same id to reduce
  duplicate operator state.

## Where to go next

- HTTP surface → [api](./api.md)
- Audio path details → [media-pipeline](./media-pipeline.md)
- Build/test workflow → [development](./development.md)
- Threat model before any deployment → [security](./security.md)
