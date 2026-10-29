# Security

Operational threat model for wacalls. The root [README](../README.md) carries the
short warning; this module is the full posture.

## The core rule

> **Every `/api/*` route requires authentication**, except `POST /api/auth/login`
> and `POST /api/auth/logout`. Browsers authenticate with the `HttpOnly`
> `wacalls_session` cookie; API clients use an API key (`Authorization: Bearer` or
> `X-API-Key`). `GET /api/events` authenticates with a single-use `?ticket=`.
> Failed logins are rate-limited and CORS can be pinned to an allowlist — but this
> is still not a hardened internet-facing service: **bind it to localhost or a
> trusted LAN**.

Enforcement lives in `cmd/server/auth.go` (public paths, credential precedence,
CSRF check) with route registration and role gates in `cmd/server/httpapi.go`.
Sessions and API keys are stored only as SHA-256 hashes. Admin bootstrap is
**environment-only, fail-closed**: when the users table is empty the password must
come from `WACALLS_ADMIN_PASSWORD`; if it is missing, startup aborts instead of
creating a default or writing a password file. The password is bcrypt-hashed at
cost 12 (`cmd/server/authstore.go`).

## Attack surface

| Surface | Exposure | Impact if reachable by an attacker |
|---|---|---|
| All `/api/*` routes | **Auth required** (`Authorization: Bearer <session token or API key>`, `X-API-Key`, or the `wacalls_session` HttpOnly cookie); login limited to 10 failed/min per `(IP, username)`, 30/min per IP, and 300/min globally; CORS `*` by default — restrict with `-allowed-origins` | With `operator` credentials: create/delete sessions (WhatsApp accounts), place/receive/end calls, read call history and peer numbers. With `admin`: also manage API keys and read diagnostics |
| `GET /api/events` (SSE) | **Auth required**: a single-use `?ticket=` (60 s TTL) minted by `POST /api/auth/sse-ticket`, or the session cookie when no ticket is supplied (EventSource cannot set headers). A leaked URL is only briefly replayable | Real-time surveillance of session pairing, call lifecycle, peer metadata |
| `POST/GET/DELETE /api/webhooks` | **Auth required, `admin` role only** (`requireRole("admin")`); an admin-registered URL still makes the server issue outbound requests | **Server-side request forgery (SSRF) egress**: the server sends signed POSTs to any http/https URL the caller supplies, including internal hosts (`http://10.x/...`), loopback services, and cloud metadata (`http://169.254.169.254/...`). Delivery is blind (no response body is echoed back), so this is a side-effect/egress vector, not an exfiltration channel — but it can trigger internal actions and, if the receiver echoes data elsewhere, aid pivoting |
| `GET /api/debug/logs` | **Auth required, `admin` role only** (`-debug-log`, default **on**) | Leak of call IDs, SSRCs, audio-level telemetry, ICE/DC states |
| `GET /` static files | Public (assets + SPA fallback, including `/login`); only when `-static` dir exists | Low — serves the SPA bundle only |
| `wacalls.db` (SQLite) | Filesystem | **WhatsApp session credentials (secrets)** plus auth sessions/keys (hashed). Full account takeover of linked devices |

## Operator identity vs. authentication

- Clients send an advisory operator id: header `X-Client-Id` or query `?clientId=`
  (`client/src/lib/client-id.ts`, `lib/api.ts`).
- The server uses it only for advisory UI/stream identity. The **conflict bucket** for
  call start/accept is the authenticated user (`user:<userID>`): an account holds at
  most one active outgoing/accepted call across tabs and devices, so a second
  start/accept returns HTTP 409. In `-auth-disabled` mode the bucket is
  `client:<clientID>`, falling back to the peer IP when no `X-Client-Id`/`?clientId=`
  is supplied, so id-less callers do not collapse into a single bucket. The id is
  **not verified**, not a credential — authentication is
  a separate layer (API key or session cookie).

## Session cookie and SSE tickets

Login is **cookie-only for the browser**: `POST /api/auth/login` sets the
`HttpOnly` `wacalls_session` cookie and returns only `{expiresAt, user}` — the token
is **never sent in the response body** and is **not persisted in `localStorage`**.
Because JavaScript cannot read an `HttpOnly` cookie, an XSS bug in the served origin
cannot exfiltrate the session credential directly. The cookie is `HttpOnly`,
`SameSite=Lax`, and `Secure` by default (`-cookie-secure=true`).

`EventSource` cannot set request headers, so the SSE stream uses a **one-time
ticket** instead of a long-lived credential:

1. The client calls `POST /api/auth/sse-ticket` with its cookie (or an API key).
2. The server returns `{ticket, expiresAt}`; the ticket is valid for **60 s** and
   for **one connection only**.
3. The client opens `GET /api/events?ticket=…`; the server consumes (burns) the
   ticket on use. Reuse or expiry returns `401`.

A ticket in an access log is therefore a short-lived, already-consumed artifact,
not a reusable session credential. The browser never puts the cookie value or an
API key in a URL, and no query-string credential other than the ticket is accepted.
Mitigate residual risk with a strict CSP, a short `-session-ttl`, and logout.

> **Plain-HTTP dev:** `Secure` cookies are not stored over `http://`. Start the
> server with `-cookie-secure=false` when serving plain HTTP on a LAN address, or
> cookie login appears to succeed while every `/api` call returns `401`.

