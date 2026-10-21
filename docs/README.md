# wacalls documentation

Technical + practical docs for **wacalls** — native WhatsApp 1:1 voice calls from the
browser, in pure Go (no cgo). The root [README](../README.md) stays the short form
(overview, 10-line quick start, API summary); this tree owns the expanded version.

> **Language:** all docs here are in English. The living planning docs stay in
> Portuguese and are linked as sources, never translated:
> [`ideias.md`](../ideias.md), [`implementacao.md`](../implementacao.md).

## Module map

| Module | Read when… | Read next |
|---|---|---|
| [getting-started](./getting-started.md) | first run, QR pairing, dev server, troubleshooting | architecture, security |
| [architecture](./architecture.md) | you need the big picture | api **or** media-pipeline, development |
| [api](./api.md) | integrating or extending HTTP/SSE | architecture (backref) |
| [media-pipeline](./media-pipeline.md) | touching the audio path or codec | ../ideias.md, ../implementacao.md |
| [development](./development.md) | contributing code or tests | architecture |
| [security](./security.md) | deploying or reviewing the threat model | read **before** exposing the port |

## Navigation

```
docs/README.md ──(index + MAP)──┐
                                │
        ┌───────────────────────┼────────────────────────┐
        ▼                       ▼                        ▼
[1] getting-started ──► [2] architecture          [6] security
        │                    │  │  │                 (cross-cutting:
        │                    │  │  └────► [3] api      read BEFORE any
        │                    │  └────────► [4] media-pipeline──┐  exposure;
        │                    └───────────► [5] development     │  also re-read
        │                                               │      │  after [4]
        └──(new user path)──────────────────────────────┼──────┘  for SRTP status
                                                        ▼
                              LIVING SOURCES (pt-BR, read-only for docs):
                              ../ideias.md · ../implementacao.md
                              ../internal/voip/media/mlow/README.md
```

### Read flows by persona

1. **User:** [getting-started] → [security] (before exposing beyond localhost) → done.
2. **New contributor:** [getting-started] → [architecture] → [development] → pick a domain: [api] or [media-pipeline].
3. **API integrator:** [architecture] → [api].
4. **Media contributor:** [architecture] → [media-pipeline] → `ideias.md` + `implementacao.md`
   (living backlog; date-stamped statuses in [media-pipeline] must be re-checked there, never re-decided here).

## Code map

| Area | Path | Documented in |
|---|---|---|
| HTTP entrypoint, routes, SSE broker, session store | `cmd/server/` | api, architecture, development |
| WhatsApp socket adapter (whatsmeow `<call>` stanzas) | `internal/wa/` | architecture |
| VoIP core: types, constants, `VoipSocket` | `internal/voip/core/` | architecture, media-pipeline |
| WhatsApp node / JID helpers | `internal/voip/wanode/` | architecture |
| MLow codec (vendored, pure Go), RTP, SRTP, PCM, keys | `internal/voip/media/` (+ `mlow/`) | media-pipeline |
| SCTP relay, STUN, subscription encoding | `internal/voip/transport/` | media-pipeline |
| `<call>` stanza build/parse, call-key crypto | `internal/voip/signaling/` | media-pipeline |
| Call lifecycle orchestration | `internal/voip/call/` | architecture, media-pipeline |
| Diagnostic ring buffer | `internal/voip/logbuf/` | api |
| React client (Vite 7, React 19) | `client/` | getting-started, architecture, development |

## Anti-duplication policy

- Root `README.md` = canonical **short** form; each module owns its expanded content.
- `internal/voip/media/mlow/README.md` is package-local upstream documentation —
  [media-pipeline](./media-pipeline.md) links it, never copies it.
- Anything asserted here must be traceable to code, CI, or the root README.
  Plan-file statuses (`ideias.md` / `implementacao.md`) are quoted **with the file as
  source of truth** — if the file changes, the doc follows.
