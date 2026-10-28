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
cd client && npm ci && cd ..
```

## Run the server

```bash
WACALLS_ADMIN_PASSWORD='choose-a-strong-one' go run ./cmd/server -addr :8080
# add -debug for verbose logging
```

Open `http://localhost:8080`, click **New session**, and scan the QR that appears
(browser and terminal) with **WhatsApp → Linked devices**. Add more accounts the
same way and switch between them in the sidebar. Sessions persist in `wacalls.db`
(SQLite, created next to the working directory). Restored sessions that were never
paired come back as **Disconnected** with a **Reactivate** button; the QR appears
only after clicking **Reactivate**.

### First login

Every `/api` route requires authentication (login and logout excepted), so sign in
before pairing. On first boot, when the users table is empty, the server creates an
admin account named by `-admin-user` (default `admin`).

`WACALLS_ADMIN_PASSWORD` is **required** for that first boot. It is an environment
variable — there is no flag for it. With an empty users table and the variable
unset, startup aborts with:

```
Banco de dados vazio. Forneça a variável de ambiente WACALLS_ADMIN_PASSWORD com a senha do admin inicial.
```

```bash
WACALLS_ADMIN_PASSWORD=changeme go run ./cmd/server -addr :3000
```

The password is bcrypt-hashed with cost 12; bcrypt accepts at most 72 bytes. Copy
`.env.example` from the repo root if you prefer to load the variable from a file.

Open `http://localhost:3000/login` and sign in. Login is **cookie-based**: the
server sets the `HttpOnly` `wacalls_session` cookie and returns only the expiry and
user — the UI never receives or stores a token. The session lasts `-session-ttl`
(default `24h`); `POST /api/auth/logout` revokes it. API clients and scripts use API
keys created by an admin at `POST /api/auth/keys` — see
[api](./api.md#authentication). The SSE stream authenticates with a short-lived
one-time ticket obtained from `POST /api/auth/sse-ticket`.

> Serving plain HTTP on a LAN address? Add `-cookie-secure=false`. The default
> `-cookie-secure=true` marks the session cookie `Secure`, and browsers will not
> send a `Secure` cookie over `http://`, so login appears to succeed but every
> `/api` call returns `401`. Keep the default behind TLS.

### Container

```bash
docker build -f Dockerfile.debian -t wacalls:debian .
docker run -p 8080:8080 -e WACALLS_ADMIN_PASSWORD='choose-a-strong-one' \
  -v wacalls-data:/data wacalls:debian
```

The database is `/data/wacalls.db` inside the volume, and the process runs as uid
**10001**, so a bind-mounted host directory must be writable by that UID
(`chown 10001 <dir>` on the host). `WACALLS_ADMIN_PASSWORD` is required on the
first run against an empty volume; the container aborts without it.

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
| `-webhook-allow-hosts` | `""` | Comma-separated hostnames webhooks may target (`""` = any host) |
| `-admin-user` | `admin` | Bootstrap admin username (used only when the users table is empty) |
| `-session-ttl` | `24h` | Auth session lifetime |
| `-allowed-origins` | `""` | Comma-separated CORS origins (`""` = allow any origin) |
| `-cookie-secure` | `true` | Mark session cookies `Secure`; set `false` for plain-HTTP dev |
| `-trust-proxy` | `false` | Honor `X-Forwarded-For` / `X-Forwarded-Proto` from a front proxy |
| `-auth-disabled` | `false` | Disable auth on `/api` routes (insecure; dev only) |

The admin bootstrap password is not a flag: set `WACALLS_ADMIN_PASSWORD` in the
environment. It is required whenever the users table is empty — startup aborts if
it is missing. See `.env.example`.

## Run the client in dev mode

The Vite dev server proxies `/api` to **`http://localhost:3001`**
(`client/vite.config.ts`), not to `:8080`. Start the backend on that port:

```bash
WACALLS_ADMIN_PASSWORD=changeme go run ./cmd/server -addr :3001  # only needed while the users table is empty
```

Authentication is enabled in dev too — sign in at `/login`. The Vite proxy forwards
the session cookie; when you reach the backend over plain HTTP from another host,
add `-cookie-secure=false`.

```bash
cd client
npm run dev    # http://localhost:5173 — proxies /api → http://localhost:3001
```

Scripts available in `client/`: only `dev`, `build`, `preview` — there is **no**
`lint` or `test` script; the type-check gate lives inside `build` (`tsc -b`).

## Production build (static client served by Go)

```bash
cd client && npm run build && cd ..
WACALLS_ADMIN_PASSWORD=changeme go run ./cmd/server -static client/dist -addr :8080
```

`WACALLS_ADMIN_PASSWORD` is only read while the users table is empty; once the admin
exists the server starts without it. `.env.example` documents both variables.

`npm run preview` serves the built `dist/` **without** an `/api` proxy — API calls will
fail unless you open the app through the Go server (the `-static` path above).

## Tests

```bash
go test ./...                       # full suite (CI runs: go test -race -count=1 ./...)
go test -race -count=1 ./cmd/server/    # server package only
cd client && npm run build          # client type-check + production build
```

See [development](./development.md) for the contributor workflow and the local
definition of done.

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
| Login succeeds but every `/api` call returns `401` | Cookie is `Secure` while the page is served over plain HTTP | Add `-cookie-secure=false`, or serve over HTTPS |
| `429` on login | Too many failed attempts: over 10/min for the same IP + username, 30/min from one IP, or 300/min globally | Wait a minute, then check the credentials |
| Startup aborts with `Banco de dados vazio...` | `WACALLS_ADMIN_PASSWORD` is unset and the users table is empty | Set the variable (see `.env.example`) and start again; see [security](./security.md) |

## Next steps

- Integrating the API? → [api](./api.md)
- Deploying past localhost? → [security](./security.md) first.
- Contributing? → [architecture](./architecture.md) → [development](./development.md)
