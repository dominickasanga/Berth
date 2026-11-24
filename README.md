<p align="center"><img src="logo.svg" alt="Berth logo" width="96" height="96"></p>

# Berth

> **Status:** milestones 1 and 2 built (login with 2FA, app registry, env/secrets,
> git deploy via compose, pre-deploy step, start/stop/restart, status, live
> container logs, rollback, discovery of expected apps on disk, choice of existing folder or own clone, monitoring of supporting
> services with graphs and email alerts). Not built yet: metrics for the Docker engine itself, the managed-service actions
> (create database/user, backups, restore, flush, prune), Kite360-specific actions. Not yet run
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

### Server inventory (checked on `erp`, 2026-11-23)

- **Machine:** Ubuntu 22.04.5 LTS, x86_64, KVM, 14 vCPU, 15 GiB RAM (about 11 GiB available), `/` 22 GB free,
  `/mnt/blockvolume` 66 GB free. The clock is UTC and synchronized, so alert emails and the Audit page show UTC.
- **Docker** server 27.5.1 (client 29.1.3). **Compose: v1 (1.29.2) only**, so the v2 plugin still has to be installed.
- **Host services:** PostgreSQL 14.24 (cluster `14-main`), Redis 6.0.16, MongoDB 8.0.12, RabbitMQ, nginx, all
  systemd units. RabbitMQ's management port 15672 is closed. Backups will need `pg_dump` 14, not the 17 the
  plan above assumed (set `PG_MAJOR=14` when that is built).
- **Go 1.27.3 is installed**, so Berth can be built on the server (`go build -o berth ./cmd/berth`); a
  cross-compiled binary is not required.
- **The Kite360 API (8001) and the NyasaHub API (8090) are not systemd units** (no unit matches). How they are
  started is still to be found out before they are moved under Berth.
- The user `ubuntu` is in the `docker` and `sudo` groups. Port 8095 is free. GopherPost listens on `*:8787`.

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
  container, started with plain `docker run`, must be stopped and removed first).
- **Also in the expected list (already in Docker in production):** GopherPost
  (`network_mode: host`, the email service on 8787), Gotenberg (joins the external network
  `kite360`, which the Kite360 API stack creates, so deploy the API first), SB web
  (`production_vite_app` on 5173), TusK (receiver plus its own Postgres 16 in a named
  volume) and pgAdmin 4 (named volume `pgadmin_data`, external network `traefik-net`).
  Each adopts its running compose project (see section 9).
