<p align="center"><img src="logo.svg" alt="Berth logo" width="96" height="96"></p>

# Berth

> **Status:** milestones 1 and 2 built (login with 2FA, app registry, env/secrets,
> git deploy via compose, pre-deploy step, start/stop/restart, status, live
> container logs, rollback, discovery of expected apps on disk, choice of existing folder or own clone). Not built yet: metrics and alerts, the managed
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

### What production actually looks like (checked on the `erp` server, 2026-11-23)

- **Docker Compose is v1 only** (`docker-compose` 1.29.2, no `docker compose` plugin).
  Berth needs **Compose v2** (2.24 or newer): it uses `up --wait`, several `--env-file`
  flags and `--profile "*"`. Install the v2 plugin before using Berth there; v1 keeps
  working beside it for the stacks it already runs.
- **The Kite360 API (port 8001) and the NyasaHub API (port 8090) run as host processes**
  (a binary named `main`), not in Docker. They use the **host's** PostgreSQL (0.0.0.0:5432),
  Redis (127.0.0.1:6379), MongoDB (127.0.0.1:27017) and RabbitMQ (*:5672). Redis and
  Mongo listen on loopback only, so a bridge-network container cannot reach them through
  `host.docker.internal`; a container would need `network_mode: host` or those services
  would have to listen on the Docker bridge.
- **NyasaHub API's own compose file is a self-contained dev stack** (its own Postgres,
  Redis, Mongo and RabbitMQ containers on 5432, 6379, 27017, 5672, 15672). It does not
  match production and would clash with the host services above. Do not deploy it there.
- **Berth's default port 8090 is already taken in production** by the NyasaHub API, so
  Berth must listen on another port there (set `BERTH_ADDR`). The NyasaHub API keeps 8090.
- Running containers are not Berth projects: `nyasahub-web` (plain `docker run`, port 3000),
  `tams-nuxt_proworkwave_erp_1` (Kite360 web, an old compose project, port 3024), plus
  Traefik v2.10 on 80/443/8080 and Gotenberg on 127.0.0.1:3025. Moving an app under Berth
  means stopping its old container first (do not remove it, so you can go back), because
  Berth's project is `berth-<app>` and the host port would otherwise be taken.

### Decisions for the first four apps (2026-11-23)

- **Berth starts with four apps:** Kite360 (web, API) and NyasaHub (web, API).
- **PostgreSQL stays on the host for now**, and no other support service (Redis, MongoDB,
  RabbitMQ) moves into Docker yet. The apps keep using the host's services. When that
  changes, the plan is one separate "data services" compose project on a shared Docker
  network, with volumes Berth never removes, no host ports, and apps connecting by service
  name. Moving existing data from the host Postgres means a dump, a restore and a short
  downtime window. The managed-services features (status, backups, restore, create
  database/user, metrics) are **not built yet**.
- **Kite360 API** in Docker reaches the host's Postgres and RabbitMQ through
  `host.docker.internal` (they listen on all interfaces; Postgres also needs `pg_hba.conf`
  to allow the Docker bridge).
- **NyasaHub API** needs the host's Redis and Mongo, which listen on loopback only, so its
  production compose file (`deployments/docker/docker-compose.prod.yml`, in its repo) runs
  only the API with `network_mode: host`. It mounts the repo's `static/` folder so uploads
  survive redeploys. The old host process on port 8090 must be stopped first, and
  migrations (`make migrate-apply`, Atlas) are still run by hand.
- **NyasaHub web** has `docker-compose.yml` (publishes 3000; the live `nyasahub-web`
  container must be stopped first).
- Berth refuses to start on Compose v1 and says how to install v2.

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
./berth admin-create dominic   # asks for a password (min 8 chars, hidden), prints a 2FA QR code to scan
./berth serve                                   # http://127.0.0.1:8090/_berth/
```

| Variable | Default | Meaning |
|---|---|---|
| `BERTH_DATABASE_URL` | required | Berth's own Postgres database |
| `BERTH_SECRET_KEY` | required | base64 of 32 bytes; encrypts secrets and 2FA seeds |
| `BERTH_DATA_DIR` | `./data` | holds `checkouts/` and `env/` (env files are mode 0600, never inside a checkout) |
| `BERTH_ADDR` | `127.0.0.1:8090` | listen address; put Traefik/TLS in front, never expose it plain |
| `BERTH_BASE_PATH` | `/_berth` | URL prefix Berth is served under, so the UI is at `https://host/_berth/`. Set it to an empty string (`BERTH_BASE_PATH=`) to serve at the site root. Letters, digits, `-`, `_`, `.` only |
| `BERTH_COOKIE_SECURE` | `true` | set `false` only for local HTTP testing |
| `BERTH_KEEP_IMAGES` | `5` | rollback image tags kept per image |
| `BERTH_COMPOSE_CMD` | `docker compose` | use `docker-compose` if only the standalone v2 binary is installed |

How a deploy treats the app's compose file:

- Berth runs it as project `berth-<app>` with `--env-file` pointing at the stored
  variables, which feeds compose's `${NAME}` interpolation. Because many apps read a
  `.env` through `env_file:` instead (Kite360 API does), Berth also writes a generated
  override file (`BERTH_DATA_DIR/env/<app>.override.yml`) that sets every saved variable
  in **every service's environment**, including services behind a profile such as
  `migrate`. An `environment:` entry beats `env_file:`, so a variable saved in Berth
  wins over the same name in the app's `.env`, and the `.env` itself is never edited.
  The override holds only `${NAME}` references; values stay in the 0600 env file.
  Remove a variable in Berth and the app's own `.env` value applies again.
