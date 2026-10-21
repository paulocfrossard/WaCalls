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
| `call-status` | `sessionId`, `id`, `owner`, `status`, `peer`, `startedAt` | status transition |
| `call-ended` | `sessionId`, `id`, `owner`, `reason`, `endedAt` | teardown |
| `incoming` | `sessionId`, `id`, `peer`, `offeredAt` | inbound call offer |
| `incoming-claimed` | `sessionId`, `id`, `owner` | an operator claimed the inbound call |

Call `status` values: `starting`, `ringing`, `connected`, `ended`
(`cmd/server/broker.go`).

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
