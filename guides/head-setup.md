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

A production head is five containers behind one domain, all traffic on port 443:

| Container | What it does |
|-----------|--------------|
| **postgres** | PostgreSQL database — stores leafs, work units, results, volunteers |
| **infrastructure** | Go server — task distribution, result validation, credit tracking |
| **dashboard** | Next.js web app — public leaf browser + admin console |
| **registry** | OCI image registry — hosts container images for container leafs |
| **caddy** | Reverse proxy — automatic HTTPS, routes everything on port 443 |

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

Tear down when you're done:

```bash
docker compose down       # stop, keep the database
docker compose down -v    # stop and wipe all data
```

> The `make` targets (`make up`, `make down`, `make logs`, `make reset`, `make rebuild`)
> are shortcuts for this **development** stack. Production always uses the explicit
> `-f compose.production.yaml` flag shown below.

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
| `PLATFORM_URL` | Your full public URL, with `https://`. Used for auth callbacks and the head's advertised URL. |
| `NEXTAUTH_SECRET` | Signs dashboard session cookies. |
| `LETTUCE_ADMIN_API_KEY` | Bootstrap key for authenticated API calls. **Save it** — you'll need it to create leafs. |
| `DASHBOARD_API_KEY` | The key the dashboard uses to talk to the infrastructure server. |
| `LETTUCE_ADMIN_EMAIL` / `LETTUCE_ADMIN_PASSWORD` | Dashboard admin account, created automatically on first boot. The password is bcrypt-hashed for you. |
| `LETTUCE_HEAD_NAME` / `LETTUCE_HEAD_DESCRIPTION` | What volunteers see for this head. |
| `LETTUCE_CORS_ORIGINS` | Allowed browser origins (your domain). |
| `LETTUCE_GRPC_PER_IP_RATE_LIMIT` | *(optional)* Per-source-IP gRPC request budget, **requests per minute** (default 60). Raise this when a whole fleet legitimately shares one source IP — e.g. many volunteers behind a single NAT, or a load test from one host — so the shared per-IP bucket does not throttle the fleet to ~1 req/s. Combine with `LETTUCE_TRUSTED_PROXIES` so volunteers behind your reverse proxy are still bucketed per real client IP. |
| `LETTUCE_GRPC_PER_PUBKEY_RATE_LIMIT` | *(optional)* Per-authenticated-volunteer gRPC request budget, **requests per minute** (default 120), keyed on the volunteer's verified Ed25519 key. This limiter sits *after* auth, so it sheds database/handler load but not signature-verification cost (the per-IP limiter is the only pre-auth, crypto-shedding ceiling). |
| `VIZ_ORIGIN` | The `viz.` subdomain, for visualization isolation. **Required in production** — it binds the viz-bundle route to this origin so author bundle code only runs in the sandboxed viz origin, never on your main app origin. |
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
key file is missing, so it can never quietly mint a new signing identity.

```bash
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/signing.key
```

This writes a PKCS#8 PEM file, which is exactly the format the server reads. The
production compose file mounts `./keys` read-only at `/keys` and loads
`/keys/signing.key`.

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

---

## Operations

### Logs

```bash
docker compose -f compose.production.yaml logs -f                 # all services
docker compose -f compose.production.yaml logs -f infrastructure  # one service
```

### Update to the latest version

```bash
git pull
docker compose -f compose.production.yaml build infrastructure
docker compose -f compose.production.yaml build dashboard
docker compose -f compose.production.yaml up -d
```

Migrations run automatically on startup. This release adds a migration
(`00002_work_unit_reservations`) that adds nullable `reserved_until` /
`reserved_volunteer_id` columns to `work_units`; it applies automatically and
needs no data migration. A reserved (buffered) unit stays `QUEUED` with
`reserved_until > now()` and is invisible to deadline/abandonment reclaim until
the volunteer actually starts running it.

