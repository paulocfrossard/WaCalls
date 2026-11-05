# API reference

All routes are registered in `cmd/server/httpapi.go` (Go 1.22+ `ServeMux` method
patterns). The root [README](../README.md) keeps a one-table summary; this module is
the expanded contract for the `main` fork.

> ## ⚠ Security posture
>
> **There is no authentication on any route** — including `/api/events`,
> `/api/webhooks`, `/api/stats` and `/api/debug/logs`. `X-Client-Id` is an advisory
> operator id, **not a credential** — it only enforces "one active call per operator"
> and hides other operators' calls from the SSE/`call-list` view.
> CORS is `Access-Control-Allow-Origin: *` (allowed headers: `Content-Type`,
> `X-Client-Id`; allowed methods: `GET, POST, DELETE, OPTIONS`). The API relies
> entirely on being unreachable outside a trusted LAN. See [security](./security.md).

## Conventions

- Base URL: server `-addr` (default `:8080`; dev proxy targets `:3001`).
- Bodies are JSON. Success with no body returns **204**. Errors return a JSON object
  with an `"error"` string unless noted (the `call-list`/SSE frames use `"type"`).
- Bodies of `POST /calls/{id}/webrtc` and `POST /api/webhooks` are capped at **64 KiB**
  (`http.MaxBytesReader`); larger bodies fail parse with `400`.
- All session-scoped routes use `{sid}` = session id from `GET /api/sessions`.
  A `{sid}` that does not exist returns `404`.
- Operator identity: header `X-Client-Id: <id>` or query `?clientId=<id>`. It is used
  for the one-call-per-operator conflict (409) and for SSE owner scoping. Requests
  without it are treated as an anonymous client: the SSE stream and `call-list` show
  every call, but an anonymous request cannot accept/reject/end a call that a named
  client has already claimed (409 `claimed by another client`).

## Sessions

`SessionInfo` JSON: `{ "id", "name", "jid", "state", "paired" }`. `state` is the
auth state (`connecting`, `qr`, `open`, `logged_out`) — **not** `status`.

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `GET` | `/api/sessions` | — | `200` `{"sessions": SessionInfo[]}` | — | ordered by creation |
| `GET` | `/api/sessions/{sid}` | — | `200` `SessionInfo` | `404` | single account |
| `POST` | `/api/sessions` | `{"name"?: "..."}` (default `"Session"`) | `200` `{"id": "..."}` | `500` | creates account + starts QR (QR via SSE) |
| `DELETE` | `/api/sessions/{sid}` | — | `204` | `404` | log out **and** remove the account |
| `POST` | `/api/sessions/{sid}/logout` | — | `204` | `404` | disconnect, keep for re-pairing |
| `POST` | `/api/sessions/{sid}/pair` | — | `204` | `400` | emit a fresh QR (`session-qr` event); `400` if pairing cannot start |

## Calls

`CallRecord` JSON (SSE / history / webhooks): `{ sessionId, callId, owner, direction,
peer, startedAt, status, endedAt?, endReason? }`. `startedAt`/`endedAt` are epoch
milliseconds. `status` ∈ `starting | ringing | connected | ended`.

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `POST` | `/api/sessions/{sid}/calls` | `{"phone": "...", "duration_ms"?: n, "record"?: bool}` | `200` `{"call": {"callId": "..."}}` | `503` not paired, `409` operator already on a call, `429` per-session cap, `429` global cap, `400` bad body, `500` | starts the WhatsApp `<call>` offer |
| `POST` | `/api/sessions/{sid}/calls/{id}/webrtc` | `{"sdp_offer": "..."}` (≤64 KiB) | `200` `{"sdp_answer": "..."}` | `400` `sdp_offer required`, `404` no such call, `409` operator busy / claimed by another client, `500` | validates the body **before** claiming the call; attaches the browser `Bridge` (pion) |
| `POST` | `/api/sessions/{sid}/calls/{id}/accept` | — | `200` `{"call": {"callId": "..."}}` | `404` no such call, `409` not in an acceptable state / claimed by another client, `500` (accept send failed, rolled back) | claims + accepts an **inbound ringing** call. A retried accept on an already-accepted call is a no-op `200`. Accepting an outbound/live call returns `409` without tearing it down. |
| `POST` | `/api/sessions/{sid}/calls/{id}/reject` | — | `200` `{"status": "ok"}` | `404` no such call (in this session), `409` claimed by another client | rejects an inbound call |
| `DELETE` | `/api/sessions/{sid}/calls/{id}` | — | `204` | `404` no such call (in this session), `409` claimed by another client | ends an active call; id-scoped (never ends another call on the same manager) |

