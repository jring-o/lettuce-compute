# Deploying a Lettuce Head

A **head** is a Lettuce server. This guide takes you from nothing to a running head
that volunteers can attach to. Once it's up, create your first computation by
following [first-leaf.md](first-leaf.md).

> **Terminology.** A **head** is a Lettuce server (the thing you deploy here).
> A **leaf** is a single computation running on that head. One head hosts many leafs.

Pick a path:

- **[Path A — Local dry run](#path-a--local-dry-run)** — run the whole stack on your
  own machine, no domain and no cost. Best for trying Lettuce and validating that
  everything works before you pay for a server.
- **[Path B — Production](#path-b--production-server--domain)** — a real, public head
  on a server with a domain and automatic HTTPS. Includes a DigitalOcean walkthrough
  and notes for any other provider.

---

## What you're deploying (production)

A production head is six containers behind one domain, all traffic on port 443:

| Container | What it does |
|-----------|--------------|
| **postgres** | PostgreSQL database — stores leafs, work units, results, volunteers |
| **infrastructure** | Go server — task distribution, result validation, credit tracking. Runs as N stateless replicas — scale with `--scale infrastructure=N` (default 1). |
| **redis** | Shared store for the cross-replica replay dedup + rate-limit buckets. Used when you run more than one `infrastructure` replica (see [Horizontal scale-out](#horizontal-scale-out)). |
| **dashboard** | Next.js web app — public leaf browser + admin console |
| **registry** | OCI image registry — hosts container images for container leafs |
| **caddy** | Reverse proxy — automatic HTTPS, routes everything on port 443, load-balances across head replicas |

Caddy routes by request:

- `https://your-domain.com` → dashboard
- `https://your-domain.com/api/v1/*` → infrastructure REST API
- `https://your-domain.com/binaries/*` → compute binary downloads
- `https://your-domain.com/v2/*` → image registry (anonymous pull, authenticated push)
- gRPC (volunteer connections) → multiplexed onto port 443 and proxied to infrastructure

No ports other than 80 and 443 are exposed. Volunteers connect over 443.

---

## Path A — Local dry run

Run the full stack on your own machine to confirm it works. This uses the development
compose file (`compose.yaml`): plain HTTP on `localhost`, no domain, no TLS, no registry.

### Requirements

- [Docker](https://docs.docker.com/get-docker/) (or Podman with the Docker-compatible
  `docker compose` interface)
- `git`

### Steps

```bash
# 1. Clone the repository
git clone https://github.com/jring-o/lettuce-compute.git
cd lettuce-compute

# 2. Build and start the dev stack (postgres + infrastructure + dashboard)
docker compose up -d --build
```

Wait about a minute for the images to build and the database to migrate, then verify:

```bash
# 3. Infrastructure health — expect {"status":"healthy","database":"connected"}
curl http://localhost:8080/api/v1/health
```

Open the dashboard at <http://localhost:3000>.

The development stack ships with a fixed admin API key, `dev-admin-key-not-for-production`.
Use it for API calls while testing locally:

```bash
# 4. Create your first leaf locally — full walkthrough in first-leaf.md
#    Use base URL http://localhost:8080 and:
#      Authorization: Bearer dev-admin-key-not-for-production
```

Optionally attach a volunteer on the same machine. Build the CLI once, then point it at
your local head over plain gRPC:

```bash
# 5. (optional) Build and run a local volunteer
go build -o lettuce-volunteer ./services/volunteer-cli/cmd/lettuce-volunteer
./lettuce-volunteer init
./lettuce-volunteer attach --server localhost --grpc-port 9090 --insecure
./lettuce-volunteer start
```

> `--grpc-port 9090 --insecure` are only valid on `attach`, not `init` — the dev stack
> serves plain gRPC on 9090 with no TLS. In production both default to 443 over HTTPS.
>
> **Fetching locally-hosted leaf artifacts needs two explicit opt-ins.** By default the
> head refuses `http://` binary URLs at leaf configuration (set
> `LETTUCE_BINARY_URL_ALLOW_INSECURE=true` on the `infrastructure` service for local
> testing only), and the volunteer's network guard refuses artifact downloads that
> resolve to loopback or private-network addresses. To let this local volunteer fetch
> binaries hosted on your own machine, start it with your head opted in:
> `LETTUCE_VOLUNTEER_ALLOW_PRIVATE_ARTIFACTS=localhost ./lettuce-volunteer start`.
> The opt-in is scoped to the named head(s) only and WARN-logged on every use — full
> details and caveats in first-leaf.md → “Testing locally”.

Tear down when you're done:

```bash
docker compose down       # stop, keep the database
docker compose down -v    # stop and wipe all data
```

> **The `make` targets are shortcuts for this development stack.** The repo-root
> `make dev-up`, `make dev-down`, `make dev-logs`, `make dev-rebuild`, and
> `make dev-reset` each run `docker compose -f compose.yaml -p lettuce-dev …` under a
> dedicated **`lettuce-dev`** Compose project name. That dedicated project name is
> deliberate: it structurally isolates the dev stack from any production stack in the
> same directory, so a `make` target can never touch your production containers or
> volumes. `make dev-reset` asks for confirmation (type `dev`) before it wipes the
> database.
>
> **One-time migration.** If you ran this dev stack before under the old default
> project name (a bare `docker compose up` with no `-p`), stop those old containers and
> volumes once with a bare `docker compose down` (add `-v` to also wipe the old data)
> **before** your first `make dev-up`, so the old project's containers don't linger
> alongside the new `lettuce-dev` ones. The bare `docker compose down` / `down -v`
> commands shown just above stay correct for that old default project and for anyone
> not using `make`.
>
> There are deliberately **no production `make` targets** — production always uses the
> explicit `-f compose.production.yaml` flag shown below.

> **Note.** The dev stack doesn't create a dashboard login account (it has no admin
> email/password configured), so dashboard sign-in is a production feature. Local mode
> is for validating the stack and the REST API. When you're ready for a real,
> sign-in-capable head, use Path B.

---

## Path B — Production (server + domain)

You'll need two things before you start:

1. **A domain you control** (e.g. `your-domain.com`) — you'll create DNS records for it.
2. **A Linux server** running Ubuntu 22.04+ — provisioned in Step 1.

The whole process is ten steps and takes about 20 minutes (plus DNS propagation).

### Step 1 — Get a server

**Recommended size:** 2 GB RAM / 1 vCPU or larger. The dashboard image is memory-hungry
to build; on a 1 GB server you must build images one at a time (covered in Step 9).

<details>
<summary><strong>Option: DigitalOcean</strong> (click to expand walkthrough)</summary>

1. Create a Droplet: **Ubuntu 22.04 (LTS) x64**, **Basic** plan, **2 GB / 1 CPU** or larger.
2. Under **Authentication**, add your SSH key (so you can log in without a password).
3. Create the droplet and note its **public IPv4 address** (shown on the droplet page).

</details>

<details>
<summary><strong>Option: any other provider</strong> (Hetzner, AWS Lightsail, Vultr, Linode, …)</summary>

Create an **Ubuntu 22.04** server with **≥ 2 GB RAM** and SSH access, and note its
**public IPv4 address**. Everything below is provider-neutral.

</details>

### Step 2 — Point DNS at the server

At your domain registrar, create **two** A records, both pointing to your server's IP:

| Name | Type | Value |
|------|------|-------|
| `your-domain.com` | A | *your server's IP* |
| `viz.your-domain.com` | A | *your server's IP* |

The `viz.` subdomain serves visualization bundles from a separate origin for browser
security isolation. Caddy provisions a TLS certificate for **both** names, so both must
resolve before you start the stack.

Confirm DNS has propagated (should print your server's IP):

```bash
dig +short your-domain.com
dig +short viz.your-domain.com
```

### Step 3 — Install Docker

SSH into the server and install Docker:

```bash
ssh root@your-domain.com           # or ssh root@<server-ip>

sudo apt update
sudo apt install -y ca-certificates curl git
curl -fsSL https://get.docker.com | sh
```

### Step 4 — Open firewall ports

```bash
sudo ufw allow 22/tcp     # SSH
sudo ufw allow 80/tcp     # HTTP — TLS challenge + redirect to HTTPS
sudo ufw allow 443/tcp    # HTTPS — dashboard, REST API, and gRPC all go here
sudo ufw enable
```

No other ports are needed — volunteer gRPC traffic is multiplexed onto 443 by Caddy.

### Step 5 — Clone the repository

```bash
git clone https://github.com/jring-o/lettuce-compute.git
cd lettuce-compute
```

### Step 6 — Generate secrets and write `.env`

```bash
cp .env.example .env
chmod 600 .env
```

Generate the secret values. Run this four times — once each for `NEXTAUTH_SECRET`,
`LETTUCE_ADMIN_API_KEY`, `DASHBOARD_API_KEY`, and `POSTGRES_PASSWORD`:

```bash
openssl rand -base64 32
```

Generate the Redis password separately — it rides inside a connection URL, so it
must be URL-safe (hex always is):

```bash
openssl rand -hex 32
```

Generate the registry push password and its hash (you'll need the plaintext to push
images later, and the hash for `.env`):

```bash
REGPASS=$(openssl rand -base64 16)
echo "registry password (save this): $REGPASS"
docker run --rm caddy:2-alpine caddy hash-password --plaintext "$REGPASS"
```

Now edit `.env` (`nano .env`) and set every value:

```bash
POSTGRES_PASSWORD=<random; avoid / and @ characters>
REDIS_PASSWORD=<random hex — required; the stack refuses to start without it>
PLATFORM_URL=https://your-domain.com
NEXTAUTH_SECRET=<random>
LETTUCE_ADMIN_API_KEY=<random — save this; you'll use it for every API call>
DASHBOARD_API_KEY=<random>
LETTUCE_ADMIN_EMAIL=you@example.com
LETTUCE_ADMIN_PASSWORD=<your dashboard admin password>
LETTUCE_HEAD_NAME=Your Server Name
LETTUCE_HEAD_DESCRIPTION=What this head computes
LETTUCE_CORS_ORIGINS=https://your-domain.com
VIZ_ORIGIN=https://viz.your-domain.com
REGISTRY_USER=lettuce
REGISTRY_PASS_HASH=<the hash printed by caddy hash-password>
```

| Variable | What it's for |
|----------|---------------|
| `POSTGRES_PASSWORD` | Database password. Avoid `/` and `@` (they break the connection string). |
| `REDIS_PASSWORD` | Password for the bundled Redis (the head's shared replay/rate-limit store). **Required** — the compose file refuses to interpolate without it. Use hex (`openssl rand -hex 32`); it is embedded in a URL, so other characters would need percent-encoding. |
| `PLATFORM_URL` | Your full public URL, with `https://`. Used for auth callbacks and the head's advertised URL. |
| `NEXTAUTH_SECRET` | Signs dashboard session cookies. |
| `LETTUCE_ADMIN_API_KEY` | Bootstrap key for authenticated API calls. **Save it** — you'll need it to create leafs. |
| `DASHBOARD_API_KEY` | The key the dashboard uses to talk to the infrastructure server. |
| `LETTUCE_ADMIN_EMAIL` / `LETTUCE_ADMIN_PASSWORD` | Dashboard admin account, created automatically on first boot. The password is bcrypt-hashed for you. |
| `LETTUCE_HEAD_NAME` / `LETTUCE_HEAD_DESCRIPTION` | What volunteers see for this head. |
| `LETTUCE_CORS_ORIGINS` | Allowed browser origins (your domain). |
| `LETTUCE_GRPC_PER_IP_RATE_LIMIT` | *(optional)* Per-source-IP gRPC request budget, **requests per minute** (default 60). Raise this when a whole fleet legitimately shares one source IP — e.g. many volunteers behind a single NAT, or a load test from one host — so the shared per-IP bucket does not throttle the fleet to ~1 req/s. Combine with `LETTUCE_TRUSTED_PROXIES` so volunteers behind your reverse proxy are still bucketed per real client IP. |
| `LETTUCE_GRPC_PER_PUBKEY_RATE_LIMIT` | *(optional)* Per-authenticated-volunteer gRPC request budget, **requests per minute** (default 120), keyed on the volunteer's verified Ed25519 key. This limiter sits *after* auth, so it sheds database/handler load but not signature-verification cost (the per-IP limiter is the only pre-auth, crypto-shedding ceiling). |
| `LETTUCE_GRPC_PER_IP_STREAM_LIMIT` | *(optional)* Per-source-IP gRPC **stream** budget, **streams per minute** (default 600). The pre-decode flood backstop: enforced when a stream is opened, *before* the server reads or decodes the request body, and covering **every** method — including the in-flight work RPCs (`SubmitResult`, `SaveCheckpoint`, …) that are deliberately exempt from the request-rate limiters above. The default sits far above an honest volunteer's cadence; raise it together with `LETTUCE_GRPC_PER_IP_RATE_LIMIT` for NAT'ed fleets that share one source IP. |
| `VIZ_ORIGIN` | The `viz.` subdomain, for visualization isolation. **Required in production** — it binds the viz-bundle route to this origin so author bundle code only runs in the sandboxed viz origin, never on your main app origin. It must be a **distinct host** from `PLATFORM_URL` and must **not share a cookie parent-domain** with it. If it is unset, empty, or resolves to the same origin as `PLATFORM_URL`, the dashboard **fails closed**: the visualization view does not render (it will not run author bundle code on the app origin). |
| `VIZ_BUNDLE_ALLOWED_ORIGINS` | *(optional)* Comma-separated `scheme://host[:port]` origins the viz-bundle route may fetch tarballs from. Defaults to the `PLATFORM_URL` origin (where `/binaries/` is served), so you normally don't set it. Set it only if you host viz tarballs on additional origins (e.g. a CDN). |
| `REGISTRY_USER` / `REGISTRY_PASS_HASH` | Credentials for pushing container images. The proxy needs the hash to start. |

### Step 7 — Set your domain in the Caddyfile

The `Caddyfile` ships with `your-domain.com` placeholders. Replace all of them with your
domain in one command (replace `example.com` with your actual domain):

```bash
sed -i 's/your-domain\.com/example.com/g' Caddyfile
```

This updates all four occurrences (the main site, the `viz.` subdomain, and the two
HTTP→HTTPS redirects). Caddy automatically obtains and renews Let's Encrypt certificates
for both names — there is no manual certificate step.

### Step 8 — Generate the signing key

The head signs credit attestations with an Ed25519 key. This key is the head's
**external trust anchor** — volunteers and consumers verify attestations against its
published public key. You **must** generate it before starting the production stack:
the server **fails to start** (a clear fatal error, not a silent regeneration) if the
key file is missing, so it can never quietly mint a new signing identity. For how consumers
independently verify attestations against this key's public half, see
[guides/attestation-verification.md](attestation-verification.md).

```bash
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/signing.key
sudo chown 10001:10001 keys/signing.key
chmod 600 keys/signing.key
```

This writes a PKCS#8 PEM file, which is exactly the format the server reads. The
production compose file mounts `./keys` read-only at `/keys` and loads
`/keys/signing.key`.

The `chown`/`chmod` matter: the head's container runs as the non-root user
**uid/gid 10001** (deliberately not 1000, so it can never accidentally match the
first host user), and the key must be readable by that uid and nobody else.
`10001:10001` with mode `600` is the supported layout, and the head now **enforces
it at boot**: it refuses to start when the key is group- or other-readable (any mode
beyond `600`) or owned by a different uid, failing with an error that names the
problem ("insecure permissions" or "not owned by") and prints the fix. If you hit
that, run `sudo chown 10001:10001 keys/signing.key && chmod 600 keys/signing.key`
and restart.

Back this key up somewhere safe. If you lose it, new attestations will carry a different
signer identity and every previously published attestation stops verifying.

> The development stack (`compose.yaml`, Path A) needs no key file — it sets
> `LETTUCE_SIGNING_KEY_AUTOGEN=true` to auto-generate an ephemeral key on first boot.
> Never enable that flag in production.

### Step 9 — Start the stack

On a 2 GB+ server, build and start everything at once:

```bash
docker compose -f compose.production.yaml up -d --build
```

On a 1 GB server, build images one at a time to avoid running out of memory:

```bash
docker compose -f compose.production.yaml build infrastructure
docker compose -f compose.production.yaml build dashboard
docker compose -f compose.production.yaml up -d
```

Database migrations and the admin user are created automatically on first boot.

### Step 10 — Verify

```bash
# Infrastructure health — expect "status":"healthy","database":"connected"
curl https://your-domain.com/api/v1/health
```

Open <https://your-domain.com> — you should see the dashboard.

Confirm the admin bootstrap ran:

```bash
docker compose -f compose.production.yaml logs infrastructure | grep bootstrap
```

You should see:

```
level=INFO msg="admin user created via bootstrap" email=you@example.com username=admin
level=INFO msg="dashboard API key created via bootstrap"
```

Sign in at `https://your-domain.com/sign-in` with the email and password from your `.env`.
The admin console is at `https://your-domain.com/dashboard/leafs`, with user management at
`/dashboard/admin/users` and settings at `/dashboard/settings`. The public leaf browser
is at `/leafs`.

**Your head is live.** Next: [create your first leaf](first-leaf.md).

---

## Connecting volunteers

Give volunteers two things: your head's address and the `lettuce-volunteer` binary
(from a GitHub release, the desktop app, or built from source). They run:

```bash
lettuce-volunteer init --server your-domain.com
lettuce-volunteer start
```

`init --server` generates identity keys, detects hardware, and configures the connection
(443 over HTTPS by default). `start` connects and begins computing. A volunteer can attach
to additional heads later with `lettuce-volunteer attach --server another-head.example.com`.

Attached volunteers automatically pick up every **PUBLIC** leaf on your head. A leaf
created with `UNLISTED` or `PRIVATE` visibility is absent from the public catalog, so it
gets no volunteers until each one explicitly pins it by ID:
`lettuce-volunteer attach <leaf-id>` (then restart the daemon). See the visibility table
in [first-leaf.md](first-leaf.md) Step 4.

---

## Operations

### Logs

```bash
docker compose -f compose.production.yaml logs -f                 # all services
docker compose -f compose.production.yaml logs -f infrastructure  # one service
```

Every service's container log is rotation-bounded in `compose.production.yaml`
(json-file driver, 50 MB × 5 files per container), so logs cannot fill the host
disk. Adjust the `x-default-logging` anchor at the top of the compose file if
you want more or less history.

### Resource limits

Every service in `compose.production.yaml` carries a memory ceiling
(`mem_limit`) and a process/thread ceiling (`pids_limit`), so one leaky or
runaway service is OOM-killed and restarted **alone** instead of exhausting the
host and taking every other service down with it. The defaults are conservative
for a small (~4 GB) host and tunable from `.env` without editing the compose
file:

| Service | Memory (default) | PIDs (default) | `.env` overrides |
|---------|-----------------|----------------|------------------|
| infrastructure | 1g | 512 | `INFRA_MEM_LIMIT` / `INFRA_PIDS_LIMIT` |
| postgres | 1g | 256 | `POSTGRES_MEM_LIMIT` / `POSTGRES_PIDS_LIMIT` |
| dashboard | 768m | 256 | `DASHBOARD_MEM_LIMIT` / `DASHBOARD_PIDS_LIMIT` |
| redis | 256m | 128 | `REDIS_MEM_LIMIT` / `REDIS_PIDS_LIMIT` |
| registry | 256m | 128 | `REGISTRY_MEM_LIMIT` / `REGISTRY_PIDS_LIMIT` |
| caddy | 256m | 256 | `CADDY_MEM_LIMIT` / `CADDY_PIDS_LIMIT` |

On a bigger host, raise the `infrastructure` and `postgres` ceilings first —
they are the two that grow with fleet size. A service that keeps getting
OOM-killed (visible as restarts in `docker compose ps` and `137` exit codes in
`docker inspect`) needs its ceiling raised, not the limit removed. Note the
ceilings are limits, not reservations — on a 1–2 GB host they simply sit above
available memory, and the kernel's global OOM killer remains the backstop.

### Database TLS (external Postgres)

The bundled deploy runs Postgres on the compose file's private network with
`sslmode=disable`, which is fine — the traffic never leaves the Docker bridge.
If you point the head at an **external** Postgres, note that the database
connection settings (`LETTUCE_DB_HOST`, `LETTUCE_DB_SSL_MODE`, credentials)
are set as literals in `compose.production.yaml` — they are wired to the
bundled `postgres` service, not to `.env`. Edit the `infrastructure` service's
environment in the compose file, and set:

```yaml
LETTUCE_DB_SSL_MODE: verify-full
```

The permissive modes (`disable`, `allow`, `prefer`) permit a silent plaintext
downgrade: an on-path attacker can strip TLS and read or modify database
traffic, including credentials. The head WARNs at boot when it detects a
permissive mode against a host that is not on a loopback/private network —
treat that warning as a misconfiguration, not noise.

One related proxy note: requests to `/api/v1/*` through the bundled Caddy carry
a **64MB request-body ceiling**. The only endpoint that can legitimately
approach it is bulk work-unit upload with large inline `input_data` — split
such batches into multiple requests (the endpoint itself enforces splitting
past 10,000 units; an oversized body gets an HTTP 413 from the proxy).

### Metrics & profiling

The head exports Prometheus metrics at `GET /metrics` and Go runtime profiles
at `GET /debug/pprof/`, both on its HTTP port (8080). Two access properties to
understand:

- **Not reachable from the internet in the shipped topology.** Caddy proxies
  only `/api/v1/*` (and gRPC) to the head, and the infrastructure service
  publishes no host ports — so `https://your-domain.com/metrics` hits the
  dashboard's 404, not the head. A scraper must run inside the compose network
  and target `http://infrastructure:8080/metrics`.
- **Admin-authenticated regardless.** Both endpoints require the admin API key
  (`Authorization: Bearer $LETTUCE_ADMIN_API_KEY`), so a deploy that exposes
  port 8080 directly still does not expose runtime internals. Give your
  Prometheus scrape job the admin key via `authorization: { credentials: ... }`.

Quick manual check from the server:

```bash
docker compose -f compose.production.yaml exec caddy \
  wget -qO- --header "Authorization: Bearer $LETTUCE_ADMIN_API_KEY" \
  http://infrastructure:8080/metrics | head -50
```

What is exported (all families prefixed `lettuce_`, plus standard Go runtime
and process collectors):

| Family | What it tells you |
|--------|-------------------|
| `lettuce_grpc_requests_total{method,code}` / `lettuce_grpc_request_duration_seconds{method}` | Volunteer gRPC traffic: rate, error mix, latency per RPC. `code="ResourceExhausted"` is the shed/backpressure signal. |
| `lettuce_http_requests_total{method,status}` / `lettuce_http_request_duration_seconds{method}` | REST/dashboard traffic rate, status mix, latency. |
| `lettuce_dispatch_ready_pool_size` | Work-unit reservations staged in the in-memory dispatch cache. Persistently 0 under load = dispatch starvation (volunteers polling faster than the refiller can stage). |
| `lettuce_dispatch_pending_reservation_writes` / `lettuce_dispatch_pending_spot_check_writes` | Async flush-queue depth. Sustained growth = the flusher cannot keep up with hand-out volume. |
| `lettuce_dispatch_shed_total{site}` | Load-shed refusals by site (`request_work_ready_pool`, `submit_result_db_slot`, …). Any sustained rate means volunteers are being turned away — the head is saturated. |
| `lettuce_db_pool_acquired_conns` / `_idle_conns` / `_max_conns` | Postgres pool pressure. `acquired` pinned at `max` = pool exhaustion. |

`/debug/pprof/` (same admin gate) serves the standard Go profiles — heap,
goroutine, allocs, plus `/debug/pprof/profile` (30s CPU) and
`/debug/pprof/trace`. For an out-of-memory or high-CPU incident:

```bash
# From inside the compose network (e.g. the caddy container as above):
wget -qO heap.pb.gz    --header "Authorization: Bearer $LETTUCE_ADMIN_API_KEY" http://infrastructure:8080/debug/pprof/heap
wget -qO profile.pb.gz --header "Authorization: Bearer $LETTUCE_ADMIN_API_KEY" "http://infrastructure:8080/debug/pprof/profile?seconds=30"
# Then copy the files off the server and open with: go tool pprof <file>
```

### Update to the latest version

```bash
git pull
docker compose -f compose.production.yaml build infrastructure
docker compose -f compose.production.yaml build dashboard
docker compose -f compose.production.yaml up -d
```

Migrations run automatically on startup (booting several replicas at once is
safe — the migration runner takes an internal advisory lock, so exactly one
replica applies them and the others wait; see
[guides/migrations.md](migrations.md)).

**Upgrading across the container-hardening + secrets-hardening releases**
(non-root containers, resource limits, a required Redis password, boot-time
secret validation, and enforced signing-key permissions) is **one combined
deploy** on the production head. Work through this checklist **before** you run
`up -d`:

1. **Add a Redis password to `.env`.** The compose file now requires it, and it
   must clear the new 32-character floor for generated secrets — `openssl rand
   -hex 32` satisfies both:

   ```bash
   echo "REDIS_PASSWORD=$(openssl rand -hex 32)" >> .env
   ```

2. **Fix the signing-key ownership and mode.** The head container runs as uid
   10001 (non-root), and the head now **enforces the key's permissions at boot** —
   it refuses to start if the key is group/other-readable or owned by another uid:

   ```bash
   sudo chown 10001:10001 keys/signing.key && chmod 600 keys/signing.key
   ```

3. **Hand the dashboard data volume to the `node` user.** The dashboard container
   now runs as the `node` user (uid 1000); an existing `dashboard_data` volume
   created by an older (root) image keeps root ownership, so chown it once:

   ```bash
   docker compose -f compose.production.yaml run --rm --no-deps --user root dashboard \
     chown -R node:node /app/data
   ```

4. **Audit `.env` for placeholder and weak secrets — rotate them first.** The head
   now **refuses to boot** on any secret that is a known placeholder or under its
   length floor, failing with a `boot secret validation: <VARIABLE>` error that
   names the offending variable. Blocklisted (case-insensitive) stems include
   `change-me`, `changeme`, `generate-with`, `replace-with`, `placeholder`, and
   `not-for-production`. The length floors are **32 characters** for the generated
   machine secrets (`LETTUCE_ADMIN_API_KEY`, `DASHBOARD_API_KEY`, `REDIS_PASSWORD`,
   `NEXTAUTH_SECRET`) and **12 characters** for the human passwords
   (`LETTUCE_ADMIN_PASSWORD`, and `POSTGRES_PASSWORD` only when the database host is
   not on a private/loopback network — the bundled topology is private, so its DB
   password is not floored). Generate replacements with `openssl rand -base64 32`
   (or `-hex 32` for a value that rides inside a URL, like `REDIS_PASSWORD`) and
   update `.env` **before** upgrading. Two rotation ripples to know:

   - Rotating `DASHBOARD_API_KEY` makes the head **re-mint** a dashboard key on the
     next boot; older non-placeholder key rows are **not** auto-revoked.
   - Rotating `LETTUCE_ADMIN_PASSWORD` does **not** change an already-created admin
     — bootstrap is create-only. Reset an existing admin's password from the
     dashboard instead (or, for a locked-out placeholder admin, use the SQL runbook
     in the [troubleshooting](#troubleshooting) table below).

   Two related boot-time actions also happen automatically on a production boot: the
   head **auto-revokes** any API key whose value is a known placeholder (it logs a
   WARN and mints a fresh `DASHBOARD_API_KEY`), and it **refuses to boot** if a
   role-ADMIN user still carries a known placeholder password, failing with
   `refusing to start: admin user "..." still has a known placeholder password`.
   Rotate those before the upgrade too.

After the first `up -d`, confirm the head came up clean. Both of these greps should
print nothing:

```bash
docker compose -f compose.production.yaml logs infrastructure | grep -iE 'permission|denied'
docker compose -f compose.production.yaml logs infrastructure | grep -i 'boot secret validation'
```

The first confirms no file-permission failure (signing key or dashboard volume); the
second confirms no secret was rejected at boot. Then check `/api/v1/health` answers
healthy.

### Work dispatch tuning

The head paces volunteers and hands out work in batches so a large fleet creates
far less request noise. These are tuned with `head.*` keys in `lettuce.yaml`
(or the matching `LETTUCE_HEAD_*` env vars). Defaults are sane for a small head;
the only one you should actively calibrate is `target_request_rate_per_sec`.

| Key (env) | Default | What it does |
|-----------|---------|--------------|
| `max_batch_per_request` (`LETTUCE_HEAD_MAX_BATCH_PER_REQUEST`) | `64` | Safety ceiling on how many work units one work request may return. This is a cap, not the limiter — the actual batch is sized by each volunteer's work-buffer deficit and per-unit duration estimate. |
| `max_inflight_per_volunteer` (`LETTUCE_HEAD_MAX_INFLIGHT_PER_VOLUNTEER`) | `10` | Max live copies (running + buffered) one volunteer may hold across all units. Also caps how deep a volunteer's hours-based work buffer can fill. |
| `min_retry_delay_seconds` (`LETTUCE_HEAD_MIN_RETRY_DELAY_SECONDS`) | `30` | Server-directed retry delay handed out when quiet. Stamped on **every** reply (including no-work). This is **advisory** — well-behaved volunteers obey it, but a self-compiled client can ignore it; see `min_send_interval_seconds` for the server-enforced counterpart. |
| `max_retry_delay_seconds` (`LETTUCE_HEAD_MAX_RETRY_DELAY_SECONDS`) | `900` | Retry delay under full load. Must stay below the 1800s stale-volunteer threshold (validated at startup). |
| `retry_delay_jitter_pct` (`LETTUCE_HEAD_RETRY_DELAY_JITTER_PCT`) | `0.20` | Server-side ± jitter on the stamped delay so a fleet does not re-contact in lockstep. |
| `target_request_rate_per_sec` (`LETTUCE_HEAD_TARGET_REQUEST_RATE_PER_SEC`) | `500` | Per-head work-request rate the load estimator treats as "fully loaded". **Not calibrated** — measure your single-head dispatch ceiling with `swarm-sim` (see `CONTRIBUTING.md`) and set this to it. The 2026-06-01 reference run measured ~240 assignments/sec on a single head, well below the default. |
| `lease_seconds` (`LETTUCE_HEAD_LEASE_SECONDS`) | `900` | Fallback hold for a buffered copy **only when its work unit has no positive deadline**. Normally a buffered copy is held until its own `deadline_seconds`, so this rarely applies (no-deadline leafs get the `no_deadline_ceiling_seconds` deadline). No longer bound by the 1800s stale-volunteer threshold — the hold is the deadline, not a short liveness lease. |
| `min_send_interval_seconds` (`LETTUCE_HEAD_MIN_SEND_INTERVAL_SECONDS`) | `30` (**on**) | Minimum seconds between **successful work hand-outs to one volunteer** (keyed on its verified identity). Unlike the advisory `min_retry_delay_seconds` above — which a self-compiled volunteer can simply ignore — this is a **server-side hard floor** the head enforces itself, so a volunteer that polls aggressively (or hacks its client to ignore the retry delay) still gets at most one batch per interval and cannot grab a disproportionate share of a scarce queue. An early request is still served — it just returns no new work. **Enabled by default** at `30` (≈ `min_retry_delay_seconds`, so a well-behaved volunteer never trips it; the default is clamped to never exceed `max_retry_delay_seconds`). Set it **negative** (e.g. `-1`) to **disable** — the per-identity/per-IP rate limits and `max_inflight_per_volunteer` still apply regardless. A positive value must be ≤ `max_retry_delay_seconds`. Enforced per replica (in-memory); under multi-replica scale-out a volunteer that reconnects to another replica gets a fresh clock, so treat it as a strong throttle, not an exact quota. |

`LETTUCE_TRUSTED_PROXIES` also governs **per-client rate limiting** on the gRPC
port: with it set, volunteers behind your reverse proxy are bucketed per real
client IP (and per authenticated key) rather than sharing one proxy-IP bucket.
The per-pubkey limiter sits *after* auth, so it does not shed
signature-verification cost — the per-IP ceiling is the only pre-auth layer.

### Dispatch cache

To keep Postgres off the work-request hot path, the head serves assignments from
an **in-process dispatch cache**: a background refiller bulk-fetches queued units
into memory, work requests are answered from that in-memory pool (no database
round-trip on the hot path), and the resulting reservations are written back to
Postgres asynchronously in batches. Each hand-out lands as a **per-copy** reservation
row, so one unit can have several copies in flight at once (see *Redundancy and the
dispatch cache* below). Under sustained overload the head serves from the cache until
it empties, then **sheds** — it returns a fast "back off and retry" to volunteers
(which obey a short local backoff) instead of letting database connections pile up.
Run-start is an explicit step (the volunteer tells the head a buffered copy has begun
executing), and **liveness is deadline-based**: a copy not submitted by its deadline is
dropped and the unit redispatches a fresh copy, and a buffered copy not started before
its hold lapses is reclaimed. There are no per-task heartbeats.

On top of deadline-based reclaim, the head also reconciles each volunteer's
**client-reported buffer**: every work request carries the units the volunteer is
currently holding, and the head promptly releases any buffered (not-yet-started)
reservation a volunteer no longer holds — e.g. after a client restart that dropped its
buffer — so those units redispatch within seconds instead of waiting out the full
deadline, and they stop counting against that volunteer's `max_inflight_per_volunteer`.
This needs volunteers on **v0.5.1+**; older clients don't report a held set and fall back
to deadline-based reclaim only (so keep the fleet updated with `lettuce-volunteer update`).

> **Running more than one head?** The dispatch cache is safe across multiple
> replicas: each replica stamps a **per-head dispatch claim** on the queued units
> it stages (claim-on-refill), so a unit held in one replica's memory is invisible
> to every other replica's refiller and is never handed to two volunteers. See
> **[Horizontal scale-out](#horizontal-scale-out)** below for how to run N replicas.
> Single-replica deploys are unchanged — a single head simply re-claims its own
> units. Browser/WASM units are dispatched by a separate immediate-assign path,
> partitioned from the cache by runtime.

These cache knobs are `head.*` keys (defaults are sane; you rarely touch them):

| Key (env) | Default | What it does |
|-----------|---------|--------------|
| `ready_pool_size` (`LETTUCE_HEAD_READY_POOL_SIZE`) | `2000` | Max pre-fetched queued units held in memory for hand-out. |
| `refill_batch_size` (`LETTUCE_HEAD_REFILL_BATCH_SIZE`) | `500` | How many units one bulk refill pulls from Postgres. |
| `dispatch_admission_cap` (`LETTUCE_HEAD_DISPATCH_ADMISSION_CAP`) | `MaxConns/2` | Bounds concurrent CLIENT write-path dispatch-cache database operations (StartWork, SubmitResult, AbandonWorkUnit, run-start, the request cold-miss identity read) so they cannot saturate the pool. Background restock + landing use `maintenance_admission_cap`. |
| `maintenance_admission_cap` (`LETTUCE_HEAD_MAINTENANCE_ADMISSION_CAP`) | `dispatch_admission_cap/4` | Reserved admission budget for background restock + spot-check landing (refiller, reservation-flush, spot-check landing) so client writes cannot starve them. |
| `flush_interval_ms` (`LETTUCE_HEAD_FLUSH_INTERVAL_MS`) | `100` | How often buffered reservation writes are flushed to Postgres. |
| `flush_batch_size` (`LETTUCE_HEAD_FLUSH_BATCH_SIZE`) | `200` | Flush early once this many reservation writes are pending. |
| `no_deadline_ceiling_seconds` (`LETTUCE_HEAD_NO_DEADLINE_CEILING_SECONDS`) | `21600` (6h) | Reclaim ceiling applied to no-deadline leafs so a unit on a vanished volunteer is always reclaimed (this is a *deadline*, not a lease, so it is intentionally not bound by the 1800s stale-volunteer threshold). The value is stamped into each no-deadline unit's `deadline_seconds` **at generation time**, so lowering it tightens reclaim for newly generated units; units generated under the old value keep it. Lower it if you need tighter reclaim for no-deadline work; or set a real deadline on the leaf. |

> **"Inactive" ≠ "gone" for long-run volunteers.** With per-task heartbeats removed,
> a volunteer steadily running long buffered units may go more than 30 minutes
> without any RPC and show as `inactive` in the dashboard while perfectly healthy.
> This is cosmetic — `inactive` does **not** stop a volunteer from being handed work.

#### Redundancy and the dispatch cache

> **Redundancy > 1 dispatches the same work unit to N distinct volunteers in
> parallel.** A unit with `redundancy_factor: N` is handed to up to N different
> volunteers at once — each gets its own independent copy with its own lease and
> deadline, and all N run **simultaneously**. The head never hands two copies of the
> same unit to the same volunteer. As copies come back, their outputs are cross-checked
> (per `comparison_mode`) and the unit validates once `agreement_threshold` of them
> agree (e.g. `redundancy_factor: 3` + `agreement_threshold: 0.67` ⇒ 2 of 3 must match).
>
> Each dispatched copy is its own row in the dispatch ledger, so the unit stays
> dispatchable until N copies are out — the cache stages it to several distinct holders
> from a single ready snapshot. (Volunteers buffer work, so you'll typically see the N
> copies fan out as different volunteers poll, within a refill cycle of each other.)
>
> If a copy **times out** (its volunteer never returns it by the deadline), that copy
> is dropped and the unit immediately dispatches a **fresh copy to another volunteer** —
> with **no per-attempt retry cap**. The only terminal stop is a dead-letter ceiling
> (`max_total_copies`, currently auto-derived to `redundancy_factor + 6`): a unit that
> can never be completed is parked `FAILED` + flagged for review rather than retried
> forever. A volunteer whose copy just timed out is briefly benched so a fresh
> volunteer gets first refusal, then becomes eligible again if the pool is otherwise
> exhausted (so work never strands).

### Account standing backpressure

Each **volunteer account** carries a **standing** that governs how the head treats its
work. `OK` is normal service. **`PROBATION`** keeps the account in rotation — it is still
dispatched work and still credited — but its results never count toward a work unit's
agreement and never cover redundancy, so the head forces full replication around it.
**`BENCHED`** stops dispatch to that account entirely until its bench expires (an expired
bench drops back to `PROBATION`, not straight to `OK`). Operators can set any standing by
hand through the admin API, and hand-set standings are never changed automatically.

Turned on, the **backpressure machine** sets these standings for you: it folds every
**adjudicated** result — one the head accepted (`AGREED`) or rejected (`DISAGREED`) after
comparing the redundant copies of a unit — into a decayed per-account rejection rate
(7-day half-life) and moves the account between `OK`, `PROBATION`, and `BENCHED` with
hysteresis (separate entry and exit rates, so an account near a threshold does not flap).
It is **off by default**: with it off, no signal is recorded and standing stays
operator-only. Before enabling, watch the head's existing per-volunteer
`volunteer rejection rate` WARN lines to learn your fleet's normal rejection rates, then
calibrate the thresholds (effective rates must order `0 < ok_rate < probation_rate <=
bench_rate <= 1`). Once enabled, the head also logs a throttled
`automatic standing backpressure` WARN naming how many accounts the machine currently
holds benched versus on probation. Inspect the current non-OK population with
`GET /api/v1/admin/standing` and release an account with
`POST /api/v1/admin/standing/clear`.

These are `head.*` keys in `lettuce.yaml` (or the matching `LETTUCE_HEAD_*` env vars);
all are OPTIONAL and take the defaults below when unset.

| Key (env) | Default | What it does |
|-----------|---------|--------------|
| `standing_backpressure_enabled` (`LETTUCE_HEAD_STANDING_BACKPRESSURE_ENABLED`) | `false` | Master switch. Off records no signal and leaves standing operator-only. Enable only after observing the WARN rates above. |
| `standing_probation_rate` (`LETTUCE_HEAD_STANDING_PROBATION_RATE`) | `0.50` | Decayed rejection rate at which an `OK` account enters `PROBATION`. |
| `standing_ok_rate` (`LETTUCE_HEAD_STANDING_OK_RATE`) | `0.25` | Decayed rejection rate at or below which a `PROBATION` account returns to `OK` — the hysteresis exit, kept strictly below the `PROBATION` entry rate. |
| `standing_bench_rate` (`LETTUCE_HEAD_STANDING_BENCH_RATE`) | `0.75` | Decayed rejection rate at which a `PROBATION` account is `BENCHED`. |
| `standing_min_sample` (`LETTUCE_HEAD_STANDING_MIN_SAMPLE`) | `5` | Minimum decayed sample (good + bad adjudications) before any transition is evaluated, so a newcomer's first unlucky results cannot bench them. |
| `standing_bench_minutes` (`LETTUCE_HEAD_STANDING_BENCH_MINUTES`) | `1440` (24h) | Auto-bench duration. An expired bench resolves to `PROBATION`, so re-entry to `OK` still goes through the hysteresis exit. |

### Registration creation cap

A **volunteer** joins a head by registering an Ed25519 keypair, which creates one volunteer
account. The **registration creation cap** bounds how many *new* accounts a single network may
create per day — counted per network, where a network is one IPv4 address or one IPv6 `/64`
prefix, and reset at each UTC midnight. It applies to **both** registration paths: the
`lettuce-volunteer` client and the browser sign-up flow. The count is charged only when a
genuinely new account is created; a volunteer whose key already exists re-registers freely and
never touches the limit.

It is **not** a hard security boundary. Someone determined to mint many identities can rotate
IP addresses (a VPN, a fresh mobile network) to land in a new bucket, so treat the cap as a
**rate-limiter on identity minting** — it raises the cost of bulk account creation without ever
touching your legitimate fleet. Already-registered volunteers are unaffected no matter how the
cap is set, so turning it on cannot lock out existing contributors.

When a network reaches the limit, the head refuses further *new* registrations from it until
the next UTC day. The `lettuce-volunteer` client surfaces this as a clear
`daily volunteer registration limit reached` failure for that server, and the browser sign-up
form shows the same message; in both cases the volunteer can simply try again later. Refused
attempts leave no trace — the counter records only registrations that actually created an
account. The per-network counter rows are retained for **7 days** (useful when investigating
which networks were creating accounts) and swept automatically, so the table does not grow
unbounded. The cap is **off by default**; enable it by setting the two variables below and
restarting the head.

These are `head.*` keys in `lettuce.yaml` (or the matching `LETTUCE_HEAD_*` env vars);
both are OPTIONAL and take the defaults below when unset.

| Key (env) | Default | What it does |
|-----------|---------|--------------|
| `registration_cap_enabled` (`LETTUCE_HEAD_REGISTRATION_CAP_ENABLED`) | `false` | Master switch. Off leaves registration unbounded (prior behavior). Enable to bound new-account creation per network on both the CLI and browser paths. |
| `registration_cap_per_ip_per_day` (`LETTUCE_HEAD_REGISTRATION_CAP_PER_IP_PER_DAY`) | `10` | Maximum new volunteer accounts one network (IPv4 address / IPv6 `/64` prefix) may create per UTC day. Raise it for sites behind one shared NAT — labs, campuses, offices — where many honest volunteers register from a single address. |

### Registration proof-of-work

**Registration proof-of-work** is a second, independent brake on bulk identity minting,
layered beneath the account trust gate. When enforcement is on, a client that wants to
register a *new* volunteer account first fetches a short-lived **challenge** from the head,
then searches for a **nonce** whose SHA-256 digest — computed over the challenge, the
volunteer's public key, and that nonce — has at least a configured number of leading zero bits
(the *difficulty target*). Finding such a nonce takes work that grows exponentially with the
difficulty (roughly `2^bits` hash attempts on average), while the head verifies a submitted
solution in a single hash. The client submits the winning nonce alongside its registration;
each challenge is **single-use** and **expires** after a TTL, so a solved challenge can be
neither stockpiled nor replayed.

Like the creation cap above, it charges only registrations that would **create a brand-new
account**, and it applies to **both** registration paths — the `lettuce-volunteer` client and
the browser sign-up flow. A volunteer whose key already exists re-registers freely and never
solves a puzzle, so turning enforcement on never affects your existing fleet.

It is **not** a hard security boundary. A determined attacker with GPUs can solve challenges
for pennies per identity, so proof-of-work only raises the *cost* of mass account creation — a
treadmill slower, best understood as one cheap layer beneath the trust gate rather than a wall.
Its value is making automated bulk registration uneconomical, not impossible.

> **⚠️ Do not enable enforcement yet.** No shipped client can solve a challenge: neither the
> `lettuce-volunteer` client nor the dashboard sign-up flow includes a solver at this time, so
> turning `registration_pow_enabled` on today would reject **every new-volunteer
> registration** and block all onboarding. Existing volunteers are unaffected. Leave it off
> until a solver-capable `lettuce-volunteer` release **and** a dashboard deploy exist. Note
> that **challenge issuance is always available** even while enforcement is off (so a future
> client can be written without probing for support), and **expired challenges are swept
> automatically**, so the challenge table never grows unbounded.

These are `head.*` keys in `lettuce.yaml` (or the matching `LETTUCE_HEAD_*` env vars);
all are OPTIONAL and take the defaults below when unset.

| Key (env) | Default | What it does |
|-----------|---------|--------------|
| `registration_pow_enabled` (`LETTUCE_HEAD_REGISTRATION_POW_ENABLED`) | `false` | Master switch for ENFORCEMENT. Off leaves new registrations unchallenged (challenge issuance stays available either way). Do NOT enable until a solver-capable volunteer client and dashboard ship — see the warning above. |
| `registration_pow_difficulty_bits` (`LETTUCE_HEAD_REGISTRATION_POW_DIFFICULTY_BITS`) | `20` | Required leading zero bits of the solution digest (~2^20 ≈ 1M hash attempts, about a second of native single-thread work). Must be in `[8, 32]` — below 8 the puzzle is effectively free, above 32 the expected client work stretches to minutes-to-hours. |
| `registration_pow_challenge_ttl_seconds` (`LETTUCE_HEAD_REGISTRATION_POW_CHALLENGE_TTL_SECONDS`) | `600` (10m) | How long an issued challenge stays redeemable. Must be `>= 60` so a slow browser or a loaded machine can still solve and submit within the window. |

### Result-audit enforcement (advanced, default off)

Heads can re-execute a random sample of validated work on operator-vetted **trusted
runner** machines (`LETTUCE_HEAD_RESULT_AUDIT_ENABLED`; runners are registered via the
admin API and run the `lettuce-volunteer audit-runner` subcommand). By default a mismatch
between a runner's output and the accepted result is only recorded and logged.
Setting `LETTUCE_HEAD_AUDIT_ENFORCEMENT_ENABLED=true` arms the consequences: after a
**second, different** runner independently reproduces the mismatch (and the two runners'
outputs agree with each other), the head zeroes the trust of every account that backed the
wrong output, claws back the unit's credit plus all of those accounts' still-immature
credit (publishing signed revocation attestations), restores credit and trust to any
volunteer whose dissenting result matches the re-executed ground truth, and re-queues the
unit if nothing on it was right. Operational notes:

- The head **refuses to boot** with enforcement on unless `LETTUCE_HEAD_CREDIT_MATURATION_DAYS`
  is greater than 9 — the clawback must be able to land before credit matures out of the
  settlement window. Set it to 10 or more.
- Enforcement liveness wants at least **two registered runners on genuinely independent
  machines** (three or more, spanning two hardware classes, is the robust setup). With one
  runner, mismatches park in a `STALLED` state and page the log — you can still act
  manually with the admin trust-slash and credit-clawback endpoints.
- Watch the log for `CONTRADICTED`: two of your trusted runners disagree about ground
  truth. One of them is broken or compromised, or the leaf is not deterministic —
  investigate both before trusting either again. `GET /api/v1/admin/audit/flagged-leaves`
  lists the leaves with enforcement history.
- Verdicts recorded while enforcement was off are never acted on retroactively; only
  mismatches observed while the switch is on can trigger consequences.

### External output verification (advanced, default off)

A leaf can opt in to accepting results as an **external reference** — a URL to output
stored elsewhere — instead of inline bytes (`allow_external_output` plus a required
`external_output_hosts` allowlist in the leaf's validation config). The head never trusts
a volunteer's claimed checksum for such a result: the submission is held out of
validation while the head fetches the URL itself (https only, exact-host allowlist, no
redirects, no proxy) and hashes the served bytes, and only that head-computed hash can
ever count toward agreement. Two knobs govern the pipeline:

| Key (env) | Default | What it does |
|-----------|---------|--------------|
| `content_fetch_enabled` (`LETTUCE_HEAD_CONTENT_FETCH_ENABLED`) | `false` | Master switch. Off refuses every external-reference submission at the front door — even for opted-in leaves — so nothing is ever held or fetched. On accepts references on opted-in leaves, holds each one, and verifies it within roughly one worker tick (30 s). |
| `content_fetch_max_bytes` (`LETTUCE_HEAD_CONTENT_FETCH_MAX_BYTES`) | `0` → 100 MB | Global ceiling on how many bytes one verification fetch will read. The effective per-fetch cap is the smaller of this and the leaf's `max_output_size_bytes`. A body over the cap fails the result's verification. |

Operational notes:

- A held result frees its redundancy slot while it waits, so the unit may be dispatched
  to one extra volunteer during the verification window — deliberate, bounded, and
  visible in the log.
- Results whose fetch fails (unreachable origin, non-200, over the byte cap, redirect,
  disallowed address, or a URL that no longer passes the leaf's CURRENT allowlist) end
  as `CONTENT_VERIFICATION_FAILED` — permanently non-votable, reason-coded in the row's
  `content_fetch_last_error`, and queryable via
  `?validation_status=CONTENT_VERIFICATION_FAILED` on the leaf results endpoint.
- Flipping the switch off with references still held is safe: they stop being fetched
  and drain through a 24-hour holding-expiry lane (the log warns while any are waiting).
- A served hash that differs from the volunteer's claim is NOT treated as fraud — the
  result simply votes on what the origin actually served, and wrong content loses the
  ordinary agreement vote. Origins that transform bytes in flight (compression,
  re-serialization) will therefore produce disagreeing results; point the allowlist at
  storage that serves the exact uploaded bytes.

### Server-issued host identity & per-account host cap

A **volunteer account** is a volunteer's Ed25519 keypair — credit, trust, result
validation, and distinctness all key on it. Separately, the head labels each physical
**machine** behind that account with a **host id**. The host id exists only so the head
can meter and pace *per machine*: how much work a given machine holds in flight, its
work-send floor, its reliability-weighted work budget, and which machine produced a
given result. A user's beefy rig and their laptop, both attached under one account
keypair, get their own independent work budgets through their distinct host ids, while
credit still pools to the one account. **A host id is never an identity, credit, or trust
boundary** — nothing about result validation, agreement, or distinctness ever keys on it.
That separation is deliberate and load-bearing: it lets the host cap below bound abuse
without ever touching how results are judged.

**The head issues host ids; clients no longer make their own.** When a volunteer
registers, the head mints a fresh random host id for that machine, returns it in the
registration response, and the client stores it (per head) and echoes it on every later
registration to that head. A machine that presents an id the head already issued to its
account keeps that id; a machine that presents an **empty** id is asking to be minted a
new one; a machine that presents an id the head does **not** recognize is answered with
an empty id and re-registers to mint a fresh one. Running **with no host id is always
valid** — browser volunteers, and any machine refused an id (below), simply share one
per-account fallback bucket for metering.

**The cap bounds how many host ids one account may hold.** Left alone the head allows
**10** host ids per account. This is a hard bound on the *total*: an account can never
have more than the cap machines metered independently at once (plus the one shared
fallback bucket). When a machine asks for a new id and the account is already at the cap,
the head first tries to reclaim a slot from a **genuinely idle** machine — one whose host
id has gone unseen longer than the activity window (default **30 days**) — and gives the
slot to the newcomer. Actively working machines refresh their last-seen on the work path
(at most once every few minutes), so a working machine is **never** evicted. If every
slot is held by a recently active machine, the mint is **refused**: registration still
succeeds, the machine simply runs under the shared per-account fallback bucket, and the
head emits a sampled `refusing host id mint: per-account host cap reached` WARN naming
the account. Nothing is rejected or lost — the refused machine keeps computing and
earning credit; it just shares one metering bucket with the account's other host-less
workers.

> **Unlike most knobs on this page, the cap is ON by default.** Server-issued host
> identity shipped as a hard cutover (see the compatibility note below), so a sensible
> default ships with it rather than off. Set `host_cap_per_account` to `0` to disable the
> cap entirely — host ids are still server-issued, there is just no ceiling on how many an
> account may hold.

These are `head.*` keys in `lettuce.yaml` (or the matching `LETTUCE_HEAD_*` env vars);
both are OPTIONAL and take the defaults below when unset.

| Key (env) | Default | What it does |
|-----------|---------|--------------|
| `host_cap_per_account` (`LETTUCE_HEAD_HOST_CAP_PER_ACCOUNT`) | `10` (**on**) | Hard bound on how many server-issued host ids one account may hold at once. Reached only when every slot is held by a recently active machine; further machines then run under the shared per-account bucket (not rejected). `0` disables the cap (ids stay server-issued, no ceiling). |
| `host_cap_active_days` (`LETTUCE_HEAD_HOST_CAP_ACTIVE_DAYS`) | `30` | How many days a host id may go unseen before it becomes evictable to free a slot for a new machine when the account is at the cap. Actively working machines refresh their last-seen on the work path, so only genuinely idle machines ever age out — and only when a new machine actually needs the slot. |

**Revoking a machine's host id.** There is no admin API for this in this release; revoke
by deleting the row directly:

```bash
docker compose -f compose.production.yaml exec postgres \
  psql -U lettuce lettuce -c "DELETE FROM hosts WHERE id = '<host-id>';"
```

The deletion takes effect on the work-request hot path within about **30 seconds** (the
head caches host-ownership facts for that long). After that the revoked machine's next
work request is refused; an up-to-date volunteer client notices, discards the stale id,
re-registers to mint a **fresh** one (subject to the cap), and resumes — so you can free a
specific id without stopping the machine. Deleting a host row is safe for accounting:
credit and result history key on the account, not the host id, so nothing already earned
is lost.

> **⚠️ Compatibility cutover — volunteers must update for this release.** Server-issued
> host identity replaces the older scheme in which each client generated its own machine
> id, and there is **no backward path**: a volunteer client built before this release
> still presents a self-generated id the head never issued, so an upgraded head
> **refuses it work**. Such a client is not silently broken — at each retry it logs that
> its build is outdated and to run `lettuce-volunteer update`, then backs off and retries
> — but it does **no work** until updated. When you upgrade the head, tell your
> volunteers to run `lettuce-volunteer update` and restart. Updated clients re-register,
> receive an issued id automatically, and continue with no manual steps.

### Horizontal scale-out

The head is **stateless** and can run as **N replicas** behind Caddy against one
shared Postgres. The replica count is a single number.

```bash
# In .env:
HEAD_REPLICAS=2          # run two head replicas (default 1)

docker compose -f compose.production.yaml up -d --build --scale infrastructure=2
# podman: podman-compose -f compose.production.yaml up -d --build --scale infrastructure=2
```

> **⚠️ podman: use `--scale`, not `HEAD_REPLICAS`.** `podman-compose` **ignores** the
> `deploy.replicas` key (verified on podman-compose 1.6.0), so the `HEAD_REPLICAS`
> value above silently runs only **one** replica. Always pass `--scale infrastructure=N`
> on the `up` command. (Docker Compose v2 honors `HEAD_REPLICAS` via `deploy.replicas`;
> `--scale` works on both, so prefer it.)

That's the whole change. Caddy fans out volunteer gRPC and the REST API across all
replicas automatically (dynamic upstreams), so you never edit the `Caddyfile` to
scale. Each replica:

- Auto-generates a **distinct head instance id** at boot (do **not** pin
  `LETTUCE_HEAD_INSTANCE_ID` — a shared id collides dispatch-claim ownership). The
  id appears as a log field, so you can confirm both replicas are receiving traffic:

  ```bash
  docker compose -f compose.production.yaml logs infrastructure | grep instance_id
  ```

- **Claims the work units it stages** (claim-on-refill): a unit held in one
  replica's memory is invisible to every other replica's refiller, so **no unit is
  handed to two volunteers** across replicas. A crashed replica's claims expire and
  any survivor re-claims the units on its next refill.
- Shares the **replay store** and **rate-limit buckets** through the bundled
  `redis` service, so a captured signed request replayed to a *different* replica is
  rejected, and each client gets its intended rate budget (not N× it).
- Contends for an **advisory-lock leadership**: exactly one replica (the leader)
  runs the singleton background jobs (lazy work-unit generation, health metrics, the
  fault-monitor sweep + reclaim). If the leader crashes, a follower takes over within
  ~15 seconds.

#### What the `redis` service is for

Running **more than one replica** **requires** the `redis` service (it is started for you
in `compose.production.yaml`). It backs two cross-replica concerns:

- **Anti-replay.** Each authenticated request carries a one-time signature. With one
  head, an in-process cache rejects a replay; with N heads that cache must be shared,
  or a captured request replayed to a second replica would slip through. Redis holds
  the shared, global signature dedup (keyed on the signature alone), TTL = the 5-minute
  signature-skew window.
- **Rate-limit fairness.** Per-IP and per-pubkey budgets become global counters, so a
  client does not get N× its budget by hitting different replicas.

**Failure policy (default fail-open).** If Redis is briefly unreachable, the head
**admits** the request and logs a loud error rather than rejecting authenticated
traffic — a Redis blip never takes the whole fleet offline or drops completed compute
on `SubmitResult`. Set `LETTUCE_REPLAY_FAIL_MODE=closed` to flip to strict rejection
if you run adversarial workloads. Run Redis with `restart: unless-stopped` (already
set). A single-replica deploy that leaves `LETTUCE_REDIS_URL` empty never touches this
path and keeps the in-process cache.

**Authentication.** The bundled Redis requires the `REDIS_PASSWORD` from `.env`
(`--requirepass`), and the head's `LETTUCE_REDIS_URL` carries it. This is
defense-in-depth for the private compose network: a compromised neighboring
container can no longer read or poison the replay/rate-limit store anonymously.

**Fail closed when scaled out.** A replica cannot see `HEAD_REPLICAS`, so a fleet
whose Redis configuration is lost would otherwise degrade *silently* to
per-replica replay/rate-limit state (a replayed signature then passes the other
replicas). Whenever you run more than one replica, also set:

```bash
LETTUCE_HEAD_REQUIRE_SHARED_STORE=1
```

in `.env` — the head then refuses to boot with no `LETTUCE_REDIS_URL` configured
(a configured-but-unreachable Redis is always fatal at boot regardless). With a
single replica, leave it unset; the head just logs a warning when no Redis is
configured.

#### Client IP behind the proxy

The pre-auth per-IP rate limiter must see the **real** client IP, not Caddy's. The
bundled `compose.production.yaml` already sets `LETTUCE_TRUSTED_PROXIES` to the Docker
network and Caddy forwards the client IP, so this works out of the box. If you front
the head with a *different* proxy, set `LETTUCE_TRUSTED_PROXIES` to that proxy's network
and make it forward the client IP — otherwise every client collapses into one bucket
keyed on the proxy IP and the whole fleet is throttled together.

#### Honest limitations

- **Rate-limit window.** Cross-replica budgets use a fixed window, which permits up to
  2× the limit across a window boundary. This is a DoS backstop, not a security
  boundary; the burst is accepted.
- **Per-volunteer inflight cap** can transiently over-admit across replicas (a volunteer
  may briefly hold a few more units than the configured max while replicas reconcile).
  It self-corrects within the ~30s reconcile and never corrupts or strands work.
- **Leader-failover reclaim pause.** During the ≤15s window after a leader crash, the
  singleton reclaim/sweep jobs pause. This is bounded and well below typical copy
  deadlines, so it only delays — never breaks — reclaim. Passive re-claim of
  crashed-replica dispatch claims needs no leader and is unaffected.

### Back up

Two things need backing up, and they are not equivalent. The database can be dumped
and restored routinely; the signing key is a single irreplaceable file.

```bash
# Database
docker compose -f compose.production.yaml exec postgres \
  pg_dump -U lettuce lettuce > backup.sql
```

#### Signing key — backup & restore

`keys/signing.key` is the head's Ed25519 attestation signing key. It is the **one
file whose loss cannot be recovered from a database backup**: it defines the head's
signer identity, and consumers verify every credit attestation against its public
half.

**Back it up.** Copy `keys/signing.key` to **offline, encrypted media or a secrets
manager**, stored at file mode `0600`. Never commit it to the repository and never
bake it into an image.

**Record its public identity (verify step).** Before you file the backup — and again
after any restore — print the public key and keep the output:

```bash
openssl pkey -in keys/signing.key -pubout
```

That printed public key **is** the head's signer identity. If the value after a
restore is byte-identical to the value you recorded before the backup, the correct
key is in place; if it differs, you have restored the wrong file.

**Restore on a rebuilt host.** After provisioning a fresh server and cloning the repo:

1. Place the backed-up file at `keys/signing.key`, next to `compose.production.yaml`.
2. Fix ownership and mode (the head enforces both at boot):

   ```bash
   sudo chown 10001:10001 keys/signing.key && chmod 600 keys/signing.key
   ```

3. Bring the stack up:

   ```bash
   docker compose -f compose.production.yaml up -d
   ```

4. Confirm the head loaded the restored key — look for this line in the log:

   ```bash
   docker compose -f compose.production.yaml logs infrastructure | grep 'attestation signing key loaded'
   ```

5. Run the verify step above and confirm the printed public key matches what you
   recorded before the backup.

**If you lose this key, it is gone.** The head deliberately **never auto-regenerates
the signing key in production** — it fails to boot on a missing key rather than
quietly minting a new one. Recreating the key produces a **new signer identity**:
every attestation signed before the loss stops verifying against the new key, and
there is no way to reconstruct the old one. That is exactly why this file is backed up
separately from the database.

### TLS renewal

Automatic. Caddy renews certificates before they expire; no action needed.

---

## Troubleshooting

| Symptom | Likely cause / fix |
|---------|--------------------|
| TLS certificate errors on first start | DNS not yet propagated, or ports 80/443 blocked. Confirm `dig +short your-domain.com` returns your IP and the firewall allows 80 + 443. Both `your-domain.com` and `viz.your-domain.com` must resolve. |
| `caddy` container won't start | `REGISTRY_PASS_HASH` is empty or malformed. Regenerate it with `caddy hash-password` (Step 6) and paste the full hash. |
| Build killed / out of memory | Build images one at a time (Step 9, 1 GB path), or use a larger server. |
| `health` returns `"database":"disconnected"` | Postgres still starting, or `POSTGRES_PASSWORD` contains `/` or `@`. Check `docker compose -f compose.production.yaml logs postgres`. |
| Can't sign in to the dashboard | Check the bootstrap log lines from Step 10. Bootstrap only runs when no admin exists; to reset, change the password from the dashboard once signed in. |
| `infrastructure` exits with "signing key file ... does not exist" | You skipped Step 8 or the `keys/signing.key` path is wrong. Generate the key (`openssl genpkey -algorithm ed25519 -out keys/signing.key`) so it lands next to `compose.production.yaml`, then restart. The server fails closed on a missing key by design. |
| `infrastructure` exits with "signing key file ... insecure permissions" or "not owned by" | The key's host permissions drifted (it is group/other-readable, or owned by a uid other than 10001). The head enforces the key's ownership and mode at boot by design. Run `sudo chown 10001:10001 keys/signing.key && chmod 600 keys/signing.key` and restart. |
| `infrastructure` exits with `boot secret validation: <VARIABLE>` | That `.env` entry is still a placeholder or is under its length floor. Generate a real value with the generator named in the error (`openssl rand -base64 32`, or `-hex 32` for a URL-embedded secret), update `.env`, and `up -d` again. Note: changing `DASHBOARD_API_KEY` re-mints the dashboard key on the next boot, and `LETTUCE_ADMIN_PASSWORD` applies only to a not-yet-created admin. |
| `infrastructure` exits with `refusing to start: admin user ... placeholder password` | The admin row in Postgres still carries the publicly-known placeholder password from a first boot with an unedited `.env`. Rotate it without the head: generate a bcrypt hash with `docker run --rm caddy:2-alpine caddy hash-password --plaintext 'your-new-strong-password'`, then `docker compose -f compose.production.yaml exec postgres psql -U lettuce -d lettuce -c "UPDATE users SET password_hash='<the-hash>' WHERE role='ADMIN' AND email='<your-admin-email>';"`, then start the head and sign in with the new password. |
| `dashboard` container exits immediately with `boot secret validation: NEXTAUTH_SECRET` | Under `NODE_ENV=production` the dashboard refuses to start when `NEXTAUTH_SECRET` is missing, a placeholder, or shorter than 32 characters. Generate one with `openssl rand -base64 32`, put it in `.env`, and `up -d dashboard`. |
| Browser console: `No 'Access-Control-Allow-Origin' header` on API calls | `LETTUCE_CORS_ORIGINS` is empty. By design it now fails closed — set it to your `PLATFORM_URL` (already pre-filled in `.env.example`) and restart `infrastructure`. In the bundled deploy the dashboard and `/api/v1/*` share the same origin, so CORS is only required if a different host (e.g. `viz.your-domain.com` or a separate admin UI) calls the API from a browser. |
| Rate-limit responses count all requests as one client | `LETTUCE_TRUSTED_PROXIES` is unset or wrong. The bundled `compose.production.yaml` already trusts Docker/RFC1918 ranges so per-client limiting works behind Caddy — only set this in `.env` if your reverse proxy sits on a different network. |
