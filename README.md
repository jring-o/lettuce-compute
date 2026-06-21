# Lettuce

A distributed volunteer compute platform. Researchers run a server (a **head**) and define computations (**leafs**); volunteers donate CPU time to run them. Cryptographic credit tracking built in.

**Status: Alpha** — looking for testers.

## How it works

Someone runs a Lettuce server — a **head** — and creates one or more **leafs** (computational projects). **Volunteers** run the CLI/GUI, attach to one or more heads, and crunch work units. Results are validated and credit is tracked automatically.

```
Head operator                        Volunteers
    |                                    |
    |  lettuce-server (Go)               |  lettuce-volunteer (Go CLI)
    |  + PostgreSQL                      |  attaches to 1+ heads
    |  + Dashboard (Next.js)             |  downloads compute binary
    |                                    |  executes work, submits results
    |  REST API + gRPC                   |
    |<---------------------------------->|
```

## Volunteer quick start

1. Download `lettuce-volunteer` for your platform from [Releases](https://github.com/jring-o/lettuce-compute/releases)
2. Initialize:
   ```bash
   ./lettuce-volunteer init
   ```
3. Attach to a head:
   ```bash
   ./lettuce-volunteer attach --server head.example.com
   ```
4. Start computing:
   ```bash
   ./lettuce-volunteer start
   ```

You can attach to multiple heads run by different operators. The CLI distributes work across all of them — and you can steer the split with `./lettuce-volunteer heads weight` (per head) and `./lettuce-volunteer leafs weight/enable/disable` (per leaf); see [Choosing what you work on](guides/volunteer-setup.md#choosing-what-you-work-on).

Not getting work, or setting up a container runtime? Run `./lettuce-volunteer doctor`
for a pass/fail diagnosis, and see the [volunteer setup guide](guides/volunteer-setup.md)
(per-OS container setup + a "why am I getting no work?" troubleshooting table).

### Logs

Every command writes JSON logs to both stderr **and** a rotating file at
`~/.lettuce/logs/volunteer.log` (under your `--data-dir`) — no shell redirection
needed. If something goes wrong, just attach that file. The file rotates at
10 MB and keeps 5 backups, so it stays bounded even at `--log-level debug`.

Tune it in `config.yaml` (or via `lettuce-volunteer config set <key> <value>`):

| key | default | meaning |
| --- | --- | --- |
| `log_to_file` | `true` | write logs to the rotating file |
| `log_to_stderr` | `true` | write logs to stderr |
| `log_file` | `<data-dir>/logs/volunteer.log` | log file path (also `--log-file`) |
| `log_max_size_mb` | `10` | rotate after this size |
| `log_max_backups` | `5` | rotated files to keep |
| `log_max_age_days` | `0` | max age of rotated files (`0` = no limit) |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` (also `--log-level`) |

#### "Connected but getting no work?"

On startup `lettuce-volunteer start` logs a readiness line — the runtimes it can
actually run, free disk vs your `max_disk_gb`, and how many of the attached
leafs you're eligible for — and raises a `WARN` for the common silent stalls:

- **No matching runtime** — e.g. every attached leaf needs a container runtime
  but you have no Docker/Podman. The volunteer now advertises only the runtimes
  it can actually run, so it won't be handed work it can't do; install a
  container runtime or attach a head with native leafs.
- **Disk gate** — free space is below `max_disk_gb`, so no work is fetched. Free
  space, lower `resource_limits.max_disk_gb`, or point `--data-dir` at a roomier
  volume.

These show up in `volunteer.log`, so attaching that file is usually enough to
diagnose a quiet volunteer.

Update to the latest version:
```bash
./lettuce-volunteer update
```

## Run your own head

Deploy your own Lettuce server (a **head**) and launch a computation (a **leaf**).

> **Not technical? Let an agent do it.** You don't have to run any of this by hand. Open this
> repository in [Claude Code](https://claude.com/claude-code) and say *"help me deploy a
> Lettuce head."* A built-in guided skill provisions the server, deploys, and verifies
> everything with you — one step at a time, doing the technical parts itself and asking you
> only for what needs a human. Then say *"help me create my first leaf"* to get your
> computation running. Skills live in [.claude/skills/](.claude/skills/); the underlying
> step-by-step guides are [guides/head-setup.md](guides/head-setup.md) and
> [guides/first-leaf.md](guides/first-leaf.md).

### Docker Compose (recommended)

```bash
git clone https://github.com/jring-o/lettuce-compute.git
cd lettuce-compute
cp .env.example .env
# Edit .env with your values
```

Pre-generate the Ed25519 signing key (the server fails to start without it),
then bring the stack up:

```bash
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/signing.key
docker compose -f compose.production.yaml up -d
```

Caddy obtains and renews Let's Encrypt TLS certificates automatically — there's
no manual certificate step.

See the [head setup guide](guides/head-setup.md) for full instructions — DNS,
secrets, local testing, and operations.

### Write a compute binary

Your compute binary needs to:

1. Read parameters from `$LETTUCE_PARAMS_FILE`
2. Do computation
3. Write results to `$LETTUCE_OUTPUT_FILE`
4. Exit 0

Any language works. Cross-compile for the platforms your volunteers use. Native
leafs **require** a SHA-256 checksum per platform binary (`binary_checksums` in
the leaf config) — volunteers verify the download and refuse to run an
unverified or tampered binary. See [guides/first-leaf.md](guides/first-leaf.md)
for a full walkthrough and working examples.

## Architecture

```
Dashboard (Next.js :3000)            — web UI for browsing leafs + admin
        |
Infrastructure (Go :8080 :9090)      — task scheduling, validation, credit tracking
        |
PostgreSQL (:5432)                    — leafs, work units, results, volunteers
```

- **Infrastructure** is standalone and self-hostable
- **Dashboard** is optional — the REST API works without it
- **Volunteer CLI** connects directly to infrastructure via gRPC

### How work is dispatched

The head, not the volunteer, owns the conversation rate, so one head keeps up
with a large fleet:

- **Server-directed retry delay** — every work reply (including "no work right
  now") tells the volunteer how long to wait before contacting again. A quiet
  head asks volunteers back quickly; a busy head stretches the delay out, so the
  fleet's request load self-throttles instead of hammering the server.
- **Work batching + a client work buffer** — volunteers request work in batches
  and keep a buffer measured in hours. While the buffer is full they make zero
  work requests; they just run what they hold.
- **In-process dispatch cache** — work requests are served from an in-memory pool
  of queued units (no database round-trip on the hot path); reservations are
  written back to Postgres asynchronously in batches.
- **Graceful shedding** — under sustained overload the head serves from the cache
  until it empties, then returns a fast "back off" instead of letting database
  connections pile up.
- **Leased buffered work + deadline-based reassignment** — buffered units are
  *leased* (reserved), not yet started, so the head won't hand the same unit to
  anyone else; the deadline clock starts only when the unit actually runs (via an
  explicit `StartWork` step), and a unit not submitted by its deadline is
  reassigned. There are no per-task heartbeats. Buffered work is downloaded and
  prepared lazily, right before it runs.
- **Per-client rate limiting** — the gRPC port is rate-limited per real client IP
  (trust-aware behind a reverse proxy) and per authenticated volunteer key.

> **Horizontal scale-out is supported.** The head is stateless and runs as N
> replicas behind Caddy against one shared Postgres — scale with
> `--scale infrastructure=N` on the compose `up` (podman-compose ignores
> `deploy.replicas`/`HEAD_REPLICAS` — verified on podman-compose 1.6.0 — so
> `--scale` is the working knob; Docker Compose honors both). A bundled redis backs
> the cross-replica replay dedup + rate-limit buckets, and exactly one head still
> owns each work unit via claim-on-refill, so no unit is handed to two volunteers.
> See [`guides/head-setup.md`](guides/head-setup.md) → "Horizontal scale-out".

Heads and volunteers ship together as a breaking release: a volunteer older than
the head's protocol version cannot attach. Update the head first, then volunteers.

## Credit system

- Ed25519 cryptographic identity for every volunteer
- Signed credit attestations verifiable via public API
- Public JSON stats API for volunteers and leafs (current + historical)
- RAC calculation with 7-day exponential decay half-life

## Development

```bash
# Start all services locally
docker compose up

# Infrastructure health
curl http://localhost:8080/api/v1/health

# Dashboard
curl http://localhost:3000/health
```

```bash
make up       # Start in background
make logs     # View logs
make down     # Stop (preserves database)
make reset    # Stop and reset database
make rebuild  # Full rebuild (no cache)
```

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See [LICENSE](LICENSE).