- **Not managed as apps:** Traefik (every route goes through it, so stopping it is an
  outage), MySQL (`blockvolume_mysql8_1`) and `simple-json-server`.
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
./berth serve                                   # http://127.0.0.1:8095/_berth/
```

| Variable | Default | Meaning |
|---|---|---|
| `BERTH_DATABASE_URL` | required | Berth's own Postgres database |
| `BERTH_SECRET_KEY` | required | base64 of 32 bytes; encrypts secrets and 2FA seeds |
| `BERTH_DATA_DIR` | `./data` | holds `checkouts/` and `env/` (env files are mode 0600, never inside a checkout) |
| `BERTH_ADDR` | `127.0.0.1:8095` | listen address (8090 is taken by the NyasaHub API on the server). `127.0.0.1` keeps it private (reach it over an SSH tunnel). `0.0.0.0` makes it reachable from the network: Berth can control Docker on the whole server, so only do that with HTTPS in front and a firewall allowing your own IP, and Berth logs a warning when it is exposed over plain HTTP |
| `BERTH_METRICS_INTERVAL` | `30` | seconds between checks of each monitored service (minimum 5) |
| `BERTH_METRICS_RETENTION_DAYS` | `7` | how long samples are kept |
| `BERTH_SMTP_HOST`, `BERTH_SMTP_PORT` (587), `BERTH_SMTP_USER`, `BERTH_SMTP_PASS`, `BERTH_SMTP_FROM`, `BERTH_ALERT_TO` | unset | alert email. Both host and `BERTH_ALERT_TO` (comma-separated) are needed. STARTTLS is used when offered; port 465 uses TLS. Without them alerts are shown on the Services page only |
| `BERTH_ALERT_URL`, `BERTH_ALERT_TO`, `BERTH_ALERT_BRAND`, `BERTH_ALERT_FROM_NAME` (`Berth`) | unset | alert email through GopherPost instead of SMTP: `BERTH_ALERT_URL` is its send-email endpoint (`http://127.0.0.1:8787/api55/v65/send-email`), `BERTH_ALERT_TO` the recipients. GopherPost sends non-`linmalawi.org` recipients (Gmail included) through Brevo. `BERTH_ALERT_BRAND=kite360` sends as `noreply@kite360.live` (verified in Brevo with DKIM and DMARC) with Kite360 branding; empty uses GopherPost's default sender and branding. Takes priority over the SMTP variables |
| `BERTH_AUTH_LOG` | `/var/log/auth.log` | the SSH log the Security page reads for failed logins (its rotated `.1` file is read too) |
| `BERTH_PUBLIC_URL` | unset | base URL (including the prefix) used for links in alert emails |
| `BERTH_REQUEST_LOG` | `quiet` | request log: `quiet` (default), `all` or `off`; see section 11 |
| `BERTH_LOG_FILE` | `<data dir>/logs/berth.log` | Berth's own log file; set to an empty string to turn the file off |
| `BERTH_LOG_MAX_MB` | `10` | rotate the log file at this size |
| `BERTH_LOG_KEEP` | `5` | rotated files kept (`berth.log.1` is the newest) |
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
- **Renamed repositories:** an expected app can list `repo_aliases`, other URLs of the same repository (its old name, for example). `kite360-web` lists `proworkwave_erp` and `kite360-api` lists `visionary_erp_api` (GitHub redirects each to the new name; both give the same commit), so the folder on the server whose remote still says `proworkwave_erp` is recognised. With "use the existing folder", Berth keeps using the remote that already works in that folder.
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
- **Compose project name (adopting an existing stack).** Berth normally runs an app as
  compose project `berth-<app>`. A stack that already runs under another project name
  (for example `tusk_db_1` belongs to project `tusk`, `tams-nuxt_proworkwave_erp_1` to
  `tams-nuxt`) would then get **new, empty named volumes** and clash on fixed container
  names such as `gotenberg` and `pgadmin`. Setting `compose_project` on the app (in
  `config/expected-apps.yaml`, or in the "Other apps found" form) makes Berth manage that
  existing project in place, keeping its containers and volumes. The app page shows the
  project in use. Tested: an adopted stack kept its volume's data; the default name made
  a new empty volume. The names in the YAML are inferred from container and image names, so
  verify each on the server before the first deploy:
  `docker inspect <container> --format '{{index .Config.Labels "com.docker.compose.project"}}'`
- **Other apps found.** Below the expected apps, Discover lists scanned folders (one level
  deep) that have a compose file (at most two folders down, skipping `node_modules` and
  similar) but are not in the expected list. Each has an Add form pre-filled from the
  folder: name, compose file, optional compose project, and the same must-choose
  "existing folder / clone" radios. The folder and compose file must be ones the scan finds
  right now. Folders already added are left out.
- **monitor** is recorded on the app and shown on its page. It does nothing until
  the metrics milestone is built.

## 10. Base URL (`/_berth`)

Berth serves everything under `BERTH_BASE_PATH` (default `/_berth`): pages, API,
static files, redirects and the session cookie (limited to that path). Open
`http://127.0.0.1:8095/_berth/`. The bare site root `/` redirects there; other paths
outside the prefix return 404.

Behind a reverse proxy, **pass the prefix through unchanged** (do not strip it), for
example with Traefik:

```yaml
labels:
  - traefik.http.routers.berth.rule=Host(`example.com`) && PathPrefix(`/_berth`)
  - traefik.http.routers.berth.entrypoints=websecure
  - traefik.http.services.berth.loadbalancer.server.port=8095
```

Live logs use server-sent events, so the proxy must not buffer responses for this path.

## 11. Berth's own logs and audit trail

Three things, all behind the login:

- **Colours:** log lines are coloured by what they say, in the live logs and the deploy output: errors
  (`ERROR`, `FATAL`, `level=error`, `DEPLOY FAILED`, `failed`, `unhealthy`) red, warnings amber, `DEPLOY OK`,
  `Healthy` and `Successfully` green, debug and docker build steps grey, `== section ==` headers blue and
  timestamps dimmed (`failed=0` is not an error). The colours come from the theme, so they follow the
  light/dark switch. Copying a line gives the original text.
