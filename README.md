# Lettuce

A distributed volunteer compute platform. Researchers run a server (a **head**) and define computations (**leafs**); volunteers donate CPU time to run them. Cryptographic credit tracking built in.

**Status: Alpha** — looking for testers.

## How it works

Someone runs a Lettuce server — a **head** — and creates one or more **leafs** (computations). **Volunteers** run the CLI, attach to one or more heads, and crunch work units. Results are validated and credit is tracked automatically.

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

You can attach to multiple heads run by different operators. The CLI distributes work across all of them.

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
