# Lettuce

Infrastructure for volunteer computing. You run the server; it answers to nobody.

You deploy your own server, a **head**, and define computations on it called **leafs**. Volunteers
anywhere donate spare CPU and GPU time to run the work. Results are cross-checked between
independent volunteers, and every credit decision is signed so it can be verified without trusting
the server that issued it.

**Status:** open beta. Anyone can deploy a head, define leafs, or volunteer. To join the private
community of builders and testers, sign up at
[scios.tech/lettuce-beta](https://www.scios.tech/lettuce-beta).

## Roles

Three roles, each independent of the others.

**Head operators** deploy a head. A head is one Postgres database, one Go server, and a reverse
proxy that obtains a TLS certificate automatically. It hosts leafs, hands out **work units**,
validates what comes back, and records credit. It runs everything it needs itself, including its own
container registry and its own signing key.

**Leaf owners** define computations on a head. A leaf is a program plus a description of how to
split it into independent work units, how to decide whether two volunteers' answers agree, and what
a completed unit is worth. Any authenticated user of a head can create and own leafs, so a head can
be one person's instance or a shared facility whose operator provisions accounts for colleagues.

**Volunteers** run the `lettuce-volunteer` client, attach to one or more heads, pick a leaf, and crunch. There
is no approval step; possession of a keypair is the whole of registration. Volunteers choose which
heads to attach to, how to split effort between them, which leafs to accept, when to run, and what
each head may execute on their machine.

Heads know nothing about each other. Credit, reputation, and standing live in one head's database,
are denominated however that operator chooses, and mean nothing anywhere else. A volunteer attached
to three heads has three unrelated relationships that share one keypair and one process.

## Let an agent do it

You do not have to run any of this by hand. Open this repository in
[Claude Code](https://claude.com/claude-code) and say what you want to do. Three guided skills ship
with the repository. Each does the technical work itself and asks you only for what needs a human,
one step at a time, checking each step before moving on.

| Say | Skill | What it does |
|---|---|---|
| *"help me design a leaf"* | `design-lettuce-leaf` | Turns a research question into a concrete computation spec, before any code is written |
| *"help me deploy a Lettuce head"* | `deploy-lettuce-head` | Provisions a server, points DNS, generates secrets, deploys, and verifies the result |
| *"help me create my first leaf"* | `create-lettuce-leaf` | Packages your code, hosts it on your head, then creates, configures, and activates the leaf |

The skills live in [.claude/skills/](.claude/skills/), and they follow the same guides you can read
yourself, [head-setup.md](guides/head-setup.md) and [first-leaf.md](guides/first-leaf.md).

## Volunteering

### In the browser

Open a head's dashboard, go to Contribute, and start computing. WebAssembly leafs run in a worker
pool in your tab, with an identity generated and held in the browser. Nothing to install. This path
covers WASM leafs only and does no checkpointing or per-machine metering, so use the client for
anything long-running.

### The client

1. Download `lettuce-volunteer` for your platform from
   [Releases](https://github.com/jring-o/lettuce-compute/releases)
2. Set up your identity and resource limits:
   ```bash
   chmod u+x lettuce-volunteer
   ./lettuce-volunteer init
   ```
3. Attach to a head. The command asks what that head may run on your machine;
   [A head is a trust domain](#a-head-is-a-trust-domain) explains the choice.
   ```bash
   ./lettuce-volunteer attach --server head.example.com
   ```
4. Start computing:
   ```bash
   ./lettuce-volunteer start
   ```

Attach to as many heads as you like. Each gets a weight, and the client keeps their shares balanced
over time; leafs are ranked the same way within a head. Weights are ratios, not caps.

```bash
./lettuce-volunteer heads weight head.example.com 200     # twice the share
./lettuce-volunteer leafs disable some-leaf               # never take this one
./lettuce-volunteer schedule set --from 20:00 --to 06:00  # overnight only
```

Not getting work? `./lettuce-volunteer doctor` gives a pass/fail diagnosis, and
`./lettuce-volunteer leafs list` shows per leaf whether this machine will fetch it and why not. The
[volunteer setup guide](guides/volunteer-setup.md) covers per-OS container setup and
troubleshooting. Update with `./lettuce-volunteer update`.

### Desktop app

A desktop application for Windows, macOS, and Linux is being built in
[`apps/volunteer-desktop`](apps/volunteer-desktop/README.md) for volunteers who would rather not
use a terminal. It wraps the client above: it starts `lettuce-volunteer` for you, walks you through
setup (on Windows, including installing a container runtime), shows what your machine is working
on, and updates itself. A head sees it as an ordinary client, because it is one. Installers will
be published as `desktop-vX.Y.Z` releases; until then, and after, the command-line client remains
the reference client, and everything the app can do, the client can do.

### A head is a trust domain

Attaching to a head means trusting the person who runs it to execute code on your machine, so the
client makes that decision explicit and records it per head. There are three runtimes.

| Runtime | What it is | Default |
|---|---|---|
| **WASM** | A sealed WebAssembly sandbox. It sees only its own work folder, not your files, network, or keys. GPU work via WebGPU. | Always allowed |
| **Container** | Docker or Podman, with all capabilities dropped, a read-only root filesystem, a non-root user, no network unless the leaf declares it, and memory, process, and disk caps. GPU via device passthrough. | Per-head opt-in |
| **Native** | The leaf's binary runs directly on your machine with no sandbox. It can read your files, including your identity key. | Off; per-head opt-in only |

```bash
./lettuce-volunteer heads trust my-head                    # show current trust
./lettuce-volunteer heads trust my-head none               # WASM sandbox only
./lettuce-volunteer heads trust my-head container          # + containers
./lettuce-volunteer heads trust my-head container,native   # + unsandboxed native
```

The decision is enforced three ways. Runtimes you have not trusted anywhere are never built, each
head is told only what it is trusted for, and a head that dispatches a unit for an untrusted runtime
has that unit refused and handed straight back. Declining everything is durable; a later upgrade
will not quietly re-grant it.

Your machine also holds the line on resources. You set CPU cores, memory, disk, GPU VRAM share, and
concurrent tasks. The client reserves exactly what it will enforce, using cgroups where available,
container limits, or WASM memory pages. It stops fetching before your disk runs low, and pauses
everything if your CPU gets too hot.

### Commands

| Command | What it does |
|---|---|
| `init` | First-run setup: identity, resource limits, optional run schedule |
| `start` / `stop` | Run or stop the daemon |
| `status` | Running tasks with progress and ETA, buffered work, credit, failing leafs |
| `doctor` | Pass/fail diagnosis of connectivity, runtimes, disk, memory, eligibility |
| `credit` | Your credit, per head and per leaf, asked of each head directly |
| `attach` / `detach` | Add or remove a head; `--leaf` pins a single leaf |
| `heads` | `list`, `weight`, `trust` |
| `leafs` | `list`, `enable`, `disable`, `weight`, `reset` |
| `schedule` | `show`, `set`, `add`, `clear` |
| `config` | Read and write settings (`config set <key> <value>`) |
| `history` | Locally recorded completed work |
| `update` | Download, verify, and install the latest release |
| `bind-did` | Optionally bind your account to an ATProto DID |
| `audit-runner` | Run audit jobs, for operator-vetted trusted runners only |

Logs go to stderr and to a rotating file at `~/.lettuce/logs/volunteer.log`. If something goes
wrong, attach that file. The `--log-level` and `--log-file` flags are one-off overrides and are
deliberately never saved; use `config set` to change them permanently.

## Running a head

Nobody's permission is required, and nothing registers your head with anyone.

The `deploy-lettuce-head` skill does everything below for you, asking only for the parts that need a
human. The manual path follows.

A head needs a domain with A records for `your-domain.com` and `viz.your-domain.com`, and a Linux
server running Ubuntu 24.04 LTS or 22.04+, with at least 2 GB of RAM and Docker installed.
Visualizations are served from the second origin so leaf-supplied code cannot reach the dashboard's
session. To try Lettuce first, the local dry run needs only Docker and no domain at all.

```bash
git clone https://github.com/jring-o/lettuce-compute.git
cd lettuce-compute
cp .env.example .env
```

Fill in every secret with a real value. A production head refuses to boot on a placeholder, or on a
secret shorter than its floor of 32 characters for generated machine secrets and 12 for human
passwords, and the error names the offending variable. Docker Compose expands `$` inside `.env`, so
escape every `$` in the registry password hash as `$$`.

Generate the attestation signing key. The head runs as uid 10001 and enforces the key's ownership
and mode at boot.

```bash
mkdir -p keys
openssl genpkey -algorithm ed25519 -out keys/signing.key
sudo chown 10001:10001 keys/signing.key
chmod 600 keys/signing.key
docker compose -f compose.production.yaml up -d --build
```

Caddy handles certificates, and migrations and the admin user run on first boot. A head with no
signing key fails to start rather than quietly minting a new identity. That key is the one
irreplaceable file on the server, so back it up. The [head setup guide](guides/head-setup.md) covers
the full walkthrough, upgrades, backups, and a restore runbook.

### What you control

A head is tuned by its operator, and two heads running the same binary can behave very differently.
Nearly every enforcement mechanism ships off, because turning it on is a policy decision only the
operator can make.

| Area | Knobs | Default |
|---|---|---|
| Dispatch pacing | Retry delays, work batching, per-machine in-flight caps, a minimum send interval | On, tuned |
| Adaptive quota | Grow a machine's allowance as its results validate | On |
| Trust gate | Require *N* already-trusted corroborators before a unit validates | Off |
| Account standing | Automatic probation and benching from decayed rejection rates | Off |
| Registration limits | Per-IP daily account cap; proof-of-work on new accounts | Off |
| Credit settlement | Maturation window, per-account daily emission cap, export circuit breakers | Off |
| Result audits | Re-run validated work on trusted runners, then slash, claw back, repair | Off |
| External outputs | Fetch and hash results submitted by reference | Off |
| DID binding | Let volunteers bind an ATProto identity | Off |
| Scale-out | N stateless replicas behind Caddy, shared Redis | 1 replica |

A lab head serving known colleagues might run redundancy 1, no trust gate, generous per-machine
caps, and credit as an internal scoreboard. A public head crediting strangers might run redundancy 3
with a seeded trust gate, standing backpressure, a 14-day credit maturation window, audits with
enforcement armed, and three replicas.

The trust gate fails closed if it is enabled with nobody seeded, so seed trusted subjects before
switching it on. Enabling audit enforcement requires a maturation window longer than the audit
horizon, and the head checks that at boot.

## Creating a leaf

A leaf's compute unit reads its parameters, computes, writes its result, and exits 0. The paths
arrive as environment variables and differ by runtime.

| | Native | Container |
|---|---|---|
| Parameters in | `$LETTUCE_PARAMS_FILE` | `$LETTUCE_PARAMETERS_FILE` (`/work/input/parameters.json`) |
| Result out | `$LETTUCE_OUTPUT_FILE` | `output.json` in `$LETTUCE_OUTPUT_DIR` (`/work/output`) |

Both should write a number from 0 to 100 into `$LETTUCE_PROGRESS_FILE` as they go, and long-running
work should save resumable state into `$LETTUCE_CHECKPOINT_DIR` so an interrupted unit continues
instead of restarting.

Any language works. Ship the unit as a WASM module, a container image, or a native binary. GPU leafs must be container or
WASM, since native binaries get no device passthrough. Publish a SHA-256 checksum for every
artifact. The head requires them for native binaries, and the client refuses to run an unverified
WASM module even where the head does not insist on one.

You then choose how the head decides that two answers agree.

- **EXACT** — byte-identical outputs, optionally ignoring declared volatile fields such as a
  wall-clock timing.
- **NUMERIC_TOLERANCE** — selected numeric fields within an epsilon. On a redundant leaf you must
  scope the comparison explicitly, or assert that every field is deterministic.

You also choose how much corroboration to require. `target_copies` is how many copies to dispatch in
parallel, and `min_quorum` is how many must agree. On a redundant leaf the agreement threshold has
to be a strict majority. Leafs whose output is only reproducible on like hardware can pin every copy
of a unit to one hardware class.

The head generates work units itself from a declared shape, either the Cartesian product of a
parameter space, a count of Monte Carlo trials with a seed derived per trial, or an input file split
into chunks. It can do that all at once, or lazily, topping up as the queue drains. Anything that
does not fit those shapes you upload in bulk.

Artifacts are versioned immutably. Publishing freezes the current execution config under a label and
repoints the leaf; running volunteers pick the new version up on their next work request without
restarting; and each unit is pinned to the version it was dispatched with, so results are never
compared across versions. Rolling back is moving the pointer.

Leafs are public (listed, dispatched to anyone), unlisted (readable by anyone with the ID, never
listed), or private (owner-only, and a stranger gets the same 404 as a missing leaf). Unlisted and
private leafs are dispatched only to volunteers who pin them by ID.

A leaf is created as metadata, configured, then activated. The head re-checks the whole
configuration on every activation, including a resume from pause, and reports everything wrong at
once rather than one error at a time.

See [guides/first-leaf.md](guides/first-leaf.md) for a full walkthrough and two working examples.

## Architecture

| Service | Role |
|---|---|
| **infrastructure** (Go, REST + gRPC) | Dispatch, validation, credit, attestations. Stateless, so it runs as N replicas. |
| **postgres** | Leafs, work units, results, volunteers, credit ledger |
| **redis** | Shared replay-dedup and rate-limit state across replicas |
| **dashboard** (Next.js) | Accounts and API keys, file hosting, visualization, browser volunteering, public leaf browser |
| **caddy** | Reverse proxy, automatic HTTPS, load-balances replicas |
| **registry** | The head's own OCI registry for container leafs |

The head is headless-capable. It boots, migrates, serves REST and gRPC, and volunteers attach with
no dashboard involved; a leaf owner holding the admin key can drive the entire lifecycle over the
API. The dashboard is where user accounts and API keys are issued, where input files and artifacts
live, and where browser volunteering and visualization happen, so in the shipped topology it is not
really optional. Schema migrations apply automatically on boot, and
[guides/migrations.md](guides/migrations.md) covers the authoring rules. Prometheus metrics and
pprof are admin-gated and, in the shipped proxy topology, unreachable from the internet.

### How work is dispatched

The head owns the conversation rate, so one head keeps up with a large fleet. Every reply, including
"no work right now", tells the volunteer when to come back, so the fleet's load self-throttles.
Volunteers request work in batches and hold a buffer measured in hours, making zero requests while
it is full. Requests are served from an in-memory pool with reservations written back
asynchronously; under sustained overload the head serves from that pool until it empties, then sheds
fast rather than letting database connections pile up.

Buffered work is leased rather than started. The deadline clock begins only when a unit actually
runs, and a unit not returned by its deadline is reassigned. There are no heartbeats. How much a
machine may hold grows as its results validate and shrinks on timeouts and disagreements; declared
specs gate what kind of work a machine is eligible for, and measured reliability governs how much.

## Validation, trust, and credit

Credit is meant to be worth something, so validation is treated as a security problem. The rule is
that identities are cheap to have and expensive to have trusted, and only trust can validate
results.

Anyone can register and start earning immediately. A result carries no weight in deciding whether a
work unit is correct until its account has earned trust, and trust comes only from work corroborated
by parties who are already trusted, seeded by the operator. A group of fresh accounts agreeing with
each other earns credit and no trust, so a Sybil farm cannot bootstrap itself by validating its own
answers.

### Identity

Your account is an Ed25519 keypair. Run the same key on every machine you own, and credit pools to
it. Each machine gets a host id issued by the head, which meters in-flight work and pacing and plays
no part in deciding whether results are correct.

You can bind your account to an ATProto DID with `bind-did`. Today it does one thing. It unifies
your devices into a single trust subject, so two machines of yours corroborate as one voice and are
never handed copies of the same unit. A DID is as free to mint as a keypair, and nothing in Lettuce
treats having one as evidence of anything. The binding lives in your own repository on your own
server, and deleting the record revokes it.

### Verifying credit

Every credit decision is signed and published as an attestation. The list response carries the
head's public key and, for each schema version, the exact ordered list of fields the signature
covers, so a consumer discovers what to verify in-band instead of hardcoding it. `POST
/api/v1/attestations/verify` rebuilds the canonical payload for a record and reports whether the
signature holds; an invalid signature is an answer, never an error. Grants carry their revocation
chain, so live credit nets grants against revocations.
[guides/attestation-verification.md](guides/attestation-verification.md) documents the canonical
forms, so you can verify records yourself without calling the head at all.

Operators can make credit unwindable with a maturation window before it settles, per-account
emission caps, and clawbacks that publish signed revocations. The public credit feed carries
circuit-breaker semantics. If an operator freezes it or an emission anomaly trips, it answers 503
with a machine-readable status header, so a downstream consumer halts rather than ingesting numbers
under investigation.

## Getting help, and getting involved

Questions and bug reports belong in
[GitHub issues](https://github.com/jring-o/lettuce-compute/issues).

Alongside the open beta, a private community of builders and testers works directly with the project
on faster development and testing, coordinating on Discord. To join, sign up at
[scios.tech/lettuce-beta](https://www.scios.tech/lettuce-beta).

## Releases and updating

Read the release note first. It states what changed and what you need to do, including whether head
operators and volunteers have to update together.

To update a head:

```bash
git pull
docker compose -f compose.production.yaml build infrastructure
docker compose -f compose.production.yaml build dashboard
docker compose -f compose.production.yaml up -d
```

Migrations run automatically on startup. Some releases need extra steps before `up -d`; the release
note and the [head setup guide](guides/head-setup.md) list them.

To update a volunteer client, run `./lettuce-volunteer update` and restart the daemon. The desktop
app updates itself; its releases are tagged `desktop-vX.Y.Z` and described in the
[desktop app README](apps/volunteer-desktop/README.md#releases).

## Development

The `make dev-*` targets run the development stack only. Each pins `-f compose.yaml -p lettuce-dev`
so they cannot touch a production stack started from the same directory; do not substitute a bare
`docker compose`.

```bash
make dev-up       # start in background
make dev-logs     # view logs
make dev-down     # stop (preserves database)
make dev-reset    # stop and reset database (asks for confirmation)
make dev-rebuild  # full rebuild (no cache)
```

```bash
curl http://localhost:8080/api/v1/health   # head, 503 when degraded
curl http://localhost:3000/health          # dashboard
```

Building the Go services standalone needs `GOWORK=off`, since the repo-root `go.work` lists only the
head services.

```bash
cd services/infrastructure && GOWORK=off go vet ./... && GOWORK=off go test ./...
```

The desktop app has its own loop; `make desktop-sidecar` compiles the client it bundles and
`make desktop-build` produces an installer. See the
[desktop app README](apps/volunteer-desktop/README.md#development-loop).

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full workflow, including the integration-test setup.

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See [LICENSE](LICENSE).
