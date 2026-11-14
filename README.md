# Lettuce

Infrastructure for volunteer computing. You run the server; it answers to nobody.

You deploy your own server, a **head**, and define computations on it called **leafs**. Volunteers
anywhere donate spare CPU and GPU time to run the work. Each leaf's owner or the head's operator
sets how many volunteers must agree on a result; with two or more, results are cross-checked between
distinct accounts. Credit decisions are signed, so anyone holding the head's public key can check
that a signed decision came from that head and has not been altered since it was signed.

**Status:** open beta. Anyone can deploy a head, define leafs, or volunteer. To join the private
community of builders and testers, sign up at
[scios.tech/lettuce-beta](https://www.scios.tech/lettuce-beta).

## Roles

The three roles are head operators, leaf owners, and volunteers.

**Head operators** deploy a head. A head is a Postgres database, a Go server, Redis, a web dashboard,
a container registry, and a reverse
proxy that obtains a TLS certificate automatically. It hosts leafs, hands out **work units**,
validates what comes back, and records credit. It runs everything it needs itself, including its own
container registry and its own signing key.

**Leaf owners** define computations on a head. A leaf is a program plus a description of how to
split it into independent work units, how to decide whether two volunteers' answers agree, and what
each accepted result earns. Any authenticated user of a head can create and own leafs, so a head can
be one person's instance or a shared facility whose operator provisions accounts for colleagues.

**Volunteers** run the `lettuce-volunteer` client, attach to one or more heads, and crunch. There
is no approval step; by default, possession of a keypair is the whole of registration. Volunteers choose which
heads to attach to, how to split effort between them, which leafs to accept, when to run, and what
each head may execute on their machine.

Heads know nothing about each other. Credit, reputation, and standing live in one head's database,
are denominated however that head's leaf owners and operator choose, and mean nothing to any other
head. A volunteer attached
to three heads has three unrelated relationships that share one keypair and one process.

## Let an agent do it

An agent can do most of this for you. Open this repository in
[Claude Code](https://claude.com/claude-code) and say what you want to do. Three guided skills ship
with the repository. Each does the technical work itself and asks you only for what needs a human,
one step at a time, checking each step before moving on.

| Say | Skill | What it does |
|---|---|---|
| *"help me design a leaf"* | `design-lettuce-leaf` | Turns a research question into a concrete computation spec, before any code is written |
| *"help me deploy a Lettuce head"* | `deploy-lettuce-head` | Walks you through getting a server and pointing DNS, then generates secrets, deploys, and verifies the result |
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
   mv lettuce-volunteer-<os>-<arch> lettuce-volunteer   # e.g. lettuce-volunteer-linux-amd64
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

Attach to as many heads as you like. Each gets a weight, a share of your machine's compute time that
the client keeps balanced over time, however long each head's tasks run; leafs share their head's
time the same way. Weights are ratios, not caps.

```bash
./lettuce-volunteer heads weight head.example.com 200     # twice the compute time
./lettuce-volunteer leafs disable some-leaf               # never take this one
./lettuce-volunteer schedule set --from 20:00 --to 06:00  # overnight only
```

Not getting work? `./lettuce-volunteer doctor` gives a pass/fail diagnosis, and, while the daemon runs,
`./lettuce-volunteer leafs list` shows per leaf whether this machine will fetch it and why not. The
[volunteer setup guide](guides/volunteer-setup.md) covers per-OS container setup and
troubleshooting. Update with `./lettuce-volunteer update`.

### Desktop app

A desktop application for Windows, macOS, and Linux is in
[`apps/volunteer-desktop`](apps/volunteer-desktop/README.md) for volunteers who would rather not
use a terminal. It wraps the client above: it starts `lettuce-volunteer` for you, walks you through
setup (on Windows, including installing a container runtime), shows what your machine is working
on, and checks for its own updates. A head sees it as an ordinary client, because it is one. The
app's installers are published on GitHub as `desktop-vX.Y.Z` releases. The command-line client
remains the reference client, though a few controls are in the app only, such as pausing all work,
suspending or aborting a single task, and installing Podman on Windows.

### A head is a trust domain

Attaching to a head means trusting the person who runs it to execute code on your machine, so the
client makes that decision explicit and records it per head. There are three runtimes.

| Runtime | What it is | Default |
|---|---|---|
| **WASM** | A sealed WebAssembly sandbox. It sees only its own work folder, not your files, network, or keys. | Always allowed |
| **Container** | Docker or Podman, with all capabilities dropped and a non-root user (for CPU leafs; GPU leafs keep the engine's default user and capabilities unless `container_gpu_relax_user` is set to false), a read-only root filesystem, no network unless the leaf declares it, and memory, process, and disk caps. GPU via device passthrough. | Per head; defaults to yes at attach when Docker or Podman is found |
| **Native** | The leaf's binary runs directly on your machine with no sandbox. It can read your files, including your identity key. | Off; per-head opt-in only |

```bash
./lettuce-volunteer heads trust my-head                    # show current trust
./lettuce-volunteer heads trust my-head none               # WASM sandbox only
./lettuce-volunteer heads trust my-head container          # + containers
./lettuce-volunteer heads trust my-head container,native   # + unsandboxed native
```

The decision is enforced three ways. Runtimes you have not trusted anywhere are never built, each
head is told only what it is trusted for, and a head that dispatches a unit for an untrusted runtime
has that unit refused and handed straight back (`audit-runner` does not check this: it runs an audit
job in any runtime built for one of your heads). Declining everything is durable; a later upgrade
will not quietly re-grant it.

Your machine also holds the line on resources. You set CPU cores, memory, disk, GPU VRAM share,
network bandwidth, and, if you want one, a cap on how many tasks run at once. Each limit is a budget
for the whole machine. Each task is given the CPU cores its leaf can use: at least the minimum the
leaf declares, up to its maximum, as many as your budget has free when the task starts, less the
minimums of buffered tasks that could start alongside the new task. It keeps
them until it finishes, is held to them (except native tasks on macOS, which macOS cannot cap),
and is told the figure so it can size its worker pool.
The tasks' cores together stay within the CPU cores you allow (after you lower that limit, tasks
started before it keep their cores, or at least their minimum, until they finish), so how many tasks run at once
follows from your CPU and memory budgets. For any leaf you can fix the cores each of its tasks is
given and cap how many of them run at once, and the client never buffers more of a leaf than it
will run. You can also have Lettuce's native and container work run only part of the time ("use at most N % of CPU
time"), and restart a running task so it picks up changed settings. Native and container tasks run at the lowest
priority, so your own programs come first (under a rootful container engine, containers yield only
to each other); WASM tasks run inside the client at its own priority. The client reserves exactly
what it will enforce, using cgroups where available,
container limits, or WASM memory pages, with two exceptions for native tasks: on Linux without
cgroups their memory ceiling is twice the reservation plus 64 MB (at least 128 MB), and on macOS they get no memory or
CPU cap. It stops fetching before your disk runs low, pauses native and container tasks if your CPU or GPU
gets too hot (where the machine lets a program read the temperature: the CPU on Linux, or on a Mac
with the `osx-cpu-temp` helper; an NVIDIA GPU through `nvidia-smi`, an AMD GPU through `rocm-smi`
on Linux and macOS — the client tells you when it cannot), keeps its downloads and its uploads
under the bandwidth you set (container image pulls, a leaf's own network traffic, `update`, and the
desktop app's own updates excepted),
and, if you turn the setting on, pauses native and container tasks while other programs need the CPU. It does not
fetch work it could not finish before the deadline on your machine (judged by how long that leaf's
units have taken here), tells you when a setting works
against you (a CPU limit that runs one task at a time, a leaf that can never start), and, while the
daemon runs, shows what
would run together under your settings (`doctor`, and the desktop app's Projects page).

### Commands

| Command | What it does |
|---|---|
| `init` | First-run setup: identity, resource limits, optional run schedule |
| `start` / `stop` | Run or stop the daemon |
| `status` | Running tasks with progress and ETA, buffered work, credit, failing leafs |
| `doctor` | Pass/fail diagnosis of connectivity, runtimes, disk, memory, eligibility |
| `credit` | Your credit, per head and per leaf, and your results by state (waiting for validation, agreed, did not agree, in progress), asked of each head directly |
| `attach` / `detach` | Add or remove a head; `--leaf` pins a single leaf |
| `heads` | `list`, `weight`, `trust` |
| `leafs` | `list`, `enable`, `disable`, `weight`, `cores`, `max-running`, `reset` |
| `tasks` | `restart` a running task with the current settings |
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
server with systemd, such as Ubuntu 24.04 LTS or 22.04+, with at least 2 GB of RAM and Docker installed.
Visualizations are served from the second origin so leaf-supplied code cannot read the dashboard's
session on the main domain. To try Lettuce first, the local dry run needs only Docker and no domain at all.

```bash
git clone https://github.com/jring-o/lettuce-compute.git
cd lettuce-compute
cp .env.example .env
```

Fill in every secret with a real value, and replace every `your-domain.com` in `.env` and the
`Caddyfile` with your domain. A production head refuses to boot on a placeholder in any secret but
`REGISTRY_PASS_HASH`, which only Caddy reads, or on a
secret shorter than its floor of 32 characters for generated machine secrets and 12 for the admin
password, and the error names the offending variable. Docker Compose expands `$` inside `.env`, so
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
signing key fails to start rather than quietly minting a new identity. That key cannot be
recreated, so back it up, along with the database, which holds every account, result, and credit
record. The [head setup guide](guides/head-setup.md) covers
the full walkthrough, upgrades, backups, and a restore runbook.

### What you control

A head is tuned by its operator, and two heads running the same binary can behave very differently.
Most of the mechanisms below ship off, because turning one on is a policy decision only the
operator can make.

| Area | Knobs | Default |
|---|---|---|
| Dispatch pacing | Retry delays, work batching, per-machine in-flight caps, a minimum send interval | On |
| Adaptive quota | Grow a machine's allowance as its results validate | On |
| Trust gate | Require *N* already-trusted corroborators before a unit validates | Off |
| Account standing | Automatic probation and benching from decayed rejection rates | Off |
| Registration limits | Per-IP daily account cap; proof-of-work on new accounts | Off |
| Credit settlement | Maturation window, per-account daily emission cap, export circuit breakers | Off |
| Result audits | Re-run validated work on trusted runners, then slash, claw back, repair | Off |
| External outputs | Fetch and hash results submitted by reference | Off |
| DID binding | Let volunteers bind an ATProto identity | Off |
| Scale-out | N replicas behind Caddy, shared Redis | 1 replica |

A lab head serving known colleagues might run redundancy 1 on its leafs, no trust gate, generous per-machine
caps, and credit as an internal scoreboard. A public head crediting strangers might run redundancy 3
on its leafs with a seeded trust gate, standing backpressure, a 14-day credit maturation window, audits with
enforcement armed, and three replicas.

The trust gate fails closed if it is enabled with nobody seeded, so seed trusted subjects before
switching it on. Enabling audit enforcement requires a maturation window longer than the audit
horizon, and the head checks that at boot.

## Creating a leaf

A leaf's compute unit reads its parameters, computes, writes its result as JSON, and exits 0. The paths
arrive as environment variables and differ by runtime. A WASM module run in a browser tab gets no
path variables. It reads `/work/params.json` and writes `/work/output.dat` or to stdout.

| | Native | Container |
|---|---|---|
| Parameters in | `$LETTUCE_PARAMS_FILE` | `$LETTUCE_PARAMETERS_FILE` (`/work/input/parameters.json`) |
| Result out | `$LETTUCE_OUTPUT_FILE` | `output.dat` in `$LETTUCE_OUTPUT_DIR` (`/work/output`), or else the first file there by name |

Both should write a number from 0 to 100 into `$LETTUCE_PROGRESS_FILE` as they go, and long-running
work should save resumable state into `$LETTUCE_CHECKPOINT_DIR` so an interrupted unit continues
on the same machine instead of restarting. Another volunteer can continue from it only when the leaf
sets `checkpointing_enabled` and the unit runs as a single copy.

Any language works. Ship the unit as a WASM module, a container image, or a native binary. GPU leafs must be container or
WASM, since native binaries get no device passthrough. Publish a SHA-256 checksum for every
artifact. The head requires them for native binaries, and the `lettuce-volunteer` client refuses to run an unverified
WASM module even where the head does not insist on one. A browser tab runs the module with no checksum check.

You then choose how the head decides that two answers agree.

- **EXACT** — byte-identical outputs, optionally ignoring declared volatile fields such as a
  wall-clock timing.
- **NUMERIC_TOLERANCE** — numbers within an epsilon and every other value exact. It compares the
  whole output unless you select fields or ignore some. On a redundant leaf you must
  scope the comparison explicitly, or assert that every field is deterministic.

You also choose how much corroboration to require. `target_copies` is how many copies to dispatch in
parallel, and `min_quorum` is how many must agree. On a redundant leaf the agreement threshold has
to be a strict majority. Leafs whose output is only reproducible on like hardware can pin every copy
of a unit to one hardware class.

The head generates work units itself from a declared shape, either the Cartesian product of a
parameter space, a count of Monte Carlo trials with a seed derived per trial, or an input file split
into chunks. It can do that all at once, or, for parameter sweeps and Monte Carlo, lazily, topping up as the queue drains. Anything that
does not fit those shapes you upload in bulk.

Once you publish a version, artifacts are versioned immutably. Publishing freezes the current execution config under a label and
repoints the leaf; running volunteers pick the new version up on their next work request without
restarting; and each unit is pinned to the version it was dispatched with, so results are never
compared across versions. Rolling back is moving the pointer.

Leafs are public (listed, dispatched to anyone), unlisted (readable by anyone with the ID, never
listed), or private (readable only by its owner and admins, and a stranger gets the same 404 as a missing leaf). Unlisted and
private leafs are dispatched only to volunteers who pin them by ID.

A leaf is created as metadata, configured, then activated. The head re-checks the execution,
validation, fault-tolerance, and data configs on every activation, including a resume from pause,
and reports the first error of each failing section together.

See [guides/first-leaf.md](guides/first-leaf.md) for a full walkthrough and two working examples.

## Architecture

| Service | Role |
|---|---|
| **infrastructure** (Go, REST + gRPC) | Dispatch, validation, credit, attestations. Runs as N replicas, but each keeps checkpoint files on its own disk. |
| **postgres** | Leafs, work units, results, volunteers, credit ledger |
| **redis** | Shared replay-dedup and rate-limit state across replicas |
| **dashboard** (Next.js) | Accounts and API keys, visualization, browser volunteering, public leaf browser |
| **caddy** | Reverse proxy, automatic HTTPS, load-balances replicas |
| **registry** | The head's own OCI registry for container leafs |

The head is headless-capable. It boots, migrates, serves REST and gRPC, and volunteers attach with
no dashboard involved; a leaf owner holding the admin key can drive the entire lifecycle over the
API. The dashboard is where user accounts and API keys are issued
and where browser volunteering and visualization happen, so in the shipped topology it is not
really optional. Schema migrations apply automatically on boot, and
[guides/migrations.md](guides/migrations.md) covers the authoring rules. Prometheus metrics and
pprof are admin-gated and, in the shipped proxy topology, unreachable from the internet.

### How work is dispatched

The head owns the conversation rate, so one head keeps up with a large fleet. Every successful reply
to a `lettuce-volunteer` work request, including
"no work right now", tells the volunteer when to come back, so the fleet's load self-throttles.
After an overload or rate-limit refusal the client waits about 30 seconds on its own, and browser
volunteers retry every 10 seconds.
The `lettuce-volunteer` client requests work in batches and holds a buffer measured in hours, making zero requests while
it is full. Its requests are served from an in-memory pool with reservations written back
asynchronously; under sustained overload the head serves from that pool until it empties, then sheds
fast rather than letting database connections pile up.

Buffered work is leased rather than started. The run deadline begins only when a unit actually
runs, and a unit not returned by its deadline is reassigned. A buffered copy not started within one
deadline of hand-out is released too, which counts against the machine. There are no heartbeats. How much a
machine may hold grows as its results validate and shrinks on timeouts and disagreements; declared
specs gate what kind of work a machine is eligible for, and measured reliability governs how much.

## Validation, trust, and credit

Credit is meant to be worth something, so validation is treated as a security problem. The rule is
that identities are cheap to have and expensive to have trusted. When the operator turns on the trust
gate (off by default), a unit validates only if its agreeing group includes enough trusted accounts,
one by default.

Anyone can register and start earning immediately. Until its account has earned trust, a result
still counts toward agreement, but it cannot be one of the trusted corroborators the trust gate asks
for. Trust comes only from work corroborated
by parties who are already trusted, seeded by the operator. A group of fresh accounts agreeing with
each other earns no trust, and it earns credit only while the trust gate is off, so a Sybil farm
cannot bootstrap itself by validating its own
answers.

### Identity

Your account is an Ed25519 keypair. Run the same key on every machine you own, and credit pools to
it. Each machine running the volunteer client gets a host id issued by the head (up to 10 per account
by default; machines past the cap are metered together under the account), which meters in-flight work and pacing and plays
no part in deciding whether results are correct.

On a head whose operator has turned on DID binding (off by default), you can bind your account to
an ATProto DID with `bind-did`. Today it does one thing. It unifies
your devices into a single trust subject, so two machines of yours corroborate as one voice and are
never handed copies of the same unit. A DID is as free to mint as a keypair, and nothing in Lettuce
treats having one as evidence of anything. The binding lives in your own ATProto repository, on
whichever server hosts it (`bind-did` defaults to `https://bsky.social`), and deleting the record revokes it.

### Verifying credit

Credit decisions are signed and published as attestations in a best-effort step after the credit is
recorded, so a failed step can leave a decision without an attestation. The list response carries the
head's public key and, for each schema version, the exact ordered list of fields the signature
covers, so a consumer discovers what to verify in-band instead of hardcoding it. `POST
/api/v1/attestations/verify` rebuilds the canonical payload for a record and reports whether the
signature holds; an invalid signature is an answer, never an error. For a grant, the verify response
also lists every revocation against it, so live credit nets grants against revocations.
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
app checks for updates itself and installs one when you click Install Now; its releases are tagged
`desktop-vX.Y.Z` and described in the
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

Building the Go services standalone needs `GOWORK=off` because the repo-root `go.work` puts the head
and the volunteer client in one workspace.

```bash
cd services/infrastructure && GOWORK=off go vet ./... && GOWORK=off go test ./...
```

The desktop app has its own loop; `make desktop-sidecar` compiles the client it bundles and
`make desktop-build` produces an installer. See the
[desktop app README](apps/volunteer-desktop/README.md#development-loop).

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full workflow, including the integration-test setup.

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See [LICENSE](LICENSE).
