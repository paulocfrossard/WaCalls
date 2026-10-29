# Architecture

The root [README](../README.md) has the one-screen overview and the numbered call
sequence. This module goes one level deeper: process layout, module responsibilities,
the browser leg, and concurrency notes.

## System overview

Two components in one repo, one process for the backend:

```
Browser (client/)                        Go server (cmd/server)
┌──────────────────────────┐            ┌───────────────────────────────────┐
│ React 19 + Vite SPA      │  HTTP/SSE  │ httpapi.go  ── routes + auth      │
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

Entrypoint `main.go` parses the flags (see
[getting-started](./getting-started.md)), opens SQLite, wires a `server`, and serves
HTTP with graceful shutdown.

| File | Responsibility |
|---|---|
| `server.go` | `newServer`, lifecycle |
| `httpapi.go` | `ServeMux` route table (Go 1.22+ method patterns), JSON helpers, `clientID` resolution, debug-log handler |
| `auth.go` | `authMiddleware`: credential precedence, session/API-key lookup, CSRF origin check, `requireRole`, audit writer |
| `authapi.go` | Public login/logout, `GET /api/auth/me`, API-key CRUD, login rate limiter |
| `authstore.go` | SQLite auth tables: users, sessions, API keys, audit log |
| `broker.go` | In-memory SSE broker: session/call event fan-out, `CallStatus` states |
| `sessionmanager.go` / `sessionstore.go` | Account sessions + SQLite persistence (`sessions` table) |
| `callregistry.go` / `callrouting.go` | Active call bookkeeping and routing |
| `bridge.go` | WebRTC (pion) adapter: one data channel labeled **`pcm`**, Int16 LE PCM both ways |
| `session.go` / `whatsapp.go` | Per-account whatsmeow connection and pairing |

Tests are co-located `*_test.go` in `package main` (bridge, db, sessionstore,
callrouting, broker, sessionmanager, debuglogs, callregistry).

## Authentication

`server.routes()` wraps the `/api` surface in `authMiddleware` (`cmd/server/auth.go`).
REST requests authenticate with `Authorization: Bearer <session token or API key>`,
`X-API-Key`, or the `wacalls_session` cookie; the middleware rejects a malformed
`Authorization` header with `401` instead of falling through. Sessions and API keys are stored as
SHA-256 hashes and looked up by hash.

**Browser login is cookie-only.** `POST /api/auth/login` sets the `HttpOnly`
`wacalls_session` cookie and returns only `{expiresAt, user}` — no session token in
the body and nothing in `localStorage`. API clients (scripts, integrations) use
admin-issued API keys instead.

**SSE uses a one-time ticket.** `EventSource` cannot set headers, so the client
calls `POST /api/auth/sse-ticket` with its cookie (or an API key) to mint a
single-use, 60 s ticket, then connects to `GET /api/events?ticket=…`. The ticket is
consumed on use and scoped to that route; with no ticket the stream falls back to
the session cookie, and no other query-string credential is accepted.

Public routes — reachable without a valid session — are `POST /api/auth/login`,
`POST /api/auth/logout`, and everything outside `/api/` (static assets + SPA
fallback). `POST /api/auth/sse-ticket` requires a valid credential.
`requireRole` gates admin-only routes (`/api/auth/keys*`, `GET /api/debug/logs`). A
cookie-authenticated non-GET request carrying an `Origin` header must match the
request host or `-allowed-origins` (CSRF guard). `-auth-disabled` is a dev-only
escape hatch that injects a synthetic `admin` principal; a nil auth store makes the
middleware a no-op. See [api](./api.md#authentication) and [security](./security.md).

### Auth design decisions

The choices behind the surface above. **(implemented)** is shipped;
**(planned)** is an agreed target that is not built yet.

1. **Opaque sessions instead of JWT (implemented).** A login mints 32 random bytes;
   only their SHA-256 lands in `sessions_auth.token_hash` (unique index). Each request
   reloads the row and rejects it when `revoked` or past the `-session-ttl` deadline.
   The browser carries it in the `HttpOnly` `wacalls_session` cookie.
   *Rationale:* the server is single-instance on SQLite. A stateless JWT would still
   need a revocation list plus signing and key rotation; an opaque session gives
   immediate revocation with one indexed `SELECT`.
   *Consequences:* session auth depends on database availability, and logout, expiry,
   and revocation all take effect immediately. JWTs are not used anywhere.

2. **API keys for integrations (implemented).** Admins issue keys; the secret is
   stored as a SHA-256 hash and shown once, with a short prefix kept for listing.
   Keys are revocable and authenticate via `X-API-Key` or `Authorization: Bearer`.

3. **Single-use SSE ticket (implemented).** Because `EventSource` cannot set headers,
   the ticket keeps a long-lived credential out of the event-stream URL. It is 60 s,
   single-use, and scoped to `GET /api/events`; `Authorization: Bearer` is also
   accepted directly. The client SDK uses the native `EventSource` —
   `@microsoft/fetch-event-source` is not a dependency.

4. **SSE revalidation and revocation (implemented).** Subscribers carry
   `sessionID` / `keyID` / `userID`; a revocation channel closes matching streams
   immediately; the 20 s keepalive revalidates read-only, fail-closed.

5. **RBAC (current).** Roles are `admin` and `operator`. Webhooks move to
   `admin` because outbound URLs are an SSRF/egress surface.

   | Surface | Current | Target |
   |---|---|---|
   | API keys (`/api/auth/keys*`), debug logs | `admin` | `admin` |
   | Sessions, calls, history, events | `operator` | `operator` |
   | Webhooks (`/api/webhooks*`) | any authenticated | `admin` |

   Session management stays `operator` because it is the product's main flow. Per-user
   ownership on the `sessions` table is deferred; the system is single-tenant.

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

- **Zustand stores** (`stores/`: calls, sessions, devices, ui, theme, auth) — local/UI/device/session-auth state.
- **TanStack Query** (`lib/query.ts` + `hooks/`) — server state (sessions, history, mutations).

Key libraries:

| File | Role |
|---|---|
| `lib/api.ts` | fetch helpers; attaches advisory `X-Client-Id` header |
| `sdk/src/index.ts` (SDK package) | `EventSource` on `/api/events?ticket=…&clientId=…`, after minting a ticket via `/api/auth/sse-ticket`; typed `BrokerEvent` union in `client/src/types/broker.ts` |
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
- The SSE broker fans out per principal; clients reconnect with the same id to reduce
  duplicate operator state.

## Where to go next

- HTTP surface → [api](./api.md)
- Audio path details → [media-pipeline](./media-pipeline.md)
- Build/test workflow → [development](./development.md)
- Threat model before any deployment → [security](./security.md)