Call controls are **session- and owner-scoped**: `accept`/`reject`/`end` first resolve
the call through the URL's session registry, so a `callId` from another session can
never be controlled here.

## History

| Method | Path | Success | Notes |
|---|---|---|---|
| `GET` | `/api/sessions/{sid}/history` | `200` `{"rows": CallRecord[]}` | **Fixed window: last 50 records.** No pagination. |

## Health & stats

| Method | Path | Success | Notes |
|---|---|---|---|
| `GET` | `/healthz` | `200` `{"status": "ok"}` | liveness |
| `GET` | `/readyz` | `200` `{"status":"ok","activeCalls":n,"maxTotal":n}` · `503` `{"status":"draining",...}` | readiness; returns **503 while the server is draining** (SIGTERM) so orchestrators stop new traffic |
| `GET` | `/api/stats` | `200` `{activeCalls, maxTotal, goroutines, memoryBytes, sysBytes, gcPauseNs, openFds, droppedEvents}` | process/runtime snapshot. `openFds` is `-1` where `/proc` is unavailable; `droppedEvents` counts SSE events dropped because a subscriber buffer was full |

None of these require auth (LAN trust).

## Webhooks (outbound notifications)

External receivers (e.g. a Chatwoot integration) can register URLs that receive
HTTP POST notifications in real time whenever a call event occurs — no SSE
connection required. Managed via `cmd/server/webhookstore.go` + `webhook_worker.go`.

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `POST` | `/api/webhooks` | `{"url": "...", "secret": "...", "events": ["incoming", "call-ended"]}` (≤64 KiB) | `201` `{"id": "..."}` | `400` invalid JSON/url/secret/events (unknown or empty name, or host not in allowlist); `500` DB error | empty or omitted `events` = subscribe to **all** events; URL scheme must be http/https with a host; optional `-webhook-allow-hosts` restricts target hostnames |
| `GET` | `/api/webhooks` | — | `200` `{"webhooks": [{id, url, events, createdAt}]}` | `500` DB error | **`secret` is never returned** (write-only) |
| `DELETE` | `/api/webhooks/{id}` | — | `204` | `404` unknown id, `500` DB error | removes the webhook |

Same trust model as the rest of `/api`: no authentication on these routes
(LAN-facing). The authenticity of the outbound payload itself is guaranteed by
the HMAC signature (below), not by the management API.

### Event types eligible for webhooks

Only call-related events are delivered (session/QR/list events are internal-only):

| Event | Trigger |
|---|---|
| `incoming` | inbound call offer |
| `call-status` | status transition (`starting`/`ringing`/`connected` — teardown arrives only as `call-ended`, never as `status: "ended"`) |
| `call-ended` | call teardown |

### Delivery envelope

Every POST carries this JSON body:

```json
{
  "event": "incoming",
  "timestamp": "2026-10-23T14:30:00Z",
  "deliveryId": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
  "data": {
    "sessionId": "sess_123",
    "callId": "call_456",
    "peer": "553299999999",
    "direction": "inbound",
    "offeredAt": 1761234567890
  }
}
```