> **Breaking release — head and all volunteers update together.** This release
> redesigns the volunteer⇄head work protocol (server-directed retry delay, work
> batching, leased buffered work). **A volunteer older than this release cannot
> talk to the new head.** Redeploy the head first, then update every volunteer
> binary (and the desktop-app sidecar). See the volunteer setup guide for the
> volunteer-side note.

### Work dispatch tuning

The head paces volunteers and hands out work in batches so a large fleet creates
far less request noise. These are tuned with `head.*` keys in `lettuce.yaml`
(or the matching `LETTUCE_HEAD_*` env vars). Defaults are sane for a small head;
the only one you should actively calibrate is `target_request_rate_per_sec`.

| Key (env) | Default | What it does |
|-----------|---------|--------------|
| `max_batch_per_request` (`LETTUCE_HEAD_MAX_BATCH_PER_REQUEST`) | `8` | Max work units one work request may return (the server-side batch cap). |
| `max_inflight_per_volunteer` (`LETTUCE_HEAD_MAX_INFLIGHT_PER_VOLUNTEER`) | `10` | Max units (running + buffered/reserved) one volunteer may hold. Also caps how deep a volunteer's hours-based work buffer can fill. |
| `min_retry_delay_seconds` (`LETTUCE_HEAD_MIN_RETRY_DELAY_SECONDS`) | `30` | Server-directed retry delay handed out when quiet. Stamped on **every** reply (including no-work); volunteers must obey it. |
| `max_retry_delay_seconds` (`LETTUCE_HEAD_MAX_RETRY_DELAY_SECONDS`) | `900` | Retry delay under full load. Must stay below the 1800s stale-volunteer threshold (validated at startup). |
| `retry_delay_jitter_pct` (`LETTUCE_HEAD_RETRY_DELAY_JITTER_PCT`) | `0.20` | Server-side ± jitter on the stamped delay so a fleet does not re-contact in lockstep. |
| `target_request_rate_per_sec` (`LETTUCE_HEAD_TARGET_REQUEST_RATE_PER_SEC`) | `500` | Per-head work-request rate the load estimator treats as "fully loaded". **Not calibrated** — measure your single-head dispatch ceiling with `swarm-sim` (see `CONTRIBUTING.md`) and set this to it. The 2026-06-01 reference run measured ~240 assignments/sec on a single head, well below the default. |
| `lease_seconds` (`LETTUCE_HEAD_LEASE_SECONDS`) | `900` | How long a buffered/reserved unit is held for a volunteer before the head may reclaim it. Must stay below 1800s. |

`LETTUCE_TRUSTED_PROXIES` also governs **per-client rate limiting** on the gRPC
port: with it set, volunteers behind your reverse proxy are bucketed per real
client IP (and per authenticated key) rather than sharing one proxy-IP bucket.
The per-pubkey limiter sits *after* auth, so it does not shed
signature-verification cost — the per-IP ceiling is the only pre-auth layer.

### Back up

```bash
# Database
docker compose -f compose.production.yaml exec postgres \
  pg_dump -U lettuce lettuce > backup.sql

# Signing key — store keys/signing.key somewhere safe and private
```

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
| Browser console: `No 'Access-Control-Allow-Origin' header` on API calls | `LETTUCE_CORS_ORIGINS` is empty. By design it now fails closed — set it to your `PLATFORM_URL` (already pre-filled in `.env.example`) and restart `infrastructure`. In the bundled deploy the dashboard and `/api/v1/*` share the same origin, so CORS is only required if a different host (e.g. `viz.your-domain.com` or a separate admin UI) calls the API from a browser. |
| Rate-limit responses count all requests as one client | `LETTUCE_TRUSTED_PROXIES` is unset or wrong. The bundled `compose.production.yaml` already trusts Docker/RFC1918 ranges so per-client limiting works behind Caddy — only set this in `.env` if your reverse proxy sits on a different network. |
