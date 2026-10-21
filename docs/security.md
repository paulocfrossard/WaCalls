# Security

Operational threat model for wacalls. The root [README](../README.md) carries the
short warning; this module is the full posture.

## The core rule

> **The HTTP API has no authentication and must never be exposed outside a trusted
> LAN.** Anyone with HTTP reachability can create accounts, place calls, read history,
> and tail diagnostics.

This is by design today, and stated in the code itself (`cmd/server/httpapi.go`, the
`handleDebugLogs` SECURITY comment): the API relies entirely on being unreachable
outside the LAN.

## Attack surface

| Surface | Exposure | Impact if reachable by an attacker |
|---|---|---|
| All `/api/*` routes | No auth, no rate limiting, CORS `Access-Control-Allow-Origin: *` | Create/delete sessions (WhatsApp accounts), place/receive/end calls, read call history and peer numbers |
| `GET /api/events` (SSE) | No auth; needs a `clientId` (trivial to supply) | Real-time surveillance of session pairing, call lifecycle, peer metadata |
| `GET /api/debug/logs` | No auth (`-debug-log`, default **on**) | Leak of call IDs, SSRCs, audio-level telemetry, ICE/DC states |
| `GET /` static files | Only when `-static` dir exists | Low — serves the SPA bundle only |
| `wacalls.db` (SQLite) | Filesystem | **WhatsApp session credentials (secrets).** Full account takeover of linked devices |

## Identity is not authentication

- Clients send an advisory operator id: header `X-Client-Id` or query `?clientId=`
  (`client/src/lib/client-id.ts`, `lib/api.ts`).
- The server uses it only to enforce **single-operator conflicts** (HTTP 409 on call
  start/accept when another operator holds the session). It is **not verified**, not a
  token, not a session credential — treat any route as world-callable if reachable.

## Media-layer controls

| Control | Default | Notes |
|---|---|---|
| SRTP HMAC verification on receive | **On** (`-srtp-auth=true`) | Invalid tags drop the packet (`internal/voip/media/srtp.go`). `false` = legacy no-verify — interop escape hatch only |
| Hop-by-hop SRTP keys | Derived per call from WhatsApp signaling | Standard WhatsApp call key flow (`internal/voip/signaling`, `media` key derivation) |
| Browser ↔ server leg | WebRTC data channel carrying raw PCM | Protected by DTLS-SRP of the WebRTC session (pion ↔ browser), but **unencrypted beyond that inside the server process** — another reason the HTTP port stays LAN-only |

End-to-end confidentiality against WhatsApp-relay operators is the platform's
property, not something this server adds.

## Data handling

- **`wacalls.db`**: holds linked-account state. Gitignored (`*.db*`); never commit,
  never ship in artifacts, restrict filesystem permissions on the host.
- **Diagnostic ring** (`-debug-log`): in-memory only (not persisted), but exposed over
  HTTP — same LAN-only rule.
- **Logs** (`-debug`, zerolog/slog): may contain JIDs and call metadata; treat logs as
  sensitive.

## Hardening checklist (before any non-LAN use)

1. Do **not** publish the `-addr` port to the internet. Bind to localhost or a private
   interface (`-addr 127.0.0.1:8080`).
2. If remote access is required, put the server **behind an authenticating reverse
   proxy** (TLS + strong auth at the proxy; the app adds none).
3. Keep `-srtp-auth=true` (default). Only disable for a time-boxed legacy interop test.
4. Consider `-debug-log=false` in shared environments to shrink telemetry exposure.
5. Protect/backup `wacalls.db` as a secret; a leak = account takeover.
6. Re-check this doc against `srtp.go` and `httpapi.go` when touching SRTP or route
   auth — the plan file (`implementacao.md` §1) tracks the SRTP hardening history.

## What this doc does not claim

- No compliance certifications (LGPD/GDPR mapping is out of scope here).
- No guarantee against a malicious linked device inside WhatsApp's own trust model.
- No sandboxing of the server process itself — run it as an unprivileged user.
