# Contributing to Lettuce

Thanks for your interest in contributing! This document explains how to contribute
and the licensing terms that apply to every contribution.

Lettuce is licensed under the **GNU Affero General Public License v3.0 (AGPL-3.0)**.
Please read the **[Contributor License Agreement](#contributor-license-agreement-cla)**
below before submitting — by contributing, you agree to it.

---

## How to contribute

1. **Open an issue first** for anything non-trivial (new features, behavior changes,
   architectural shifts) so we can agree on the approach before you invest time.
2. **Fork** the repo and create a topic branch off `main`.
3. Make your change. Keep PRs focused — one logical change per PR.
4. **Make sure it builds and tests pass:**
   - Go services: `go build ./...` and `go test ./...` in `services/infrastructure`
     and `services/volunteer-cli`
   - Dashboard: `npm install && npm test` in `services/dashboard`
5. Run linters where configured (`golangci-lint run`, `npm run lint`).
6. **Sign off every commit** (see [DCO](#developer-certificate-of-origin-dco)):
   `git commit -s`
7. Open a pull request. Describe the change and link the issue.

---

## Running integration tests

Most Go tests run with plain `go test ./...`. A subset is gated by the
`//go:build integration` tag and needs a real Postgres — without
`LETTUCE_TEST_DB_URL` they `t.Skip`. Two modules contain integration tests:
`services/infrastructure` and `services/volunteer-cli`.

### 1. Stand up an ephemeral Postgres

Use port `5433` to avoid clashing with a local Postgres on `5432`. Pick podman
or docker — the flags are identical:

```bash
podman run -d --name lettuce-test-pg \
  -e POSTGRES_USER=lettuce -e POSTGRES_PASSWORD=testpass -e POSTGRES_DB=lettuce \
  -p 5433:5432 postgres:16-alpine
```

(replace `podman` with `docker` if you prefer). Wait for the container to be
ready with `pg_isready -h localhost -p 5433` before continuing.

### 2. Apply migrations

The test harness's `setupTestDB` does **not** migrate — it assumes a
pre-migrated schema. Migrations are embedded via golang-migrate, so they're
applied by code, not the CLI. The simplest way to run them against the test DB
is a throwaway program. Create `services/infrastructure/cmd/testmigrate/main.go`:

```go
package main

import (
	"log"
	"os"

	"github.com/lettuce-compute/infrastructure/internal/database"
)

func main() {
	if err := database.RunMigrations(os.Getenv("LETTUCE_TEST_DB_URL")); err != nil {
		log.Fatal(err)
	}
}
```

Then from `services/infrastructure`:

```bash
go run ./cmd/testmigrate
```

Delete the `cmd/testmigrate` directory when you're done — it's not meant to be
committed.

### 3. Export the env vars

All of these need to be set for a green run. The first is what most tests
read; the rest are required by `internal/database/*_test.go`, which builds its
own `DatabaseConfig` from the individual components. Without
`LETTUCE_TEST_DB_PASSWORD` and `LETTUCE_TEST_DB_NAME` you'll see auth failures
against a `lettuce_test` DB that doesn't exist.

```bash
export LETTUCE_TEST_DB_URL=postgres://lettuce:testpass@localhost:5433/lettuce?sslmode=disable
export LETTUCE_TEST_DB_HOST=localhost
export LETTUCE_TEST_DB_PORT=5433
export LETTUCE_TEST_DB_NAME=lettuce
export LETTUCE_TEST_DB_USER=lettuce
export LETTUCE_TEST_DB_PASSWORD=testpass
```

### 4. Run the tests

Integration test packages share **one** test DB and `DELETE` common tables
during setup/teardown, so running packages concurrently causes cross-package
contention (shared mutable state, not a real bug). Serialize with `-p=1`:

```bash
go test -tags=integration -p=1 ./...
```

Run that once in `services/infrastructure`, then again in
`services/volunteer-cli`.

### 5. Tear down

```bash
podman rm -f lettuce-test-pg
```

(or `docker rm -f`). Confirm with `podman ps -a` (or `docker ps -a`) — the
name should be gone.

### Windows caveat

`TestE2EV04FullLifecycle` can hang on Windows hosts when `wmic` / `amd-smi`
are slow or missing; it passes on Linux CI. If you're on Windows and only
that one test fails, suspect the environment before your change.

---

## Load-testing with swarm-sim

`swarm-sim` is a volunteer-swarm simulator used to load-test a head and
calibrate its dispatch tuning. It lives at
`services/volunteer-cli/cmd/swarm-sim` (in the volunteer-cli module, the only
place that can import the real signing client, so each simulated volunteer
speaks the genuine per-request-signed protocol). It spins up a fleet of
simulated volunteers, optionally seeds a leaf with work units over the head's
HTTP admin API, runs for a fixed duration, and prints a metrics report.

### Profiles

- **`naive`** — loops `RequestWorkUnit` with `MaxAssignments=1` at a fixed
  interval and **ignores** the head's *server-directed retry delay*. It
  approximates a pre-batching client and produces the maximum request noise.
- **`buffered`** — the full client model: it requests work in batches
  (*work batching*), maintains an in-memory *client work buffer* sized in hours,
  **obeys** the *server-directed retry delay*, makes **zero** `RequestWorkUnit`
  calls while its buffer is full, and treats *per-client rate limiting*
  (`ResourceExhausted`) as a fixed local backoff.
- **`overload`** — the saturation driver: a hot `RequestWorkUnit` loop with
  **no** inter-request delay that ignores the *server-directed retry delay*
  entirely, so a fleet larger than the head's dispatch ceiling pins the hot
  path. Use it to measure the single-head dispatch ceiling (the report's
  per-second **peak dispatch/sec**) and to prove the head sheds gracefully
  (`ResourceExhausted`) instead of collapsing the DB pool. The report's
  `shed (ResourceExhausted)` ratio and the `DB-pool collapse` flag are the
  pass/fail signals: under overload the head should shed (ratio > 0) and the
  collapse flag should stay **no** (zero `DeadlineExceeded`/`Unavailable` on
  `RequestWorkUnit`). By default the profile ignores sheds and re-requests
  immediately (maximal pressure); pass `--honor-shed` to back off like a real
  client.
- **`request-only`** — a supplementary **HandOut-isolation probe**: `overload`'s
  hot `RequestWorkUnit` loop with **no** `StartWork`/`SubmitResult` (no
  pretend-compute), so it measures the pure server-side *dispatch cache* HandOut
  path — the lock/scan ceiling and head-side `RequestWorkUnit` p50/p99 vs
  concurrency — with zero write-path noise. It is how the FIX-1 lock-cliff
  removal is read in isolation. **It reserves units without recycling them, so it
  MUST be run against a generously primed ready pool and a short `--duration`**
  (large head `ready_pool_size` + `refill_batch_size` and a high `--seed-units`),
  or once the pool drains it just measures the `readyLen==0`+admission-saturated
  shed branch instead of the HandOut win. The **headline** FIX-1 number still
  comes from the `overload` before/after comparison (where units recycle via
  `StartWork` and keep the pool primed); `request-only` is a supplementary probe.

Run-start is the explicit `StartWork` RPC (one call per unit actually executed,
**not** per request); there is no per-task heartbeat, so liveness is
deadline-based. The report's per-RPC table shows `StartWork`, a `throttl`
(`ResourceExhausted` graceful shed) column, a `collaps` column (**true** head-side
DB-pool collapse: a head-side `DeadlineExceeded`/`Unavailable` while the run is
live), and a `cancel` column. **`cancel` is the known false-positive guard:** an
in-flight RPC cancelled at run shutdown — or a client-side `DeadlineExceeded` from
mere head lock-slowness as the run winds down — is recorded as `cancel`, **not**
`collaps`, so it does not falsely trip the DoD collapse flag. Read write-path
shedding (FIX 2/3) from the `StartWork`/`SubmitResult` rows' `throttl`>0 with
`collaps`==0 — **not** from the RWU-only `collapsed` headline flag / `shed_ratio`,
which are keyed only on `RequestWorkUnit` by design.

### Seeding

By default the simulator seeds an active leaf with work units over the HTTP
admin API (create → configure → update → activate → `work-units/generate`),
idempotent on the `--seed-leaf` name. Leaf creation needs a `creator_id` that
references a real user row, so pass `--creator-id` (or `SWARM_SIM_CREATOR_ID`)
the id of an existing user. Pass `--no-seed` to target a pre-existing leaf by
name without seeding.

### In-process vs standalone head

There is no in-process head harness for the simulator: it lives in a different
module from the head and cannot import the head's internal packages. Run the
simulator against a **standalone head process**:

1. Start a throwaway Postgres (a non-default host port; do **not** touch a local
   Postgres 17 service):

   ```bash
   podman run -d --name lettuce-sim-pg \
     -e POSTGRES_USER=lettuce -e POSTGRES_PASSWORD=testpass -e POSTGRES_DB=lettuce \
     -p 5434:5432 docker.io/library/postgres:16-alpine
   ```

2. Run the head with TLS off on `127.0.0.1:9090` (gRPC) / `:8080` (HTTP),
   pointing at that Postgres. The head applies migrations on boot. Set
   `LETTUCE_ADMIN_API_KEY` (required) and, to provision an owner user for
   seeding, `LETTUCE_ADMIN_EMAIL` + `LETTUCE_ADMIN_PASSWORD`. On Windows +
   podman, the published port is reachable via the podman VM's `eth0` IP, not
   `127.0.0.1`, so set `LETTUCE_DB_HOST` to that VM IP.

3. Read the bootstrapped admin user's id from the DB (`SELECT id FROM users
   WHERE role='ADMIN'`) and pass it as `--creator-id`.

4. Run a profile:

   ```bash
   go run ./cmd/swarm-sim \
     --head-grpc=127.0.0.1:9090 --head-http=http://127.0.0.1:8080 \
     --admin-key=$LETTUCE_ADMIN_API_KEY --creator-id=<admin-user-id> \
     --volunteers=25 --profile=buffered --duration=60s \
     --seed-leaf=swarm-test --seed-units=100000 \
     --buffer-hours=2 --max-assignments=8 --report=text
   ```

   Run it once per profile (`--profile=naive`, then `--profile=buffered`) to
   compare fleet `RequestWorkUnit` rate at equal dispatch throughput.

#### Ceiling / overload run (single-head dispatch ceiling + graceful shedding)

To measure the single-head dispatch ceiling and confirm the head sheds
gracefully under naive overload (the Layer 2 Definition of Done), drive the
`overload` profile with a fleet well above capacity:

```bash
go run ./cmd/swarm-sim \
  --head-grpc=127.0.0.1:9090 --head-http=http://127.0.0.1:8080 \
  --admin-key=$LETTUCE_ADMIN_API_KEY --creator-id=<admin-user-id> \
  --volunteers=2000 --profile=overload --duration=60s \
  --seed-leaf=swarm-ceiling --seed-units=200000 --report=json
```

Read **`peak_dispatch_per_sec`** as the ceiling, **`shed_ratio`** as the
graceful-backpressure fraction, and **`collapsed`** (must be `false`) as the
no-pool-collapse assertion.

Two head-side knobs are required for a meaningful ceiling run:

- **Raise the gRPC rate limits.** A single-host swarm shares one source IP, so
  the Layer 0 per-IP bucket (default 60/min) would shed the fleet before the
  Layer 2 admission cap is exercised — and the `shed_ratio` would just measure
  the per-IP limiter, not the head's dispatch capacity. Raise both budgets on
  the **head process** when launching it:

  ```bash
  LETTUCE_GRPC_PER_IP_RATE_LIMIT=100000000 \
  LETTUCE_GRPC_PER_PUBKEY_RATE_LIMIT=100000000 \
  ...other env... lettuce-server --config ...
  ```

  (These map to `SetGRPCRateLimits`; a non-positive value leaves the default.)

- **Set the DB pool to the operator size (~60)** to reproduce the collapse
  threshold the load test originally found — the code default is 25:

  ```bash
  LETTUCE_DB_MAX_CONNS=60 ...
  ```

Run the overload profile **before** and **after** the Layer 2 changes to compare
`peak_dispatch_per_sec` against the ~240/s baseline and to confirm `collapsed`
flips from `true` (pre-Layer-2 pool collapse) to `false` (graceful shedding).

For the perf-hardening pass, run `--profile=overload` at **20/100/200/500/1000**
volunteers before vs after, comparing the per-RPC `RequestWorkUnit` p50/p99 and
`peak_dispatch_per_sec`: the headline FIX-1 result is that the head-side
`RequestWorkUnit` latency cliff under a concurrent storm (the old
sub-ms@20 → hundreds-of-ms@500 climb) is materially reduced, with `RequestWorkUnit`
still off Postgres (`collapsed`=`false`). Write-path shedding (FIX 2/3) is read
from the `StartWork`/`SubmitResult` rows' `throttl`>0, `collaps`==0 — confirm the
head returns `ResourceExhausted` instead of logging `context deadline exceeded`.

#### HandOut-isolation run (FIX-1 lock cliff, write path excluded)

To isolate the pure HandOut path from the write path, run the `request-only`
probe against a **generously primed** ready pool and a **short** duration so the
numbers are the contended HandOut, not the drained-pool shed branch:

```bash
# Head launched with a large primed pool, e.g.:
#   LETTUCE_HEAD_READY_POOL_SIZE=20000 LETTUCE_HEAD_REFILL_BATCH_SIZE=5000 \
#   LETTUCE_GRPC_PER_IP_RATE_LIMIT=100000000 LETTUCE_GRPC_PER_PUBKEY_RATE_LIMIT=100000000 \
#   LETTUCE_DB_MAX_CONNS=60 ...other env... lettuce-server --config ...
go run ./cmd/swarm-sim \
  --head-grpc=127.0.0.1:9090 --head-http=http://127.0.0.1:8080 \
  --admin-key=$LETTUCE_ADMIN_API_KEY --creator-id=<admin-user-id> \
  --volunteers=500 --profile=request-only --duration=15s \
  --seed-leaf=swarm-handout --seed-units=300000 --report=json
```

Read the `RequestWorkUnit` row's p50/p99 vs volunteer count (sweep 20/100/200/500)
as the pure HandOut latency curve. If the run reports a high `shed_ratio` or many
`RequestWorkUnit` sheds, the pool drained — raise `--seed-units` /
`ready_pool_size` / `refill_batch_size` or shorten `--duration` and re-run, since
a drained pool measures the shed branch, not the HandOut win.

### Per-client rate limiting note

The head's *per-client rate limiting* buckets gRPC callers per source IP. A
swarm run entirely from one host shares a single source IP, so a large fleet
will be throttled (`ResourceExhausted`) — this is itself a valid Layer-0
measurement, but it means request-rate numbers from a single-host swarm are
bounded by the per-IP limit. The simulator retries **registration** through the
limiter so the fleet still comes up; steady-state `RequestWorkUnit` numbers
reflect whatever the limiter allows. For a ceiling/overload run, raise the
budgets on the head with `LETTUCE_GRPC_PER_IP_RATE_LIMIT` /
`LETTUCE_GRPC_PER_PUBKEY_RATE_LIMIT` (see the ceiling-run recipe above) so the
shed ratio measures the head's dispatch admission cap, not the per-IP bucket.

### Integration smoke test

`cmd/swarm-sim/smoke_test.go` (`//go:build integration`) runs 20 volunteers ×
5s for **both** profiles against a head you stand up out of process, and
asserts the buffered profile makes strictly fewer `RequestWorkUnit` calls than
the naive profile. It is env-gated and skips unless these are set:

```bash
export SWARM_SMOKE_GRPC=127.0.0.1:9090
export SWARM_SMOKE_HTTP=http://127.0.0.1:8080
export SWARM_SMOKE_ADMIN=$LETTUCE_ADMIN_API_KEY
export SWARM_SIM_CREATOR_ID=<admin-user-id>
go test -tags=integration -run TestSwarmSimSmoke ./cmd/swarm-sim/
```

### Calibrating `target_request_rate_per_sec`

The head's `target_request_rate_per_sec` (env
`LETTUCE_HEAD_TARGET_REQUEST_RATE_PER_SEC`, default 500) is **not** a trusted
default — it must be calibrated against the real single-head assignment ceiling
on the target hardware before the head's *server-directed retry delay* behaves
meaningfully under load. Use the simulator's reported dispatch throughput
(assignments/sec) at saturation as the measured ceiling, then set the head knob
to that value and re-run. Until calibrated, the delay curve (which keys off the
ratio of observed request rate to this target) over- or under-shoots.

---

## Developer Certificate of Origin (DCO)

Every commit must be signed off. The sign-off certifies that you wrote the code
or otherwise have the right to submit it under the project's license. Add it with:

```bash
git commit -s -m "your message"
```

This appends a line like `Signed-off-by: Your Name <you@example.com>` to the commit
message (your real name and a valid email are required). By signing off you certify
the following:

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

---

## Contributor License Agreement (CLA)

> **Why this exists.** Lettuce ships under AGPL-3.0 today. This agreement lets the
> maintainer (a) offer commercial/dual licenses to organizations that cannot comply
> with the AGPL, and (b) relax the project's license in the future (for example to
> Apache-2.0 or MIT) **without having to locate and re-secure permission from every
> past contributor**. You keep the copyright to your work; you simply grant a broad
> license so the project stays relicensable by its maintainer.

By submitting a Contribution to this project, **You** (the individual or legal entity
making the Contribution) agree to the terms below. If you are contributing on behalf
of your employer, you confirm you are authorized to do so.

In this Agreement, **"Maintainer"** means **Jonathan Starr**,
the copyright holder and steward of the Lettuce project. **"Contribution"** means any
original work of authorship — code, documentation, or other material — that You
intentionally submit to the project (e.g., via pull request, patch, or issue).

1. **Copyright license grant.** You retain all right, title, and interest in Your
   Contributions. You hereby grant the Maintainer a perpetual, worldwide,
   non-exclusive, royalty-free, irrevocable license to reproduce, prepare derivative
   works of, publicly display, publicly perform, sublicense, and distribute Your
   Contributions and any derivative works thereof. **This grant expressly includes
   the right for the Maintainer to license and sublicense Your Contribution (and
   works derived from it) under any license terms whatsoever, including without
   limitation the AGPL-3.0, other open source licenses such as Apache-2.0 or MIT,
   and proprietary or commercial license terms.**

2. **Patent license grant.** You grant the Maintainer and recipients of software
   distributed by the Maintainer a perpetual, worldwide, non-exclusive, royalty-free,
   irrevocable (except as stated in this section) patent license to make, have made,
   use, offer to sell, sell, import, and otherwise transfer Your Contribution, where
   such license applies only to those patent claims licensable by You that are
   necessarily infringed by Your Contribution alone or by combination of Your
   Contribution with the project.

3. **You have the right to grant this license.** You represent that each of Your
   Contributions is Your original creation and that You are legally entitled to grant
   the above licenses. If Your employer has rights to intellectual property You
   create, You represent that You have received permission to make the Contributions
   on behalf of that employer, or that the employer has waived such rights.

4. **No obligation.** You understand that the decision to include Your Contribution
   in any project or distribution is entirely at the Maintainer's discretion, and
   this Agreement does not obligate the Maintainer to use Your Contribution.

5. **"As is."** Unless required by applicable law or agreed to in writing, You provide
   Your Contributions on an "AS IS" basis, without warranties or conditions of any
   kind, either express or implied.

**Acceptance.** Submitting a Contribution (for example, opening a pull request) and
signing off your commits under the DCO above constitutes Your acceptance of this CLA.

---

## Questions

Open an issue or start a discussion. Thanks for helping build Lettuce!

---

*This document is provided for project-governance purposes and is not legal advice.
The CLA terms underpin the project's ability to dual-license and relicense; before
relying on them commercially, the Maintainer should have them reviewed by a lawyer
and insert the correct legal entity name above.*
