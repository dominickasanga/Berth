<p align="center"><img src="logo.svg" alt="Berth logo" width="96" height="96"></p>

# Berth

> **Status:** milestones 1 and 2 built (login with 2FA, app registry, env/secrets,
> git deploy via compose, pre-deploy step, start/stop/restart, status, live
> container logs, rollback). Not built yet: metrics and alerts, the managed
> services (Postgres/Mongo/Redis/Docker), Kite360-specific actions. Not yet run
> against the real Kite360 repos or a real server. This file is the source
> of truth for the next developer or AI agent picking this up. Read it fully before
> writing code.
> Feasibility study (full reasoning, risk table, survey of every app in `/var/www`):
> https://claude.ai/artifact/R2p1iKSzwNvipxbPa7e3mT

## 1. What this is

Berth is an **internal** web tool for running and managing the apps on our servers
as Docker containers, from one place: deploy, start/stop/restart, logs, health,
env/secrets, routing, rollback. It also manages the services those apps use
(PostgreSQL, MongoDB, Redis) and the Docker engine itself.

It is not a product for customers and not a general PaaS. It exists because the
servers hold ~40 apps under `/var/www`, each deployed a different way (systemd
units, hand-run `docker compose`, nginx, Traefik), with config and routing living
only on the servers.

**Stack (decided): a Go monolith.** One binary serves the web UI, the API and
the background work (builds, deploys, health polling). No separate front-end app.

```
 Browser (admin) ──▶ Berth (one Go binary: UI + API + workers)
                         │
                         ├─▶ Docker Engine API (/var/run/docker.sock)
                         │        └─▶ app containers (on traefik-net)
                         └─▶ git clone / docker build per app
```

## 2. Scope

### v1: exactly two apps (the pilot)

| App | Repo | Path in repo | Runtime | Public URL |
|---|---|---|---|---|
| Kite360 web | github.com/dominickasanga/kite360 | `app/tams-nuxt` | Nuxt 4, Node 24, port 3024 | kite360.live |
| Kite360 API | github.com/dominickasanga/Kite360-API | `go-api` | Go 1.25 + LibreOffice + pg_dump, port 8001 | proworkwave.com/api2/ |

Both already have a `Dockerfile` and `docker-compose.yml` in those paths (the
API's were added 22 Nov 2026 and **have not yet been built or run anywhere**; first
job is to confirm `docker compose build` works on the server).

### Later (do not build for these yet, but don't design them out)

Other `/var/www` apps: SB-API, NyasaHub, GopherPost, TAMS, finance_api, etc.
The study groups all 44 by Docker readiness.

### Out of scope

Multi-server orchestration, Kubernetes, autoscaling, building a container
registry, running a database cluster or replication.

## 3. What v1 must do

1. Register an app: git repo URL, branch, subfolder, Dockerfile/compose path.
2. Store per-app env vars and secrets; inject them at start; never show a secret
   again after it is saved; never write them into the git checkout.
3. Deploy on demand: pull → build → run pre-deploy step → start → wait for health.
4. Pre-deploy hook. For the Kite360 API this is the `migrate` service (shared
   schema, then every tenant). If it fails, the old container keeps running.
5. Volumes that survive every redeploy. Berth must **never** delete a volume or
   bind-mounted folder.
6. Start / stop / restart, live logs, status from the container health check
   (`/healthz` on the API).
7. Roll back to the previous image (keep the last N image tags per app).
8. Login for admins (with 2FA). Berth is root on the server through the Docker
   socket, so it must never be reachable without auth.
9. **One instance per app, always.** No replicas, no scaling. A deploy stops the
   old container, then starts the new one. Berth refuses to run a second copy.
10. Deploys are driven through `docker compose`, reusing each repo's own compose
    file (decided).

### Kite360-specific actions (the reason to build instead of using Portainer)

- Run tenant migrations for all tenants or one schema; show `-migrate=status`.
- Trigger and list database backups (`-job=` on the API binary).
- Run one-off jobs, e.g. `sse -job=backfill_previews`.
- Show which container owns the scheduler (`SCHEDULER_ENABLED=true`).

### Services Berth also manages

Besides the apps, Berth manages the shared services they depend on. Each one may
run on the host (today's Kite360 Postgres) or in a container (NyasaHub's
compose starts its own Postgres, Redis and Mongo); Berth must handle both.