- **Deploy output** has a **Full screen** button for each deploy (Escape or the same button leaves).
- **Logs page** (`/_berth/logs`): Berth's own log, live, with the same Side panel and Full
  screen views as the apps' logs. It shows the last 300 lines on open, then new ones as they
  are written. It is Berth's log (starts, deploys, failed logins, errors), not an app's log.
- **Audit page** (`/_berth/audit`): who did what and when (logins, app changes, deploys,
  rollbacks, variable changes, mode switches), newest first, 100 per page. Stored in
  PostgreSQL. Secret values are never recorded; variable changes show only the name.
- **Log file** (`BERTH_DATA_DIR/logs/berth.log`, mode 0640): the same lines as on stderr,
  rotated by size (`BERTH_LOG_MAX_MB`, `BERTH_LOG_KEEP`). Under systemd, stderr also goes to
  the journal, so `journalctl -u berth` works too.

**Request log.** One line per request, in the terminal, the file and the Logs page:
`http: GET /_berth/services 200 3ms user=admin ip=127.0.0.1`. Only the path is logged: never
the query string (flash messages), a form value, a password or a secret. A client-supplied
`X-Forwarded-For` is shown as `xff=` but never trusted. `quiet` (the default) leaves out
static files and the pages' own background polling (the status boxes refresh every 10 seconds)
unless they fail, so real traffic stays readable; `all` logs everything; `off` logs none.
Long-lived streams (live logs) are logged when they end.

What gets logged: startup (Compose version, log file, address), every deploy and rollback
start and finish with its result, control failures, failed and refused logins with the
client address (never the password or code), and one `audit:` line per audited action.
The Logs page keeps the last 1000 lines in memory; the file and the Audit page keep history.

## 12. Monitoring services

The **Services** page monitors the things your apps depend on. It is built and tested
against real PostgreSQL, Redis, MongoDB and RabbitMQ.