## Logout and revocation

`POST /api/auth/logout` is public and always clears the `wacalls_session` cookie.
It revokes the presented session when a credential reaches the server — in the
browser, the session cookie itself. Logout therefore works without a bearer token.
If the cookie was never stored (for example `Secure` over plain HTTP without
`-cookie-secure=false`), logout can only clear a cookie the browser never had and
the session stays valid until `-session-ttl` expires; use `-cookie-secure=false`
for plain-HTTP dev or terminate TLS in front of the server.

## Media-layer controls

| Control | Default | Notes |
|---|---|---|
| SRTP HMAC verification on receive | **On** (`-srtp-auth=true`) | Invalid tags drop the packet (`internal/voip/media/srtp.go`). `false` = legacy no-verify — interop escape hatch only |
| Hop-by-hop SRTP keys | Derived per call from WhatsApp signaling | Standard WhatsApp call key flow (`internal/voip/signaling`, `media` key derivation) |
| Browser ↔ server leg | WebRTC data channel carrying raw PCM | Protected by DTLS-SRP of the WebRTC session (pion ↔ browser), but **unencrypted beyond that inside the server process** — another reason the HTTP port stays LAN-only |

End-to-end confidentiality against WhatsApp-relay operators is the platform's
property, not something this server adds.

## Webhook egress

Webhook delivery is **blind by design**: the response body/headers are never
echoed back to any API surface (the worker only logs status codes). An attacker
who holds a valid credential can register a URL and cause the server to send
signed JSON POSTs to internal hosts, loopback services, or cloud metadata
endpoints — but cannot read the responses through this channel.

**Defaults:** by default any http/https host is allowed (receivers are
typically on the same LAN or localhost, matching this repo's trust model).
For hardened deployments, start the server with:

```
-webhook-allow-hosts chatwoot.local,internal-n8n.lan
```

This restricts both **registration** (create rejects foreign hosts with `400`)
and **delivery** (worker re-validates and skips). Hostnames are matched
case-insensitively; any port is allowed. IPv6 bracket notation (`[::1]`) is
normalized when parsing the flag.

**Residual risk:** with an empty allowlist, SSRF remains possible (an admin can
register `http://169.254.169.254/...` — webhook routes are `requireRole("admin")`,
so the exposure requires an admin account, not just any valid credential). DNS rebinding of an
allowlisted hostname (resolving to an internal IP at delivery time) is out of
scope — the worker does not resolve hosts, it only string-compares the
hostname.

**Never point webhooks at cloud metadata IPs** (`169.254.169.254`,
`fd00:ec2::254`) or untrusted internal services.

## Data handling

- **`wacalls.db`**: holds linked-account state and the auth tables (sessions/keys are
  hashed). Gitignored (`*.db*`); never commit, never ship in artifacts, restrict
  filesystem permissions on the host.
- **Bootstrap password**: supplied only through `WACALLS_ADMIN_PASSWORD` and never
  written to disk by the server. Keep it out of shell history, process listings, and
  committed `.env` files; bcrypt accepts at most 72 bytes.
- **Audit log** (SQLite auth tables): records logins, failures, logouts, and API-key
  changes. Rows are retained for **30 days**; a background sweep runs every **6 h**
  (and once at startup). Failed-login audit rows are additionally capped per client
  IP to prevent the audit table from becoming a write amplifier.
- **Diagnostic ring** (`-debug-log`): in-memory only (not persisted), exposed over
  HTTP to the `admin` role — still sensitive.
- **Logs** (`-debug`, zerolog/slog): may contain JIDs and call metadata; treat logs as
  sensitive.

## Hardening checklist (before any non-LAN use)

1. Do **not** publish the `-addr` port to the internet. Bind to localhost or a private
   interface (`-addr 127.0.0.1:8080`).
2. Set `WACALLS_ADMIN_PASSWORD` before the first boot — with an empty users table and
   the variable unset, startup aborts. There is no password-reset endpoint yet — treat
   the bootstrap credential as permanent.
3. Keep `-cookie-secure=true` (default) and terminate TLS in front of the server.
   Enable `-trust-proxy` only when a trusted proxy sets `X-Forwarded-For` /
   `X-Forwarded-Proto`.
4. Set `-allowed-origins` to the exact SPA origin(s). Leave the `*` default only on an
   isolated LAN.
5. If remote access is required, put the server **behind an authenticating reverse
   proxy** (TLS + strong auth at the proxy). The app's own accounts are password-only
   and have no MFA — defense in depth still matters.
6. Keep `-srtp-auth=true` (default). Only disable for a time-boxed legacy interop test.
7. Consider `-debug-log=false` in shared environments to shrink telemetry exposure.
8. Protect/backup `wacalls.db` as a secret; a leak = account takeover.
9. Set `-webhook-allow-hosts` to the exact receiver hostname(s) if webhooks are used — otherwise an admin who registers a webhook can point it at internal/metadata targets.
10. Re-check this doc against `auth.go`, `authapi.go`, `httpapi.go`, and `srtp.go` when
    touching route auth or SRTP — the plan file (`implementacao.md` §1) tracks the SRTP
    hardening history.

## What this doc does not claim

- No compliance certifications (LGPD/GDPR mapping is out of scope here).
- No guarantee against a malicious linked device inside WhatsApp's own trust model.
- No sandboxing of the server process itself — run it as an unprivileged user.