| Service | What Berth does |
|---|---|
| **PostgreSQL** | Status and version; list databases, schemas (Kite360 tenants), sizes, active connections; create a database and user for an app; `pg_dump` backups on demand and on a schedule, list and restore them; show long-running queries |
| **MongoDB** | Status and version; list databases and collections with sizes; create a database user; `mongodump` backups, list and restore |
| **Redis** | Status, version, memory use, key count per DB, connected clients (`INFO`); flush a DB |
| **Docker itself** | Engine version and info; disk use (`docker system df`); list and remove images, volumes and networks; prune unused build cache and dangling images; live engine events |

Rules for these:

- **Read-only by default.** Anything that deletes or overwrites (drop, restore,
  flush, remove volume, prune) needs an explicit confirmation that names the
  target, and is written to an audit log (who, what, when).
- **Never touch a volume an app is using**, even during a prune.
- **Restore into a new database first**, then switch, rather than over the live one.
- Admin credentials for each service are stored like app secrets (section 3,
  point 2), never in the repo.
- Backups go to a host folder Berth is configured with, and are listed with
  size and age so a missing nightly backup is visible.

### Metrics and alerts

Performance metrics for every app and for the services they use. Berth only
reads; each service gets a read-only role (section 3, secrets rules apply).
Metrics history is stored in Berth's own PostgreSQL database: about 7 days at
full resolution, then downsampled. Polled every 15 to 30 seconds.

| Source | Metrics |
|---|---|
| **App containers** (Docker stats API) | CPU %, memory vs limit, network in/out, block I/O, restart count, uptime, health-check status over time, last deploy duration |
| **App `/metrics`** (optional) | Request rate, error rate, latency, scraped only if the app exposes it |
| **PostgreSQL** (`pg_stat_*`) | Connections vs `max_connections`, database and tenant-schema sizes, long-running queries, cache hit ratio, transaction rate, locks and deadlocks, last backup age |
| **Redis** (`INFO`) | Memory used, hit/miss ratio, evictions, connected clients, ops/sec, keys per DB |
| **MongoDB** (`serverStatus`) | Connections, opcounters, memory, database and collection sizes, slow operations |

- Graphs for 1h, 24h and 7d, plus a current-status card per app and per service.
- **Email alerts** on: failed health check, high CPU or memory, connections near
  the maximum, missing backup. Thresholds are set per app/service.
- Slack and Prometheus/Grafana export are not in v1.

## 4. Hard rules from the existing setup

Learned from reading both Kite360 repos. Breaking any of these causes real damage.

- **One scheduler only.** Exactly one API container may run with
  `SCHEDULER_ENABLED=true`. Deploys are stop-then-start for the API, never two
  copies side by side, or every reminder/report email is sent twice.
- **Media and backups live on the host** at `BASE_PATH/media` and
  `DBBACKUP_STORAGE_OPTIONS`, mounted at the same path inside the container.
- **Stable secrets.** `FIELD_ENCRYPTION_KEY` (user emails/phones are encrypted
  with it; lose it and that data is unreadable), `SECRET_KEY`,
  `FILE_LINK_SIGNING_KEY`. Berth stores them once and reuses them.
- **Host services** (Postgres, RabbitMQ :5672, gopherpost :8787) are reached from
  containers as `host.docker.internal` (`extra_hosts: host-gateway`).
- **Routing** is Traefik on the external Docker network `traefik-net`,
  entrypoint `websecure`, by container labels (see pgadmin4's compose for the
  house pattern). SSE routes must not be buffered at any hop.
- **Cloudflare** sits in front; origin should only accept Cloudflare IPs.
- Source of truth for the API's behaviour: `Kite360-API/go-api/PORT_STATUS.md`
  ("Production topology") and `Kite360-API/README.md` ("Running it in Docker").

## 5. Known issues in the wider estate (for later phases)

- Real `.env` files are committed in MEMIS, SB-Metrics, SB-Web-APP,
  TAMS_Container. Berth should be where those values live instead.
- Port clashes on the host (3000, 3308, 4000, 5173). Berth should route through
  Traefik on internal networks and avoid publishing host ports.
- SB-API's Docker files exist but are empty.
- Gotenberg README says the Kite360 API converts through Gotenberg; the code on
  `main` still calls a local `soffice`.

## 6. Open decisions (ask Dominic before choosing)

- **UI rendering** inside the monolith: Go `html/template` (+ htmx) or a small
  front end embedded with `go:embed`. Not yet chosen.
