# Database migrations — operations runbook & authoring policy

The head manages its PostgreSQL schema with [golang-migrate](https://github.com/golang-migrate/migrate).
The SQL files live in `services/infrastructure/migrations/` (numbered `NNNNN_name.up.sql` /
`.down.sql` pairs), are embedded into the server binary, and are **applied automatically every time
the head boots** — there is no separate migration step in a normal deploy. Concurrent boot of
several replicas is safe: golang-migrate serializes appliers with an internal advisory lock.

This guide covers the two things that are *not* automatic: recovering when a migration fails
partway (the "dirty schema" state), and the rules for writing new migrations.

## How a migration failure presents

If a migration fails mid-apply, golang-migrate records the attempted version in the
`schema_migrations` table with `dirty = true` and the head exits. Because the compose stack runs
the head with `restart: unless-stopped`, the container then restart-loops; every attempt logs an
error naming the dirty version and pointing here. The head deliberately refuses to run on an
indeterminate schema — the loop is the alarm, not the problem.

The migration session runs with fail-fast timeouts (serving traffic is unaffected — these apply to
the migration connection only):

- **`lock_timeout=5s`** — boot-time DDL such as `ALTER TABLE` needs an `ACCESS EXCLUSIVE` lock. On
  a busy table it would otherwise queue behind long-running queries *and block every query behind
  it* for as long as it waits. With the timeout, a contended migration aborts quickly (marking the
  schema dirty) instead of freezing traffic; retry it off-peak.
- **`statement_timeout` of 60s** (via golang-migrate's `x-statement-timeout`) — bounds each
  migration's execution so a runaway statement cannot wedge boot indefinitely. A migration that
  legitimately needs longer is a *maintenance-window migration* (see the policy below) and should
  be run with traffic stopped.

## Recovering from a dirty schema

You need `psql` access to the head's database. On the compose stack:

```bash
docker compose -f compose.production.yaml exec postgres psql -U lettuce -d lettuce
```

1. **Find the dirty version.**

   ```sql
   SELECT version, dirty FROM schema_migrations;
   ```

2. **Inspect the failed migration** — `services/infrastructure/migrations/NNNNN_*.up.sql` for the
   reported version — and compare it against the live schema (`\d <table>` in psql) to determine
   which of its statements applied before the failure. Each migration file runs as a single
   multi-statement execution, but DDL in PostgreSQL is not atomic across a mid-execution
   connection loss or timeout, so partial application is possible.

3. **Repair by hand** — either finish the migration (run its remaining statements yourself) or
   undo the statements that did apply. Pick one side; do not leave it half-done.

4. **Clear the dirty flag** to match what you did:

   ```sql
   -- If you completed migration N by hand:
   UPDATE schema_migrations SET version = N, dirty = false;

   -- If you undid migration N's partial changes:
   UPDATE schema_migrations SET version = N - 1, dirty = false;
   ```

   (The `migrate` CLI's `force` command does the same thing if you prefer it:
   `migrate -path services/infrastructure/migrations -database "$DATABASE_URL" force <version>`.)

5. **Restart the head** (`docker compose -f compose.production.yaml up -d infrastructure`). Boot
   re-applies anything still pending — including migration N itself if you set `version = N - 1`.

If the failure was a lock timeout (step 2 shows *nothing* applied), no repair is needed: clear the
flag with `version = N - 1` and restart when the table is quiet.

## Policy for writing new migrations

These rules exist because migrations run unattended at boot, on every deployment, against
databases we do not control. They bind all future migrations; they are checked at review.

1. **Never edit an applied migration.** Once a migration has shipped in a release, its file is
   frozen — fixes go in a *new* migration. Editing shipped SQL makes fresh installs diverge from
   upgraded ones and turns the version history into a lie.
2. **Additive first.** Prefer adding columns/tables/indexes over dropping or rewriting, so a code
   rollback does not require a schema rollback.
3. **Drop in two releases.** To remove a column that live code reads or writes: release 1 stops
   the code using it; release 2 (after release 1 is deployed everywhere) drops it. A single-step
   drop breaks any replica still running the previous code during a rolling deploy.
4. **Indexes on hot tables use `CREATE INDEX CONCURRENTLY`, one statement per file.**
   A plain `CREATE INDEX` blocks writes to the table for the whole build. `CONCURRENTLY` cannot
   run inside a transaction, and golang-migrate executes each file as one multi-statement
   execution (an implicit transaction) — so a concurrent index build must be the **only**
   statement in its migration file.
5. **Batch backfills.** An unbatched `UPDATE table SET … WHERE …` over a large table locks and
   rewrites it in one shot, at boot. Backfills over potentially-large tables must loop in bounded
   batches — or be explicitly declared maintenance-window (below).
6. **Declare maintenance-window migrations.** A migration that cannot meet the session timeouts
   (long index build, large backfill, table rewrite) must say so in a header comment in the file
   (`-- MAINTENANCE WINDOW: <why, expected duration>`), and its release notes must tell operators
   to stop traffic (or accept the pause) before deploying.
7. **Write an honest `.down.sql`.** Every migration ships a down pair. If the down is lossy
   (restored columns come back empty, deleted rows are gone), say so in a comment at the top of
   the down file — an operator deciding whether to roll back needs that fact.

**Retroactive record:** migrations `00002` (index builds) and `00006` (per-copy dispatch: plain
index builds, unbatched backfill `UPDATE`s, and a column drop whose down restores the columns
empty — its reservation data is unrecoverable) predate this policy and are, by these rules,
maintenance-window migrations with a lossy down. They are long applied on every live head and are
**not** being edited (rule 1); this note is their required declaration.