- `event` — one of the three types above.
- `timestamp` — RFC 3339 UTC, enqueued time.
- `deliveryId` — stable 32-hex id, **identical across all retry attempts of one event**. Use it as an idempotency/dedup key (retries can re-send the same body after an ambiguous timeout).
- `data` — event-specific fields. `sessionId`, `callId`, `peer`, `direction` (`inbound`/`outbound`) are always present for call events. `incoming` adds `offeredAt`; `call-status` adds `status`, `owner`, `startedAt`; `call-ended` adds `endReason`, `endedAt`, `owner`. **`startedAt`, `endedAt`, `offeredAt` are epoch milliseconds (UTC)**; the envelope `timestamp` is RFC 3339.

Webhooks always receive the **unfiltered** event (owner/session scoping applies only to
the SSE stream, not to outbound webhooks).

### Headers

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `User-Agent` | `WaCalls-Webhook/1.0` |
| `X-WaCalls-Event` | event type (`incoming` / `call-status` / `call-ended`) |
| `X-WaCalls-Delivery-Id` | stable 32-hex id, identical across all retry attempts of one event (also present in the signed `deliveryId` envelope field) |
| `X-WaCalls-Signature` | `sha256=<hex HMAC-SHA256 of the raw body>` |

### Verifying the signature (receiver side)

The signature is `HMAC-SHA256(raw_body_bytes, secret)` in lowercase hex,
prefixed with `sha256=`. Verify against the **raw request body** (before any
JSON parsing/re-serialization):

```
expected = "sha256=" + hex(HMAC_SHA256(raw_body, webhook_secret))
constant_time_equal(expected, header["X-WaCalls-Signature"])
```

If the header is missing or does not match, reject the delivery (HTTP 401) —
the sender **will not retry** (401 is a permanent 4xx). Return 500 or 429
instead if you want the delivery retried.

### Retry & resilience

- Delivery is **best-effort**: a buffered channel + worker pool (5 goroutines)
  decouples event emission from HTTP round-trip — a slow/dead receiver never
  blocks live calls. Events can be dropped (buffer full, retries exhausted,
  shutdown) and **are not durable across restarts**.
- On a transient failure — network error, `3xx`, `5xx`, `408`, `429` — the worker
  retries with **exponential backoff: 1s → 5s** (3 total attempts), then gives
  up and drops the event (logged via `slog`). Permanent `4xx` responses (400,
  401, 403, 404, …) are **not retried**.
- **Redirects are not followed** (`CheckRedirect = ErrUseLastResponse`): a
  `3xx` counts as a failed (but retryable) attempt. Register the final URL,
  not one that redirects.
- Hooks for one event are delivered **concurrently** (bounded by a global
  semaphore). There is **no cross-event ordering guarantee** — a slow receiver
  can invert `call-status`/`call-ended` delivery.
- If the internal buffer is full (many webhooks failing simultaneously), new
  events are **dropped with an error log** — the call itself is never stalled.
- **Duplicates are possible** (retry after an ambiguous timeout): receivers
  must be idempotent and should dedup on `deliveryId` (or the
  `X-WaCalls-Delivery-Id` header).

