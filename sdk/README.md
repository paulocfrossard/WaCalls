# wacalls-sdk

Vanilla TypeScript SDK for WaCalls — embeddable in any host page (Chatwoot agent
screens, plain HTML, or a framework app) with **zero runtime dependencies**: it
uses only Web APIs (`EventSource`, `fetch`, `RTCPeerConnection`,
`AudioContext`/`AudioWorklet`, `<audio>`). The client owns the SSE stream, the
REST call actions, the WebRTC/media lifecycle and the remote audio playback, so
the host app only wires events and buttons.

## Install / build

```sh
npm ci             # lockfile install — do not use npm install
npm run build      # tsup → dist/
npm run typecheck  # tsc --noEmit
```

| Bundle | Path | Module type |
|---|---|---|
| ESM | `dist/index.js` | `import { WaCallsClient } from "wacalls-sdk"` |
| IIFE | `dist/index.global.js` | `<script>` → global `WaCallsSDK` |
| Types | `dist/index.d.ts` | TypeScript declarations |

## Quickstart (script tag)

```html
<script src="./sdk/dist/index.global.js"></script>
<script>
  const client = new WaCallsSDK.WaCallsClient({
    baseUrl: "http://localhost:8080",
    clientId: "chatwoot-agent-1",
  });

  // SUBSCRIBE FIRST: every SSE frame lands on broker:event,
  // typed frames also fan out to call:incoming / call:status / call:ended.
  client.on("broker:event", (ev) => console.log("sse", ev));
  client.on("call:incoming", (p) => console.log("ringing", p));

  // RUN INSIDE A CLICK: startCall/answerCall resume the AudioContext
  // during the user gesture (browser autoplay policy).
  document.getElementById("call").onclick = async () => {
    client.connect();                                   // open SSE (idempotent)
    await client.startCall(sessionId, "5511999999999"); // media opens here
  };
</script>
```

A Chatwoot PoC example page once lived here but was removed from the repo along
with the `examples/` directory; the snippet above is the reference usage.

## Configuration — `new WaCallsClient(config)`

