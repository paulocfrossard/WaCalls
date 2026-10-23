# API reference

All routes are registered in `cmd/server/httpapi.go` (Go 1.22+ `ServeMux` method
patterns). The root [README](../README.md) keeps a one-table summary; this module is
the expanded contract.

> ## ⚠ Security posture
>
> **There is no authentication on any route** — including `/api/events` and
> `/api/debug/logs`. `X-Client-Id` is an advisory operator id, **not a credential**.
> CORS is `Access-Control-Allow-Origin: *`. The API relies entirely on being
> unreachable outside a trusted LAN. See [security](./security.md).

## Conventions

- Base URL: server `-addr` (default `:8080`; dev proxy targets `:3001`).
- Bodies are JSON. Success with no body returns **204**. Errors return a JSON object
  with an `"error"` string unless noted.
- All session-scoped routes use `{sid}` = session id from `GET /api/sessions`.
- Operator identity: header `X-Client-Id: <id>` or query `?clientId=<id>` — used only
  for single-operator conflict (409) on call start/accept.

## Sessions

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `GET` | `/api/sessions` | — | `200` list of `{id, name, jid, status, paired}` | — | ordered by creation |
| `POST` | `/api/sessions` | `{"name": "..."}` | `200` `{"id": "..."}` (pairing begins, QR via SSE) | — | creates account + starts QR |
| `DELETE` | `/api/sessions/{sid}` | — | `204` | `404` | log out **and** remove the account |
| `POST` | `/api/sessions/{sid}/logout` | — | `204` | `404` | disconnect, keep for re-pairing |
| `POST` | `/api/sessions/{sid}/pair` | — | `204` | `404` | emit a fresh QR (`session-qr` event) |

## Calls

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `POST` | `/api/sessions/{sid}/calls` | `{"phone": "...", "duration_ms"?: n, "record"?: bool}` | `200` `{"callId": "..."}` | `503` unpaired, `409` operator busy, `429` over `-max-calls-per-session`, `400`/`500` | starts the WhatsApp `<call>` offer |
| `POST` | `/api/sessions/{sid}/calls/{id}/webrtc` | `{"sdp_offer": "..."}` | `200` `{"sdp_answer": "..."}` | `404` | attaches the browser `Bridge` (pion) |
| `POST` | `/api/sessions/{sid}/calls/{id}/accept` | — | `200` | `409` claimed/busy, `404` | claim + accept an incoming call |
| `POST` | `/api/sessions/{sid}/calls/{id}/reject` | — | `200` **always** (idempotent, even if absent) | — | reject incoming |
| `DELETE` | `/api/sessions/{sid}/calls/{id}` | — | `204` | `404` | end an active call |

## History

| Method | Path | Success | Notes |
|---|---|---|---|
| `GET` | `/api/sessions/{sid}/history` | `200` rows | **Fixed window: last 50 records.** No pagination. |

## Webhooks (outbound notifications)

External receivers (e.g. a Chatwoot integration) can register URLs that receive
HTTP POST notifications in real time whenever a call event occurs — no SSE
connection required. Managed via `cmd/server/webhookstore.go` + `webhook_worker.go`.

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `POST` | `/api/webhooks` | `{"url": "...", "secret": "...", "events": ["incoming", "call-ended"]}` | `201` `{"id": "..."}` | `400` invalid body/url/secret/events (unknown or empty name, or host not in allowlist); `500` DB error | empty or omitted `events` = subscribe to **all** events; URL scheme must be http/https with a host; optional `-webhook-allow-hosts` restricts target hostnames |
| `GET` | `/api/webhooks` | — | `200` `{"webhooks": [{id, url, events, createdAt}]}` | — | **`secret` is never returned** (write-only) |
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

Payloads are JSON objects with a `type` discriminator (client union: `BrokerEvent` in
`client/src/lib/event-stream.ts`):

| `type` | Payload (main fields) | Meaning |
|---|---|---|
| `session-list` | `sessions: SessionInfo[]` | full session snapshot |
| `session-qr` | `sessionId`, `qr` | fresh pairing QR |
| `auth-state` | `sessionId`, `paired`, `state`, `qr?` | pairing/connection state change |
| `call-list` | `calls: CallListRow[]` | active calls snapshot (`sessionId`, `callId`, `owner`, `direction`, `peer`, `startedAt`, `status`, `endReason?`) |
| `call-status` | `sessionId`, `id`, `owner`, `status`, `peer`, `direction`, `startedAt` | status transition |
| `call-ended` | `sessionId`, `id`, `owner`, `peer`, `direction`, `reason`, `endedAt` | teardown |
| `incoming` | `sessionId`, `id`, `peer`, `offeredAt` | inbound call offer |
| `incoming-claimed` | `sessionId`, `id`, `owner` | an operator claimed the inbound call |

Call `status` values: `starting`, `ringing`, `connected`, `ended`
(`cmd/server/broker.go`). The `call-status` **event** never carries
`status: "ended"` — teardown arrives only as `call-ended` (history rows may
still show `ended`).

## Debug logs

| Method | Path | Success | Notes |
|---|---|---|---|
| `GET` | `/api/debug/logs` | `200` `{"events": [...], "nextSeq": N}` | Query: `since=<seq>` (default `0`), `limit` (default `100`, **cap `500`**). Tail with `since=nextSeq`. |

Event shape (`internal/voip/logbuf`): `{seq, ts, source, level, msg, fields?}`.
Sources include `bridge-uplink`, `bridge-downlink`, plus client-side debug rows
mirrored via the UI. Gated by `-debug-log` (default `true`).

## Static client

`GET /` serves files from `-static` (default `client/dist`) **only if the directory
exists**; otherwise that route is not registered. The dev UI (Vite on `:5173`) is not
part of this API — it proxies `/api` to `:3001`.

## Error / status quick reference

- `200` — success with body (or idempotent reject)
- `204` — success, no body (delete/logout/pair/end call)
- `400` — malformed request
- `404` — unknown session or call
- `409` — operator busy / call already claimed
- `429` — session call limit exceeded
- `503` — session not paired

There is **no rate limiting** on any route.
