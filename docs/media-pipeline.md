# Media pipeline

Deep dive on the audio path: browser mic to WhatsApp peer and back. Protocol steps
(stance parsing, key derivation, STUN registration) are implemented and covered by
tests under `internal/voip` (`go test ./...`).

## Contract summary

| Property | Value | Source |
|---|---|---|
| Sample rate | 16 kHz | `internal/voip/media` (RTP session, codec) |
| Frame | **960 samples = 60 ms** | `codec.go` (`mlowFrameSize`), `mlow` encoder/decoder |
| PCM format on the wire (browser ↔ server) | **Int16 LE, mono**, data channel labeled **`pcm`** | `cmd/server/bridge.go`, `sdk/src/utils/pcm.ts` |
| Codec | **MLow** (vendored, pure Go, no cgo — declared in `mlow/README.md`) | `internal/voip/media/mlow/` |
| RTP payload type | **120** (`PayloadTypeWhatsAppOpus`) | `internal/voip/core/types.go` |
| Media encryption | SRTP (HMAC tag verified on receive by default) | `internal/voip/media/srtp.go`, flag `-srtp-auth` |
| Browser ↔ server transport | WebRTC data channel (pion server-side), no audio track | `bridge.go` |
| WhatsApp ↔ peer transport | SRTP over SCTP/relay transport (STUN binding/allocate) | `internal/voip/transport/` |

The call core (`internal/voip/call`) only ever sees `[]float32` PCM — the bridge owns
the Int16 conversion and the data channel.

## Uplink (you → peer)

```
mic
 └► AudioWorklet capture processor (embedded in sdk/src/worklets/sources.ts)
     └► sdk/src/core/webrtc.ts → datachannel "pcm" (Int16 LE @16 kHz)
         └► cmd/server/bridge.go  → float32 PCM
             └► CallManager: MLow encode (960-sample frames)
                 └► RTP packetization (PT=120, 16 kHz)
                     └► SRTP Protect (+ HMAC tag)
                         └► WhatsApp relay (via internal/voip/transport + wa socket)
```

## Downlink (peer → you)

```
WhatsApp relay
 └► SRTP Unprotect — HMAC tag verified by default (-srtp-auth=true);
     invalid tag ⇒ packet dropped (RFC 3711 behavior, see implementacao.md §1)
     └► RTP depacketize → MLow decode (960-sample / 60 ms frames, float [-1,1])
         └► bridge → Int16 LE PCM over datachannel "pcm"
             └► AudioWorklet playback processor → speaker
```

Client-side helpers: `sdk/src/utils/pcm.ts` (Float32 ↔ Int16 LE); the in-call
VU meters live inline in `components/domain/call/CallCard.tsx` (AnalyserNode on
remoteStream/mic). Worklet sources are embedded in `sdk/src/worklets/sources.ts`
and installed via Blob URLs (`audioWorklet.addModule`).

## SRTP authentication status

- `-srtp-auth` defaults to **`true`**: the server verifies the SRTP HMAC tag on
  receive (`srtp.go`; kill-switch documented in `srtp_test.go`).
- `-srtp-auth=false` restores the legacy no-verify behavior — do not use outside
  controlled legacy interop.
- `implementacao.md` §1 is the spec for the HMAC work; the code ships the verify path.
  **When editing this paragraph, re-check `srtp.go` and the §1 status in that file —
  the plan file is the source of truth for pending items, not this doc.**

## Known constraints (date-stamped)

Statuses below are quoted from the living planning docs (pt-BR). Re-check them there
before acting; this module never re-decides them.

| Item | Status (as of last sync with plan files) | Source |
|---|---|---|
| Reframe 960 → 320 samples (60 → 20 ms) | **ABORTED** — vendored MLow API requires exactly 960 samples (`encoder.go`, `opusFrameSamps = 960` in `decoder.go`); a 20 ms TOC decode only truncates output. Revisit only with an explicit decision to modify the vendored codec. | `implementacao.md` §4, `ideias.md` (Rodada 2) |
| PLC on playback underrun | Planned item with verification criteria (not documented as shipped here — check the plan file + `sdk/src/core/webrtc.ts` for current behavior) | `implementacao.md` §3 |
| DTX/VAD gate on uplink | Planned item — check plan file for status | `implementacao.md` §5 |
| End-to-end instrumentation / one-way SLO | Planned item — check plan file; diagnostic ring (`-debug-log`, `GET /api/debug/logs`) is the current telemetry surface | `implementacao.md` §2 |

## Debugging the audio path

- Server side: run with `-debug-log=true` (default) and tail
  `GET /api/debug/logs` (sources such as `bridge-uplink`, `bridge-downlink`) — or use
  the client **Debug** page (`pages/DebugPage.tsx`), which polls the same endpoint.
- Client side: `sdk/src/core/webrtc.ts` emits structured debug rows (gum, dc, ice, uplink,
  downlink, plc) through `lib/debug-log.ts`.
- Reference vectors for the codec live in `internal/voip/media/mlow/testdata/`
  (upstream: `purpshell/meowcaller`, MIT — see the package
  [README](../internal/voip/media/mlow/README.md)).

## Where to go next

- HTTP/SSE surface → [api](./api.md)
- Module boundaries → [architecture](./architecture.md)
- Backlog and decisions → [`ideias.md`](../ideias.md), [`implementacao.md`](../implementacao.md)