| Option | Type | Required | Default | Notes |
|---|---|---|---|---|
| `baseUrl` | `string` | yes | — | API origin (e.g. `http://localhost:8080`); `""` = same-origin |
| `clientId` | `string` | yes | — | Stable operator id — sent as `X-Client-Id` (REST) and `?clientId=` (SSE) |
| `token` | `string?` | no | — | Session token — REST `Authorization: Bearer <token>` **only**; wins over `apiKey` |
| `ticketProvider` | `() => Promise<string>` | no | — | Awaited before **every** SSE (re)connect; the result is sent as `?ticket=` (see [Authentication](#authentication)) |
| `apiKey` | `string?` | no | — | Legacy API key — REST `Authorization: Bearer <apiKey>` **only**; never on the SSE URL |
| `micId` | `string \| null?` | no | system default | Exact microphone `deviceId` for calls opened by this client |
| `outId` | `string \| null?` | no | system default | Output `deviceId` (`setSinkId`) for the internal downlink `<audio>` |

## Events — `client.on(event, cb)` / `client.off(event, cb)`

| Event | Payload | Emitted when |
|---|---|---|
| `call:incoming` | `{ callId, peer, sessionId, offeredAt? }` | inbound call offer (`type: "incoming"` frame) |
| `call:status` | `{ status, callId, sessionId, peer, owner, startedAt? }` | status transition (`starting`/`ringing`/`connected`/`ended`) — also fired once per row of a `call-list` snapshot; frames whose status is outside that enum are **not** fanned out here (use `broker:event`) |
| `call:ended` | `{ reason, callId, sessionId, endedAt? }` | call teardown; the SDK also releases local media for the active call |
| `error` | `{ message }` | SDK-level failure (JSON parse, media/playback, reconnect) |
| `broker:event` | `Record<string, unknown>` (raw frame with a `type` field) | **every** parsed SSE data frame, before the typed fan-out — includes frames with no typed event (`session-list`, `session-qr`, `auth-state`, `call-list`, `incoming-claimed`, …) |

`offeredAt` / `startedAt` / `endedAt` are epoch-ms broker timestamps, passed
through only when the raw frame carries them.

## Methods

| Method | Signature | Notes |
|---|---|---|
| `connect()` | `() => void` | Opens the single SSE stream (idempotent); reconnects with backoff until `disconnect()` |
| `disconnect()` | `() => void` | Closes SSE, cancels reconnects, best-effort server-side hangup + local media release |
| `setToken` | `(token: string \| null) => void` | Replaces the REST bearer; a string also reopens the live SSE stream. `null` clears the bearer, stops reconnects and **closes** the stream — the next `connect()` (or a new `setToken`) reopens it. An active call is never torn down |
| `startCall` | `(sessionId, phone, opts?: { durationMs?, record? }) => Promise<void>` | `POST .../calls` then opens media (user-gesture context) — **`durationMs`/`record` are reserved: currently ignored by the server** |
| `answerCall` | `(sessionId, callId) => Promise<void>` | accept + open media (uses the server-returned `callId` when present) |
| `rejectCall` | `(sessionId, callId) => Promise<void>` | no media involved |
| `hangup` | `(sessionId, callId) => Promise<void>` | end call + close local media (teardown runs even if the DELETE fails) |
| `toggleMute` | `(isMuted: boolean) => void` | enables/disables the mic track and stores the desired state for future calls |
| `setMicId` / `setOutId` | `(deviceId: string \| null) => void` | `setOutId` also re-routes the active downlink immediately; `null` resets to the default output |
| `activeCallId` | getter → `string \| null` | callId of the locally-open call |
| `isMuted` | getter → `boolean` | desired mute state (survives call changes) |
| `activeOpenCall` | getter → `OpenCall \| null` | raw peer connection/stream (advanced) |
| `activeAudio` | getter → `HTMLAudioElement \| null` | internal downlink element (advanced) |

Low-level building blocks (`openCall`, `Emitter`, PCM helpers, downlink monitor)
are re-exported for advanced embedders — see `dist/index.d.ts`.

## Authentication

REST and SSE authenticate differently, because `EventSource` cannot send headers.

**REST (`fetch`)** — every call sends `X-Client-Id`, plus
`Authorization: Bearer <token | apiKey>` when either is configured (`token` wins),
and `credentials: "include"`, so the HttpOnly session cookie from a browser login
authenticates the request even with no bearer set. `apiKey` is a **REST-only**
credential; it is never used for the event stream.

**SSE (`EventSource`)** — the URL carries **no** long-lived credential in its query
string: neither the API key nor the session token is appended. The only query
credential the server accepts is a short-lived, single-use `?ticket=`, supplied in
one of two ways:

- **`ticketProvider`** (recommended): `ticketProvider?: () => Promise<string>` is
  awaited before **every** SSE (re)connect and its result is appended as
  `?ticket=<ticket>`. Mint a fresh single-use ticket there — tickets are consumed
  on use and expire quickly, so caching one breaks reconnects. If the provider
  throws or returns an empty/blank string, the SDK opens the stream cookie-only.
- **Cookie-only**: with no `ticketProvider`, the stream authenticates purely via
  the ambient session cookie (same `credentials: "include"` semantics as REST).

```ts
const client = new WaCallsSDK.WaCallsClient({
  baseUrl: "https://wa.example.com",
  clientId: "chatwoot-agent-1",
  // Called on every (re)connect; never send a long-lived key in a URL.
  ticketProvider: async () => {
    const res = await fetch("/api/auth/sse-ticket", {
      method: "POST",
      credentials: "include",
    });
    return (await res.json()).ticket; // → ?ticket=<ticket>
  },
});
```

Rotate the REST credential with `client.setToken(token)`; `setToken(null)` clears
it, stops reconnects and closes the stream (the next `connect()` reopens it).
Details: [`docs/api.md`](../docs/api.md#authentication),
[`docs/security.md`](../docs/security.md).

## Notes

- **Zero runtime dependencies** — nothing to install downstream; `dependencies` in
  `package.json` is empty by design.
- **Audio worklets are embedded** as source strings and registered via a
  `Blob` URL — no worklet files to host on your server.
- **One `EventSource`** per client: `{baseUrl}/api/events?clientId=…[&ticket=…]`,
  `data:` frames only, `: ping` comments ignored natively, exponential-backoff
  reconnect (1s → 30s cap) until `disconnect()`.
- **AudioContext requires a user gesture**: always call `startCall` /
  `answerCall` from a click/keypress handler (the SDK resumes the context
  internally during that gesture).
- **Auth**: see [Authentication](#authentication). REST uses
  `Authorization: Bearer <token | apiKey>` when configured **and** always
  `credentials: "include"`; SSE uses only `?ticket=` from `ticketProvider`, or
  the session cookie when no provider is set.
- **Security (read this before shipping a key)**: no API key or session token is
  ever placed in a URL, so access logs and `Referer` headers only ever see a
  short-lived ticket (already consumed or expired). However, in the
  Chatwoot/Vite build `VITE_WACALLS_API_KEY` is **inlined into the public
  browser bundle** — anyone can read it. Prefer cookie login plus
  `ticketProvider`, treat any bundled key as a scoped, rotatable UI credential
  (never a secret with backend power), and rotate it when in doubt.
- **`StartCallOptions` are reserved**: the server currently **ignores**
  `durationMs` and `record` (it decodes them and drops them — no call timeout,
  no recording). Do not promise those features until the server implements them.
- **Structured errors**: any non-2xx REST response rejects with an exported
  `SdkRequestError` carrying `method` / `path` / `status` / `body` (its
  `message` keeps the `"METHOD path status body"` text for log matching).
