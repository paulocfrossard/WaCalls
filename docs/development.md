# Development

Contributor workflow: commands, CI parity, layout, and conventions. Day-one setup is
in [getting-started](./getting-started.md); module responsibilities are in
[architecture](./architecture.md).

## Canonical commands

There is **no Makefile** — raw `go` / `npm` commands are the contract (and what CI
runs).

### Server (repo root)

```bash
go vet ./...                          # static checks
gofmt -l .                            # must print nothing (CI fails otherwise)
go build ./...
go test -race -count=1 ./...          # full suite (matches CI)
go test -race -count=1 -run '^TestName$' ./cmd/server/   # single test
```

Notes:
- Tests are co-located `*_test.go` in the same package (server tests are in
  `package main` under `cmd/server/`).
- CI runs tests with `CGO_ENABLED=1` (race detector) while the build job uses
  `CGO_ENABLED=0` — the product itself needs no cgo (MLow is vendored pure Go).

### Client (`cd client` first — all npm scripts live there)

```bash
npm ci                # lockfile install (CI uses this, not npm install)
npm run build         # tsc -b (type-check gate) && vite build → dist/
npm run dev           # Vite :5173, /api proxy → http://localhost:3001
npm run preview       # serve dist/ WITHOUT /api proxy (see caveats)
```

Gaps to know (documented so nobody hunts for missing scripts):
- **No `lint` or `test` script** exists in `client/package.json` — the de-facto quality
  gate is `tsc -b` (inside `build`) plus CI.
- `package.json` declares **no `engines`** field; CI pins Node **22**.

## CI parity (`.github/workflows/`)

| Workflow | Trigger | Jobs |
|---|---|---|
| `ci.yml` | push/PR to `main` | **server:** `go mod download`+`verify` → `go vet` → `gofmt -l` gate → `go build` → `go test -race -count=1` (CGO=1). **client (Node 22):** `npm ci` → `npx tsc -b` → `npm run build` |
| `release.yml` | release path | packaging (see the workflow for the exact steps) |

Local definition of done: the `ci.yml` server+client commands pass locally.

## Repository layout (docs-relevant)

```
cmd/server/          HTTP entrypoint + all server-side glue (see architecture.md)
internal/wa/         whatsmeow adapter
internal/voip/       core, wanode, signaling, transport, media (+ mlow), call, logbuf
client/              React 19 + Vite 7 SPA (see architecture.md for the src map)
docs/                this tree
ideias.md            living brainstorm (pt-BR) — source of truth for ideas/status
implementacao.md     living implementation plan (pt-BR) — Pré-requisitos/Passos/
                     Riscos/Critério de verificação per item
.github/workflows/   ci.yml, release.yml
```

Planning docs (`ideias.md`, `implementacao.md`) follow an item template:
`Pré-requisitos → Passos → Riscos → Critério de verificação`. Mirror that discipline in
changes: every behavior change needs a verification criterion.

## Database

SQLite via `modernc.org/sqlite` (pure Go). Schema created idempotently at startup
(`cmd/server/sessionstore.go`):

```sql
CREATE TABLE IF NOT EXISTS sessions (
  id   TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  jid  TEXT
);
```

- Path: `-db` flag (default `wacalls.db`, working directory).
- `wacalls.db*` files are **gitignored** and hold WhatsApp session secrets — never
  commit them (see [security](./security.md)).
- No migration framework; schema is CREATE-IF-NOT-EXIST only.

## Client conventions

- Import alias `@/` → `client/src` (Vite `resolve.alias`).
- Server state → TanStack Query; UI/device/session-runtime state → Zustand stores.
- API access goes through `services/*` on top of `lib/api.ts` (which attaches
  `X-Client-Id`); SSE goes through `lib/event-stream.ts`.
- No client-side router — pages are component state.

## Debugging

- Server: `-debug` (verbose, incl. whatsmeow internals), `-debug-log` (diagnostic ring,
  default on) + `GET /api/debug/logs` or the client Debug page.
- Media path specifics: [media-pipeline](./media-pipeline.md).

## Docs workflow

- This tree is the expanded documentation; root `README.md` stays short and links here.
- Anything asserted in `docs/` must trace to code, CI, or the planning files.
- Status quotes from `ideias.md` / `implementacao.md` are date-stamped — when a status
  changes, update both the plan file and `media-pipeline.md`.