- ~~**How Berth deploys**~~ Decided: drive `docker compose`.
- ~~**Build vs reuse**~~ Decided: build from scratch (Coolify, Dokploy, Komodo
  and Portainer were surveyed; none has the Kite360-specific actions).
- ~~**Where Berth's own state lives**~~ Decided: PostgreSQL, for both app state
  and metrics history. Use a dedicated database and role for Berth, separate from
  the app databases it manages, so Berth can still start and show status when an
  app database is down.
- **Which server** hosts the pilot, and the real Traefik labels currently used
  for the Kite360 API (they are only on the server, not in any repo).
- Production Postgres major version (sets `PG_MAJOR` for `pg_dump`; default 17).

## 7. Suggested first steps

1. On the server: `cd Kite360-API/go-api && docker compose build` and fix
   anything that breaks. Same for `kite360/app/tams-nuxt`.
2. Bring both up by hand with compose on a staging host with copies of the DB
   and media. Confirm previews, backups, email and SSE work.
3. Only then start Berth's code: app registry + deploy + logs for these two.

## 8. Running Berth (milestone 1)

Code layout: `cmd/berth` (entry point), `internal/store` (Postgres + migrations),
`internal/crypt` (AES-GCM for secrets), `internal/deploy` (git + compose engine),
`internal/web` (UI: `html/template` + htmx, all embedded in the binary).

```sh
createdb berth                                  # dedicated database for Berth
export BERTH_DATABASE_URL='postgres:///berth?host=/tmp'
export BERTH_SECRET_KEY=$(openssl rand -base64 32)   # keep it: losing it makes stored secrets unreadable
go build -o berth ./cmd/berth
echo 'a-password-of-12+-chars' | ./berth admin-create dominic   # prints the 2FA URL for your authenticator app
./berth serve                                   # http://127.0.0.1:8090
```

| Variable | Default | Meaning |
|---|---|---|
| `BERTH_DATABASE_URL` | required | Berth's own Postgres database |
| `BERTH_SECRET_KEY` | required | base64 of 32 bytes; encrypts secrets and 2FA seeds |
| `BERTH_DATA_DIR` | `./data` | holds `checkouts/` and `env/` (env files are mode 0600, never inside a checkout) |
| `BERTH_ADDR` | `127.0.0.1:8090` | listen address; put Traefik/TLS in front, never expose it plain |
| `BERTH_COOKIE_SECURE` | `true` | set `false` only for local HTTP testing |
| `BERTH_KEEP_IMAGES` | `5` | rollback image tags kept per image |
| `BERTH_COMPOSE_CMD` | `docker compose` | use `docker-compose` if only the standalone v2 binary is installed |

How a deploy treats the app's compose file:

- Berth runs it as project `berth-<app>` with `--env-file` pointing at the stored
  variables. A variable reaches a container only if the compose file references it
  (`${NAME}` or `environment:`), as in any compose setup.
- The pre-deploy service (for example `migrate`) must be declared with
  `profiles: ["tools"]` so `compose up` does not also start it. Berth runs it with
  `run --rm --no-deps`; if it exits non-zero the deploy stops before `up`, and the
  old container keeps running.
- Berth never runs `compose down` or `-v`; volumes survive deploys.
- **Rollback:** after each build Berth keeps every image as `<repo>:berth-<deploy id>`
  and prunes only those tags beyond `BERTH_KEEP_IMAGES`. "Roll back to this" on an
  older successful deploy checks out its commit, re-tags its images and runs
  `up --no-build`. If an image was pruned the button is hidden, and a failed
  rollback changes nothing. The pre-deploy step is **not** run on rollback, so
  database migrations are not undone.
- **Logs:** "Show live logs" streams `compose logs -f` (last 200 lines, all
  services) over server-sent events. A reverse proxy must not buffer it.

Tests: `go test ./...`. The web test needs a scratch database:
`createdb berth_test; BERTH_TEST_DATABASE_URL='postgres:///berth_test?host=/tmp' go test ./...`
(it truncates that database's tables).

Decided not to build: per-app deploy keys. Berth runs `git` as the user it runs as,
so a private repo works only if that user already has access (SSH key, credential
helper, or a token in the repo URL). Berth stores no git credentials.

Known gaps in milestone 1: TOTP codes can be replayed within their 30-second
window, login throttling is in memory, and all admins have equal rights.
