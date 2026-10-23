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
| `POST/GET/DELETE /api/webhooks` | No auth (like all `/api`); lets any LAN caller register outbound webhook URLs | **Server-side request forgery (SSRF) egress**: the server will send signed POSTs to any http/https URL the caller supplies, including internal hosts (`http://10.x/...`), loopback services, and cloud metadata (`http://169.254.169.254/...`). Delivery is blind (no response body is echoed back to the API caller), so this is a side-effect/egress vector, not an exfiltration channel — but it can trigger internal actions and, if the receiver echoes data elsewhere, aid pivoting |
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

## Webhook egress

Webhook delivery is **blind by design**: the response body/headers are never
echoed back to any API surface (the worker only logs status codes). An attacker
who can reach `/api/webhooks` can register a URL and cause the server to send
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

**Residual risk:** with an empty allowlist, SSRF remains possible (any LAN
caller can register `http://169.254.169.254/...`). DNS rebinding of an
allowlisted hostname (resolving to an internal IP at delivery time) is out of
scope — the worker does not resolve hosts, it only string-compares the
hostname.

**Never point webhooks at cloud metadata IPs** (`169.254.169.254`,
`fd00:ec2::254`) or untrusted internal services.

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
6. Set `-webhook-allow-hosts` to the exact receiver hostname(s) if webhooks are used — otherwise any LAN caller can point webhooks at internal/metadata targets.
7. Re-check this doc against `srtp.go` and `httpapi.go` when touching SRTP or route
   auth — the plan file (`implementacao.md` §1) tracks the SRTP hardening history.

## What this doc does not claim

- No compliance certifications (LGPD/GDPR mapping is out of scope here).
- No guarantee against a malicious linked device inside WhatsApp's own trust model.
- No sandboxing of the server process itself — run it as an unprivileged user.