| Kind | What it checks |
|---|---|
| **PostgreSQL** | version, uptime, connections vs `max_connections` (idle and idle-in-transaction counts, and a table of who holds them grouped by application, with numbered names like pgAdmin's collapsed), transactions/s, cache hit ratio, deadlocks, waiting locks, long-running queries (text shown to `pg_monitor`/superusers), databases with sizes and connections, optional schema sizes (for the Kite360 tenants) |
| **Redis** | version, role, memory (and % of `maxmemory`), clients, ops/s, keyspace hit ratio, evictions/s, keys, `PING` latency. Uses a small built-in client, no library |
| **MongoDB** | version, uptime, connections, ops/s, resident memory, databases and sizes, ping latency |
| **RabbitMQ** | through the management API: ready/unacked messages, publish and deliver rates, connections, consumers, memory vs limit, disk free, file descriptors, resource alarms, the queues with most messages. If the management port is closed, or it refuses the login, Berth falls back to checking that AMQP answers: the service stays *up*, a note explains what is missing, and a refused login raises one warning (not a down alert) |
| **MySQL** | reachability and version, from the server greeting (no login needed) |
| **HTTP / TCP** | status code and response time / connect time, for GopherPost, Gotenberg, the APIs, Traefik and others |

- **Overview** (`/_berth/overview`, first item of the menu): where everything stands in one screen.
  *This server*: CPU, memory, the fullest disk and network in/out as tiles (amber from 75%, red from
  90%), five graphs (CPU, memory, fullest disk, network in, network out; 1 hour, 24 hours or 7 days)
  and a table of every real disk. *Apps*: each app's containers running, CPU, memory and restarts.
  *Services*: one line per group with how many are down or have alerts. Each app page also has a
  *Resources* card with its CPU and memory graphs.
  - Server numbers are read from `/proc` and the disks, so they exist only where Berth runs on Linux
    (on a Mac the page says so). The disks are found automatically (ext4, xfs, btrfs...; docker's and
    snap's are left out). CPU is the share of time not idle; memory counts what is really used
    (`MemTotal - MemAvailable`, so the file cache is not counted); network counts the real network cards,
    not loopback, `docker0`, `veth*` or `br-*`.
  - App numbers come from the `docker` CLI for the app's compose project (`docker ps`, `inspect` and
    `stats`), once per check. CPU is docker's figure, where 100% is one full core. Apps with monitoring
    off (for example pgAdmin) are not measured. The server and each app are measured as internal
    services (kinds `server` and `container`): they share the samples, the 7-day history and the alert
    emails, but are not listed on the Services page and cannot be added or edited by hand.
  - **Alerts:** a disk more than 85% full, memory in use above 90%, a container that keeps restarting
    (critical) and a container failing its health check. A stopped app does not alert. The server's
    thresholds are fixed for now. The pause control is on the service page, which an alert links to.
- **Groups:** the Services page is sorted into sections with icons and a jump bar at the top:
  Databases and caches (PostgreSQL, MySQL, MongoDB, Redis), Messaging (RabbitMQ), Apps and APIs (HTTP)
  and Ports and other (TCP). A section shows how many of its services are down or have alerts.
- **Add** a service with the form (a **Test connection** button tries it without saving). The
  page also **suggests** the services found on the production server
  (`config/expected-services.yaml`); adding one opens the form pre-filled.
- **Checks and history:** every `BERTH_METRICS_INTERVAL` seconds, stored in Berth's PostgreSQL
  for `BERTH_METRICS_RETENTION_DAYS`. Graphs (1 hour, 24 hours, 7 days) are drawn on the server
  as SVG, so pages stay small and need no JavaScript library. There is no downsampling: raw
  samples are kept for the retention period and averaged into buckets when drawn.
- **Alerts:** a service is *down* after 2 failed checks in a row (adjustable per service). Each
  kind also has thresholds you can change in its form: PostgreSQL connections (85%), long
  queries and waiting locks; Redis memory (90% of `maxmemory`); MongoDB connections (80%);
  RabbitMQ backlog (10000 ready messages) and any memory or disk alarm; HTTP response time.
  An alert opens once, is emailed once, and a "RESOLVED" email follows when it clears. An email
  that could not be sent is retried on later checks. Alerts always show on the Services page,
  even without email set up.
- **Pausing alert emails:** on a service's page, *Alert emails* pauses the emails for 1 hour,
  4 hours, 24 hours, 7 days or until you resume them (for a planned restart, or a service you
  have stopped on purpose). Checks, graphs and the alerts on the Services page carry on, and the
  card shows *emails paused*. If the problem is still there when the pause ends, the email is
  sent then. The pause is recorded in the Audit page. To stop checking a service altogether,
  edit it and untick *enabled*.
- Berth keeps its own pool to its database at 5 connections (the pgx default is one per CPU), because it shares the PostgreSQL server with the apps it manages. Each Postgres check opens one short-lived connection, named `berth-monitor` in `pg_stat_activity`.
- Passwords are stored encrypted like app secrets and are never shown again. When you edit a service and leave the password empty, the saved one is kept, unless you changed the username, host or port: then Berth asks you to type the password again, because the saved one belongs to the old settings. Berth only reads;
  use read-only monitoring users.

**Creating monitoring users** (Berth runs on the same host, so connect to `127.0.0.1`):

```sql
-- PostgreSQL
CREATE ROLE berth_monitor LOGIN PASSWORD '...';
GRANT pg_monitor TO berth_monitor;
-- pg_hba.conf must allow it from 127.0.0.1 (scram-sha-256)
```

```sh
# RabbitMQ: the management API is not open on the server yet
sudo rabbitmq-plugins enable rabbitmq_management
sudo rabbitmqctl add_user berth '...'
sudo rabbitmqctl set_user_tags berth monitoring
sudo rabbitmqctl set_permissions -p / berth '^$' '^$' '^$'   # matches nothing: can list queues, cannot read or publish. Repeat per vhost
```

MongoDB: a user with the `clusterMonitor` role. Redis: an ACL user limited to `+info +ping`, or no
password if it listens on loopback only.

**Not built yet:** metrics for the apps' own containers (CPU, memory, restarts) and the Docker
engine, and the managed-service actions (create a database and user, backups, restore, flush,
prune). Monitoring is read-only.

### Security page (`/_berth/security`)

Berth scans every registered app once a day (the first scan runs two minutes after start; **Scan now**
runs one on demand) and shows the result per app. It only reads: nothing is changed, and secret values
are never read into a result.

- **Go packages:** each `go.mod` of the app (the folder itself and up to two levels down, so Kite360's
  `go-api/go.mod` is found) is compared with the public Go vulnerability database. A package is listed
  when the version in use falls in an affected range, with the vulnerability ids (CVE/GHSA), a summary,
  the version that fixes it and, per package, the version to update to. The comparison happens on this
  server: only `https://vuln.go.dev/index/modules.json` (the same file for everyone) and the entries of
  matching vulnerabilities are downloaded, so the list of packages is not sent anywhere. A listed
  package uses a version with a published vulnerability, which is a reason to update; it does not prove
  the code calls the flaw (that needs `govulncheck`). The Go standard library and npm packages are not checked yet.
- **Containers:** the running containers of the app (`docker ps`, `inspect`) are checked for: privileged
  mode and a mounted Docker socket (high), extra capabilities such as `SYS_ADMIN` (high) and shared
  process list (high), the host network and running as root (medium), ports published on all interfaces,
  and images built more than 180 days ago (low).
- **Secrets in the repository:** the files git tracks, and every file name that was ever committed, are
  matched for `.env*` (not `.env.example`), `*.bak`, private keys and certificate bundles,
  `cookies.txt`, `.npmrc`/`.netrc`/`.pgpass`, database dumps and SQLite files, and tracked JSON files that
  hold a service account key. Files still in the repository are red, files removed but still in the git
  history are amber: anything ever committed should be treated as leaked and rotated, because removing
  the file does not remove it from the history. Only names are shown.
- **Break-in attempts** (refreshed every 5 minutes, not daily):
  - **SSH:** the SSH log (`/var/log/auth.log` and the rotated `.1`, or `BERTH_AUTH_LOG`) is read for failed
    passwords and invalid user names. It shows the failures of the last 24 hours and 7 days, the
    addresses and user names behind them, attempts as `root`, and the recent successful logins, so you can
    see whether each one was you. A **login that followed 5 or more failures from the same address** within
    the day before is flagged in red: that is what a guessed password looks like. It also reads `sshd`'s settings
    (`/etc/ssh/sshd_config` and `sshd_config.d`) and warns when password login or root login is on, and shows
    whether `fail2ban` is running. Berth's user must be able to read the log: on Ubuntu add it to the `adm`
    group (`sudo usermod -aG adm <user>`, then restart Berth); the page says so when it cannot. Addresses
    are only counted and shown: none is looked up or sent anywhere.
  - **Berth sign-ins:** every failed or refused sign-in is now written to the audit trail (`login.failed`,
    `login.blocked`) with the visitor's address (from Cloudflare's `CF-Connecting-IP` header, else the
    first forwarded address, else the connection's), and summarised by address. At most 20 are recorded per
    minute, so an attack cannot fill the database; the log file still has every one.
- A check that cannot run (folder missing, no `git`, Docker not reachable, the database unreachable)
  says so instead of showing "nothing found".

## 13. Updating Berth on the server

Berth does not update itself. The server holds a git checkout of this repository, so an update is
**pull, build, swap, restart**. `./update.sh` does all four:

```sh
cd /mnt/blockvolume/Berth
./update.sh
```

- It runs `git pull --ff-only`, builds to `berth.new` beside the running binary, keeps the old one as
  `berth.prev`, swaps with an atomic `mv` (safe while Berth is running), and restarts the `berth`
  systemd service if there is one. Otherwise it tells you to restart by hand.
- **Database changes are automatic:** Berth applies its own migrations when it starts. They only add
  tables and columns. Take a copy first if you like: `pg_dump "$BERTH_DATABASE_URL" -Fc -f ~/berth-$(date +%F).dump`.
- **Which build is running:** `./berth version`, the `berth <commit>` line at startup, and the footer of
  every page. `+local-changes` means the folder had uncommitted edits when it was built.
- **Way back:** `mv -f berth.prev berth && sudo systemctl restart berth`. Migrations are not undone,
  but they are additive, so the previous build keeps working.
- **First time only:** if the folder was copied by hand and `git pull` complains about local changes,
  `git status` shows what differs. For a source-only folder like this one,
  `git fetch origin && git reset --hard origin/main` is safe (it never touches `data/`, which is ignored).
- If the repository is private, `git pull` needs credentials on the server (an SSH remote or a token).
- Needs Go 1.26 or newer (the server has 1.27.3) and internet access for the Go modules.

## 14. Moving an app under Berth: checklist

Learned moving the Kite360 and NyasaHub apps from hand-started processes and containers to Berth
on 2026-11-23. All four apps (Kite360 web and API, NyasaHub web and API) now run through Berth; SB API followed on 2026-11-24 (items 12 to 15).

1. **Back up the database first** (`pg_dump -Fc <db>`) and keep a copy off the server.
2. **A container cannot use `127.0.0.1` or `localhost` for host services.** Use
   `host.docker.internal` for PostgreSQL, RabbitMQ, GopherPost and the rest. Save those values as
   Berth variables: they win over `.env`, so a still-running old process keeps reading its own `.env`.
   (An app that can use `network_mode: host`, like the NyasaHub API, does not need this.)
3. **PostgreSQL:** add `host <db> <user> 172.16.0.0/12 scram-sha-256` to `pg_hba.conf`, reload, and test
   from a throwaway container before deploying:
   `docker run --rm --add-host host.docker.internal:host-gateway -e PGPASSWORD postgres:14-alpine psql "host=host.docker.internal user=... dbname=..."`.
4. **RabbitMQ `guest` only works from the same machine,** so a container needs its own user
   (`rabbitmqctl add_user`, `set_permissions`).
5. **Run the container as the owner of the host folders it writes to** (`APP_UID`/`APP_GID`, 1001 is
   `ubuntu`). Files such as a Firebase key must be mounted (`SECRETS_DIR`) and the path variable must
   be the path *inside* the container (`/run/secrets/kite360/...`).
6. **Networks shared by two stacks** (here `kite360`, used by the API and Gotenberg) are declared
   `external: true` in both and created once by hand: Compose refuses to adopt a network it did not create.
7. **Exactly one scheduler.** `SCHEDULER_ENABLED=true` in one running process only. The Kite360 API logs
   `reminders ready` (on) or `reminders NOT ready: SCHEDULER_ENABLED is false` (off); check it after every deploy.
8. **Stop the old process before the container binds its port,** and make sure nothing revives it
   (`crontab -l`, systemd units, pm2). Record how it was started, in case you need to go back.
9. **Email loses mail silently when sending is broken:** the Kite360 consumer retries four times, then
   acknowledges the message and records `failed` in `public.email_log`. After a move, check
   `SELECT status, count(*) FROM public.email_log WHERE date_created > now() - interval '30 minutes' GROUP BY status`
   and expect `sent`.
10. **Existing stacks keep their volumes** only if Berth uses their compose project name
    (`compose_project`), see section 9.
11. **Never commit `.env` copies** (`.env.*`, `*.bak`); check `git ls-files | grep env` first.
12. **Check what the app opens by relative path** (`grep -rnE 'ParseFiles|ReadFile|os.Open|"\./|"internal/'`).
    Anything not copied into the image (templates, data files) fails only when that code runs, often at
    start. SB-API needed `internal/templates`, `internal/models/metadata` and writable `geonames/`/`uploads/`
    folders. Test the image as the user it will run as: `docker run --rm --user 1001:1001 --entrypoint sh <image> -c 'ls ...; touch ...'`.
13. **Build with the Go version the server's `go.mod` asks for**, not the Mac's: uncommitted edits on the server
    can raise it (`go.mod requires go >= 1.25.0`). Keep `.env`, key files and `cookies.txt` out of the image with `.dockerignore`.
14. **A key file read by relative path** (Firebase `./key.json`) needs a Berth variable pointing at the mounted path
    (`/run/secrets/...`). Without it the Firebase library only says "project ID is required", not that the file is missing.
15. **A deploy that fails at "unhealthy" leaves a restarting container**; each restart can re-run seeds against the
    live database. Read `docker logs` for the cause, and `docker stop` it if you cannot fix it quickly.
16. **Every app needs its own RabbitMQ queue names.** NyasaHub and SB-API both defaulted to `email_messages_queue`
    and each consumer looks a message's `SentEmailID` up in its own database, so an email could be taken by the
    wrong app (sent wrongly, dropped or bounced). Set `RABBITMQ_EMAIL_QUEUE` per app (`sb_email_queue`,
    `nyasahub_email_queue`, `kite360_email_queue`) and check `rabbitmqctl list_queues name consumers` shows one
    consumer on each.