- **Containers cannot use `127.0.0.1` for host services.** A dev `.env` that points
  `DATABASE_URL`, `RABBITMQ_URL` or the email endpoints at `127.0.0.1`/`localhost`
  fails inside Docker ("connection refused"), because that address is the container.
  Either edit the `.env` to use `host.docker.internal`, or leave the `.env` as it is
  and save those variables in Berth instead. Postgres must also accept connections
  from the Docker bridge (`listen_addresses`, `pg_hba.conf`).
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
- **Start / Stop / Restart** act on containers that already exist and never build or
  pull. The status box shows the state (not deployed, running, stopped) and enables
  only the buttons that can work: Start when stopped, Stop and Restart when running.
  Nothing is built until the first Deploy. A failed action shows its full output on
  the page and in Berth's own log.
- **Logs:** "Show live logs" streams `compose logs -f` (last 200 lines, all
  services) over server-sent events. A reverse proxy must not buffer it.

Tests: `go test ./...`. The web test needs a scratch database:
`createdb berth_test; BERTH_TEST_DATABASE_URL='postgres:///berth_test?host=/tmp' go test ./...`
(it drops and recreates that database's `public` schema, so use a scratch database).

Decided not to build: per-app deploy keys. Berth runs `git` as the user it runs as,
so a private repo works only if that user already has access (SSH key, credential
helper, or a token in the repo URL). Berth stores no git credentials.

Known gaps in milestone 1: TOTP codes can be replayed within their 30-second
window, login throttling is in memory, and all admins have equal rights.

## 9. Discovering expected apps

The built-in list of apps Berth expects is `config/expected-apps.yaml` (embedded in
the binary, so edit it and rebuild). Today it holds the two pilot apps, Kite360 web
and Kite360 API, both with `monitor: true`.

The **Discover** page scans folders for them. Default scan paths are `/var/www` and
`/mnt/blockvolume`; add or remove paths on that page (stored in Postgres). Berth
looks one level deep and only reads folder names plus each folder's `.git/config`
and `.git/HEAD`; it never runs anything in them.

- **Matching** is by git remote URL (https, ssh and `git@host:path` forms compare
  equal) **and** by folder name (the entry's `name` or its `folder_names`). A folder
  whose name matches but whose remote is a different repo is shown with a warning.
- **Two copies** (for example one under `/var/www`, one under `/mnt/blockvolume`):
  the page lists both and you flag one with "Use this copy" before it can be used as
  the existing folder. The flag is stored, and asked again if the flagged folder
  disappears.
- **You choose how to run each app** when registering (no default, you must pick):
  - **Use the existing folder.** Berth runs the app from that folder. Offered only
    when the folder is a git checkout of the expected repository, on a branch. A
    deploy runs `git fetch` and `git merge --ff-only`, nothing else: it never
    resets, cleans, force-checks-out or switches branches, so local changes survive.
    If tracked files have local edits that upstream also changes, or the branch has
    diverged, the deploy fails with git's message and changes nothing. People can
    edit the `.env` next to the compose file by hand; Berth passes it to compose,
    then variables saved in Berth, which win on conflict. Rollback restores the
    kept images only and never checks out an old commit in that folder.
  - **Clone a new copy.** Berth keeps its own clone under `BERTH_DATA_DIR/checkouts`
    and may reset it on each deploy; variables are managed in Berth. An existing
    folder is only remembered as `source_path` for reference and is not touched.
- Credentials in a remote URL are stripped and never stored or shown. With "existing
  folder", the repo URL and branch come from the folder itself.
- git refuses folders owned by another user ("dubious ownership"). If Berth runs as
  a different user than the folder's owner, run `git config --global --add
  safe.directory <folder>` as the user Berth runs as.
- **Switching an already-registered app** between the two modes: on the app's page
  open "Change how it runs" (the Apps list and the page show the current mode as a
  badge). "Existing folder" is offered only when a matching git folder is found
  (for apps in `expected-apps.yaml`, by scanning; for apps added by hand, only if a
  folder was recorded). The change takes effect on the next deploy, is refused while
  a deploy is running, and never deletes the old clone or touches the folder. After
  switching to a clone, Start/Stop/Logs may not work until you Deploy once.
- **monitor** is recorded on the app and shown on its page. It does nothing until
  the metrics milestone is built.

## 10. Base URL (`/_berth`)

Berth serves everything under `BERTH_BASE_PATH` (default `/_berth`): pages, API,
static files, redirects and the session cookie (limited to that path). Open
`http://127.0.0.1:8090/_berth/`. The bare site root `/` redirects there; other paths
outside the prefix return 404.

Behind a reverse proxy, **pass the prefix through unchanged** (do not strip it), for
example with Traefik:

```yaml
labels:
  - traefik.http.routers.berth.rule=Host(`example.com`) && PathPrefix(`/_berth`)
  - traefik.http.routers.berth.entrypoints=websecure
  - traefik.http.services.berth.loadbalancer.server.port=8090
```

Live logs use server-sent events, so the proxy must not buffer responses for this path.
