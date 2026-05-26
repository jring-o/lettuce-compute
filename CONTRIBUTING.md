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

	"github.com/lettuce-compute/lettuce/services/infrastructure/internal/database"
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
