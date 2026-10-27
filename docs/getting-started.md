# Getting started

Practical path from clone to a first call. For the big picture see
[architecture](./architecture.md); before exposing the port beyond your machine, read
[security](./security.md).

## Requirements

- **Go 1.26+**
- **Node 22+** and **npm** (only to build/run the React client — this is what CI uses;
  `client/package.json` does not declare an `engines` field)
- No C compiler, cgo, or native libraries — the MLow codec is vendored pure Go
  (`internal/voip/media/mlow`).

## Install

```bash
git clone <repo-url> wacalls-go
cd wacalls-go
go mod download
cd client && npm install && cd ..
```

## Run the server

```bash
go run ./cmd/server -addr :8080    # add -debug for verbose logging
```

Open `http://localhost:8080`, click **New session**, and scan the QR that appears
(browser and terminal) with **WhatsApp → Linked devices**. Add more accounts the
same way and switch between them in the sidebar. Sessions persist in `wacalls.db`
(SQLite, created next to the working directory). Restored sessions that were never
paired come back as **Disconnected** with a **Reactivate** button; the QR appears
only after clicking **Reactivate**.

### Server flags (complete)

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:8080` | HTTP listen address |
| `-db` | `wacalls.db` | SQLite session database path |
| `-static` | `client/dist` | Static client directory; served at `/` only if the directory exists |
| `-debug` | `false` | Verbose logging (includes whatsmeow's internal log) |
| `-debug-log` | `true` | Emit audio diagnostic events to the ring behind `GET /api/debug/logs` |
| `-max-calls-per-session` | `8` | Max concurrent calls per session (`0` = unlimited) |
| `-srtp-auth` | `true` | Verify the SRTP HMAC tag on receive (`false` = legacy no-verify) |

## Run the client in dev mode

The Vite dev server proxies `/api` to **`http://localhost:3001`**
(`client/vite.config.ts`), not to `:8080`. Start the backend on that port:

```bash
go run ./cmd/server -addr :3001
```

```bash
cd client
npm run dev    # http://localhost:5173 — proxies /api → http://localhost:3001
```

Scripts available in `client/`: only `dev`, `build`, `preview` — there is **no**
`lint` or `test` script; the type-check gate lives inside `build` (`tsc -b`).

## Production build (static client served by Go)

```bash
cd client && npm run build && cd ..
go run ./cmd/server -static client/dist -addr :8080
```

`npm run preview` serves the built `dist/` **without** an `/api` proxy — API calls will
fail unless you open the app through the Go server (the `-static` path above).

## Tests

```bash
go test ./...                       # full suite (CI runs: go test -race -count=1 ./...)
go test -race -count=1 ./cmd/server/    # server package only
cd client && npm run build          # client type-check + production build
```

See [development](./development.md) for the contributor workflow and CI parity.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Dev UI loads but every `/api` call fails (502/ERR_CONNECTION) | Backend not listening on the proxy target | Start the server with `-addr :3001` in dev mode |
| `http://localhost:8080` shows 404 / directory listing | `-static` directory missing or not built | `cd client && npm run build`, or drop `-static` and use `npm run dev` |
| QR never appears / pairing loops | Device not linked, or session **Disconnected** | Click **Reactivate** on the session (or retry `POST /api/sessions/{sid}/pair`); use **WhatsApp → Linked devices → Link a device**; check terminal logs with `-debug` |
| `npm run preview` cannot reach the API | Preview has no `/api` proxy by design | Serve the build from the Go server (`-static client/dist`) instead |
| Audio missing but call connects | Browser mic permission, or device selection | Grant mic permission; pick devices in the UI (device hooks enumerate `getUserMedia` / `setSinkId`) |
| `429` when starting a call | Per-session call limit reached | Raise `-max-calls-per-session` (default `8`) or end active calls |
| `409` on call start/accept | Operator already busy (single-operator conflict via `X-Client-Id`) | Finish the active call, or use a distinct client id |

## Next steps

- Integrating the API? → [api](./api.md)
- Deploying past localhost? → [security](./security.md) first.
- Contributing? → [architecture](./architecture.md) → [development](./development.md)