## Server-sent events

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/events` | One SSE channel for everything. Client id from `?clientId=` (the client uses this) or `X-Client-Id`. |

Query parameters:

| Param | Meaning |
|---|---|
| `clientId` | operator id used for owner scoping (also settable via `X-Client-Id`) |
| `sessions` | optional comma-separated session ids to scope the stream (e.g. `?sessions=s1,s2`). Omitted/empty = **all sessions**. |
| `apiKey` | **accepted and ignored** (forward-compatible; `EventSource` cannot send headers). The REST counterpart is `Authorization: Bearer <apiKey>`. |

Payloads are JSON objects with a `type` discriminator (client union: `BrokerEvent` in
`client/src/types/broker.ts`):

| `type` | Payload (main fields) | Meaning | Scoping |
|---|---|---|---|
| `session-list` | `sessions: SessionInfo[]` | full session snapshot | all sessions |
| `session-qr` | `sessionId`, `qr` | fresh pairing QR | session-filtered |
| `auth-state` | `sessionId`, `paired`, `state`, `qr?` | pairing/connection state change | session-filtered |
| `call-list` | `calls: CallListRow[]` | active calls snapshot (`sessionId`, `callId`, `owner`, `direction`, `peer`, `startedAt`, `status`, `endReason?`) | **per subscriber**: hides calls owned by another non-empty client; session-filtered |
| `call-status` | `sessionId`, `id`, `owner`, `status`, `peer`, `direction`, `startedAt` | status transition | session-filtered; owner-filtered (hidden from other operators once claimed) |
| `call-ended` | `sessionId`, `id`, `owner`, `peer`, `direction`, `reason`, `endedAt` | teardown | session-filtered; owner-filtered |
| `incoming` | `sessionId`, `id`, `peer`, `offeredAt` | inbound call offer | session-filtered; delivered to **all** operators (unclaimed) |
| `incoming-claimed` | `sessionId`, `id`, `owner` | an operator claimed the inbound call | session-filtered; **exempt from the owner filter** (every operator must clear its ringing entry) |

Call `status` values: `starting`, `ringing`, `connected`, `ended`
(`cmd/server/broker.go`). The `call-status` **event** never carries
`status: "ended"` — teardown arrives only as `call-ended` (history rows may
still show `ended`).

Slow subscribers: each SSE subscriber has a 32-frame buffer; frames that do not fit
are **dropped** (non-blocking send) and counted in `GET /api/stats` → `droppedEvents`.

## Debug logs

| Method | Path | Success | Notes |
|---|---|---|---|
| `GET` | `/api/debug/logs` | `200` `{"events": [...], "nextSeq": N}` | Query: `since=<seq>` (default `0`), `limit` (default `100`, **cap `500`**). Tail with `since=nextSeq`. |

Event shape (`internal/voip/logbuf`): `{id, seq, ts, origin, source, level, msg, fields?, iso}`.
Sources include `uplink-mic`, `uplink-frame`, `downlink-decode`, `session-audio`,
`bridge-uplink`, `bridge-downlink`, `relay-filter`, `dup-detector`, `srtp`, `encoder`,
`decoder`, `webrtc-assign`, `session`. Gated by `-debug-log` (**default `false`** now —
enable it explicitly to populate the ring).

## Static client

`GET /` serves files from `-static` (default `client/dist`) **only if the directory
exists**; otherwise that route is not registered. The dev UI (Vite on `:5173`) is not
part of this API — it proxies `/api` to `:3001`.

## Server flags that affect the API

| Flag | Default | Effect |
|---|---|---|
| `-addr` | `:8080` | listen address |
| `-max-calls-per-session` | `8` | `429` "max concurrent calls" when a session already has this many (`0` = unlimited) |
| `-max-total-calls` | `0` | `429` "max total calls" across all sessions (`0` = unlimited) |
| `-debug` | `false` | verbose logging (incl. whatsmeow) |
| `-debug-log` | `false` | populate `GET /api/debug/logs` ring |
| `-srtp-auth` | `true` | verify SRTP HMAC on receive |
| `-webhook-allow-hosts` | `""` | restrict webhook target hostnames |

## Error / status quick reference

- `200` — success with body (also idempotent accept retry and reject `{"status":"ok"}`)
- `201` — webhook created
- `204` — success, no body (delete/logout/pair/end call)
- `400` — malformed request / invalid webhook URL/events / invalid `sdp_offer`
- `404` — unknown session, or call not in this session
- `409` — operator already on a call / call claimed by another client / call not in an acceptable state
- `429` — per-session or global call limit exceeded
- `500` — internal error (e.g. failed accept send, DB error)
- `503` — session not paired; `/readyz` while draining

There is **no rate limiting** on any route.
