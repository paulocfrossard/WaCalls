# Development

Contributor workflow: commands, local gates, layout, and conventions. Day-one setup is
in [getting-started](./getting-started.md); module responsibilities are in
[architecture](./architecture.md).

## Canonical commands

There is **no Makefile** — raw `go` / `npm` commands are the contract (and what the
container build runs).

### Server (repo root)

```bash
go vet ./...                          # static checks
gofmt -l .                            # must print nothing (gate must pass before commit)
go build ./...
go test -race -count=1 ./...          # full suite (matches the container build gate)
go test -race -count=1 -run '^TestName$' ./cmd/server/   # single test
```

Notes:
- Tests are co-located `*_test.go` in the same package (server tests are in
  `package main` under `cmd/server/`).
- The container build compiles with `CGO_ENABLED=0` (no cgo needed — MLow is vendored
  pure Go). Run the race suite locally with `CGO_ENABLED=1`.

### Client (`cd client` first — all npm scripts live there)

```bash
npm ci                # lockfile install — the only supported path (never npm install)
npm run build         # tsc -b (type-check gate) && vite build → dist/
npm run dev           # Vite :5173, /api proxy → http://localhost:3001
npm run preview       # serve dist/ WITHOUT /api proxy (see caveats)
```

`client/package-lock.json` is committed, so `npm ci` is the only supported install
path — do not use `npm install`.

Gaps to know (documented so nobody hunts for missing scripts):
- **No `lint` or `test` script** exists in `client/package.json` — the de-facto quality
  gate is `tsc -b` (inside `build`).
- `package.json` declares **no `engines`** field; the Dockerfiles pin Node **22**.

## Local definition of done

The workflows that used to live in `.github/workflows/` are no longer part of this
repository. These are the gates:

```bash
gofmt -l cmd internal && go vet ./... && go build ./... && go test -count=1 ./...   # Go 1.27.3 toolchain
cd client && npm ci && npm run build
cd sdk && npm ci && npm run build
```

Container parity: both Dockerfiles run the same `npm ci` + `tsc -b` + `vite build` and
`go build` steps, so a local pass is a good proxy for the image build.

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
  `X-Client-Id`); SSE goes through the SDK's EventSource (`sdk/src/index.ts`),
  with the typed `BrokerEvent` union in `client/src/types/broker.ts`.
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
