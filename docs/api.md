# API reference

All routes are registered in `cmd/server/httpapi.go` (Go 1.22+ `ServeMux` method
patterns). The root [README](../README.md) keeps a one-table summary; this module is
the expanded contract.

> ## ⚠ Security posture
>
> **Every `/api/*` route requires authentication**, except `POST /api/auth/login`
> and `POST /api/auth/logout`. Browser sessions authenticate with the `HttpOnly`
> `wacalls_session` cookie set by login; API clients use an API key. The SSE stream
> uses a short-lived one-time ticket (`?ticket=`) — see [Authentication](#authentication).
> `X-Client-Id` remains an advisory operator id, **not a credential**. CORS allows
> any origin by default; restrict it with `-allowed-origins`. See
> [security](./security.md).

## Conventions

- Base URL: server `-addr` (default `:8080`; dev proxy targets `:3001`).
- Bodies are JSON. Success with no body returns **204**. Errors return a JSON object
  with an `"error"` string unless noted.
- All session-scoped routes use `{sid}` = session id from `GET /api/sessions`.
- Operator identity: header `X-Client-Id: <id>` or query `?clientId=<id>` — advisory UI
  and stream identity only. It is **not** the conflict key: call start/accept is bucketed
  per authenticated user (`user:<userID>`), so an account holds one active
  outgoing/accepted call across tabs and devices (`409` otherwise); in `-auth-disabled`
  mode the bucket is `client:<clientID>`, falling back to the peer IP when no client id
  is supplied. It is **not** a credential.

## Authentication

All routes below require a credential unless marked public. `POST /api/auth/login`
and `POST /api/auth/logout` are public; every other `/api/*` route is protected by
`authMiddleware` (`cmd/server/auth.go`).

### Credential precedence

Send **one** credential. For REST routes the server reads them in this order
(`cmd/server/auth.go`):

1. `Authorization: Bearer <session token or API key>` — tried as a session
   token first, then as an API key. Login never returns a token, so the bundled
   browser client uses the cookie instead.
2. `X-API-Key: <key>` — an API key.
3. Cookie `wacalls_session` — set by login, `HttpOnly`, `SameSite=Lax`,
   `Secure` by default.

The SSE route (`GET /api/events`) is the exception: `EventSource` cannot set
headers, so callers exchange their credential for a single-use `?ticket=` first.
See [Server-sent events](#server-sent-events).

A present-but-malformed `Authorization` header is rejected with `401`; it does not
fall through to a weaker source. No REST route accepts a credential in the query
string, which keeps secrets out of access logs and `Referer` headers.

### Browser vs. API-client logins

- **Browser (SPA):** `POST /api/auth/login` sets the `HttpOnly`
  `wacalls_session` cookie and returns the user/expiry only — **no token in the
  body**. Every subsequent request authenticates with the cookie the browser sends
  automatically.
- **API clients / scripts:** create an API key as an admin (`POST /api/auth/keys`)
  and send it in `Authorization: Bearer <key>` or `X-API-Key`. There is no bearer
  session token to copy out of a login response.

CSRF guard: a non-GET request authenticated **by the session cookie** (no header
credential) that carries an `Origin` header must have an `Origin` matching the
request host or a configured `-allowed-origins` entry; otherwise the server
returns `403`. Requests without an `Origin` header, and requests authenticated by
`Authorization`/`X-API-Key`, are not CSRF-checked.

### Auth and key routes

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `POST` | `/api/auth/login` | `{"username": "...", "password": "..."}` | `201` `{"expiresAt", "user": {"id", "username", "role"}}` | `400` malformed JSON, `401` invalid credentials, `429` rate limited, `503` auth not configured | sets the `HttpOnly` `wacalls_session` cookie; **no token in the body** |
| `POST` | `/api/auth/logout` | — | `204` | — | **public**; revokes the session carried by the request when a credential is supplied, and always clears the cookie |
| `GET` | `/api/auth/me` | — | `200` `{"user": {"id", "username", "role"}}` | `401` | resolves the current credential |
| `POST` | `/api/auth/sse-ticket` | — | `201` `{"ticket": "<hex>", "expiresAt": <unix ms>}` | `401` | mints a **single-use, 60 s** SSE ticket for any valid credential; use it as `GET /api/events?ticket=…` |
| `POST` | `/api/auth/keys` | `{"name": "..."}` | `201` `{"id", "name", "key", "prefix", "createdAt"}` | `400`, `403` | **admin only**; the raw `key` is shown **once** |
| `GET` | `/api/auth/keys` | — | `200` key metadata | `403` | **admin only**; no raw keys, no hashes |
| `DELETE` | `/api/auth/keys/{id}` | — | `204` | `403`, `404` | **admin only**; revokes the key |

### Roles

| Role | Can do |
|---|---|
| `admin` | API key management (`/api/auth/keys*`), webhook management (`/api/webhooks*`), and `GET /api/debug/logs`; everything an operator can do |
| `operator` | Everything else |

### Status semantics

- `401` — no credential, malformed `Authorization`, an unknown/expired/revoked
  key, or an unknown/expired/already-used SSE ticket.
- `403` — authenticated but not allowed: insufficient role, or a cookie-only write
  from a disallowed `Origin`.
- `429` — **login only**: more than **10 failed attempts/min per `(IP, username)`**,
  **30 attempts/min per IP**, or **300 attempts/min globally**. A successful login
  clears its `(IP, username)` bucket and refunds one unit from the IP and global
  counters; failed attempts are never refunded. Other routes are not rate-limited.

`-auth-disabled` (dev only) bypasses the middleware with a synthetic `admin`
principal — never use it on a shared network.

## Sessions

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `GET` | `/api/sessions` | — | `200` list of `{id, name, jid, state, paired}` | — | ordered by creation |
| `POST` | `/api/sessions` | `{"name": "..."}` | `201` `{"id": "..."}` (pairing begins, QR via SSE) | — | creates account + starts QR |
| `DELETE` | `/api/sessions/{sid}` | — | `204` | `404` | log out **and** remove the account |
| `POST` | `/api/sessions/{sid}/logout` | — | `204` | `404` | disconnect, keep for re-pairing |
| `POST` | `/api/sessions/{sid}/pair` | — | `204` | `404` | emit a fresh QR (`session-qr` event) |

## Calls

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `POST` | `/api/sessions/{sid}/calls` | `{"phone": "...", "duration_ms"?: n, "record"?: bool}` | `200` `{"call": {"callId": "..."}}` | `503` unpaired, `409` operator busy, `429` over `-max-calls-per-session`, `400`/`500` | starts the WhatsApp `<call>` offer |
| `POST` | `/api/sessions/{sid}/calls/{id}/webrtc` | `{"sdp_offer": "..."}` | `200` `{"sdp_answer": "..."}` | `404` | attaches the browser `Bridge` (pion) |
| `POST` | `/api/sessions/{sid}/calls/{id}/accept` | — | `200` `{"call": {"callId": "..."}}` | `409` claimed/busy, `404` | claim + accept an incoming call |
| `POST` | `/api/sessions/{sid}/calls/{id}/reject` | — | `200` **always** (idempotent, even if absent) | — | reject incoming |
| `DELETE` | `/api/sessions/{sid}/calls/{id}` | — | `204` | `404` | end an active call |

## History

| Method | Path | Success | Notes |
|---|---|---|---|
| `GET` | `/api/sessions/{sid}/history` | `200` rows | **Fixed window: last 50 records.** No pagination. |

Rows are scoped to the authenticated principal **before** the 50-record cap
(`cmd/server/callvisibility.go`): `admin` (and `-auth-disabled`) receives every
row for the session; an `operator` receives only rows whose `ownerKey` is
`user:<ownUserID>` — other operators' rows and `client:<id>` / `client:<ip>`
orphans are excluded, and an empty/unknown role fails closed. Filtering is
server-side only: each row's `owner` (raw client id) is unchanged and
`ownerKey` is never returned.

## Webhooks (outbound notifications)

External receivers (e.g. a Chatwoot integration) can register URLs that receive
HTTP POST notifications in real time whenever a call event occurs — no SSE
connection required. Managed via `cmd/server/webhookstore.go` + `webhook_worker.go`.

| Method | Path | Body | Success | Errors | Notes |
|---|---|---|---|---|---|
| `POST` | `/api/webhooks` | `{"url": "...", "secret": "...", "events": ["incoming", "call-ended"]}` | `201` `{"id": "..."}` | `400` invalid body/url/secret/events (unknown or empty name, or host not in allowlist); `500` DB error | empty or omitted `events` = subscribe to **all** events; URL scheme must be http/https with a host; optional `-webhook-allow-hosts` restricts target hostnames |
| `GET` | `/api/webhooks` | — | `200` `{"webhooks": [{id, url, events, createdAt}]}` | — | **`secret` is never returned** (write-only) |
| `DELETE` | `/api/webhooks/{id}` | — | `204` | `404` unknown id, `500` DB error | removes the webhook |

These routes are **`admin` only** (`requireRole("admin")`) on top of the same
credential as the rest of `/api` (see [Authentication](#authentication));
`-allowed-origins` and the CSRF rule apply
too. The authenticity of the outbound payload itself is guaranteed by the HMAC
signature (below), not by the management API.

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
| `POST` | `/api/auth/sse-ticket` | Exchange any valid credential for `{"ticket", "expiresAt"}`. Single-use, 60 s TTL. |
| `GET` | `/api/events` | One SSE channel for everything. **Requires authentication** — a single-use `?ticket=` (preferred), or the `wacalls_session` cookie when no ticket is supplied. Client id from `?clientId=` (the client uses this) or `X-Client-Id`. |

Because `EventSource` cannot send headers, the SSE stream is authenticated with a
**one-time ticket** (or the session cookie when no ticket is supplied) — the
ticket is the only query credential the server accepts:

```
POST /api/auth/sse-ticket     # with cookie, Bearer API key, or X-API-Key
→ 201 {"ticket": "<hex>", "expiresAt": <unix ms>}

GET /api/events?ticket=<hex>&clientId=<id>
```

Rules:

- A ticket is **single-use** and expires after **60 s**. Reuse or expiry returns
  `401`; fetch a fresh ticket for every (re)connect.
- The ticket is scoped to `GET /api/events` and carries the identity and role of
  the credential that minted it.
- Session tokens and API keys are **never accepted in the URL**. The only query
  credential is a ticket — short-lived and single-use precisely so a leaked URL is
  not a durable credential.
- The browser obtains the ticket with `credentials: "include"` (cookie login). A
  script can mint one with its API key and pass the ticket to `EventSource`.

Payloads are JSON objects with a `type` discriminator (client union: `BrokerEvent` in
`client/src/types/broker.ts`):

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

`call-list` is scoped per subscriber: the initial snapshot on connect and every
subsequent broadcast are filtered server-side, so `admin` and `-auth-disabled`
receive all active calls (including `client:<id>` / `client:<ip>` orphans) while
an `operator` receives only calls bucketed `user:<ownUserID>`; an empty/unknown
role fails closed. The row `owner` field stays the raw client id (UI "my call"
check) and `ownerKey` is never serialized.

## Debug logs

| Method | Path | Success | Notes |
|---|---|---|---|
| `GET` | `/api/debug/logs` | `200` `{"events": [...], "nextSeq": N}` | Query: `since=<seq>` (default `0`), `limit` (default `100`, **cap `500`**). Tail with `since=nextSeq`. Requires the `admin` role. |

Event shape (`internal/voip/logbuf`): `{seq, ts, source, level, msg, fields?}`.
Sources include `bridge-uplink`, `bridge-downlink`, plus client-side debug rows
mirrored via the UI. Gated by `-debug-log` (default `true`).

## Static client

`GET /` serves files from `-static` (default `client/dist`) **only if the directory
exists**; otherwise that route is not registered. For non-`/api` paths without a
matching static file, the fallback distinguishes the two cases:

- **Path has a file extension** (e.g. `/assets/app.js`) → `404`, so probes and
  monitoring still see a missing asset instead of an HTML shell.
- **Path is extensionless** (e.g. `/login`) → `index.html`, so the client router
  handles it.

The dev UI (Vite on `:5173`) is not part of this API — it proxies `/api` to `:3001`.

## Error / status quick reference

- `200` — success with body (or idempotent reject)
- `201` — created (login, API key, session)
- `204` — success, no body (delete/logout/pair/end call)
- `400` — malformed request
- `401` — missing, malformed, expired, or revoked credential
- `403` — authenticated but not allowed (role, or disallowed `Origin` on a cookie-only write)
- `404` — unknown session, call, webhook, or key
- `409` — operator busy / call already claimed
- `429` — session call limit exceeded, or too many login attempts (10/min per IP + username, 30/min per IP, 300/min global)
- `503` — session not paired, or auth not configured

Only the login route is rate-limited. Everything else relies on a valid credential.
