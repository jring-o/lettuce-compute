# Leafs

Each subdirectory here holds the **integration layer** for a single
computation (a "leaf") that runs on this head.

A leaf needs:

- A **Dockerfile** (container leafs) or a build script (native leafs) that
  produces the artifact volunteers download.
- An **entrypoint** that honors the Lettuce contract — reads parameters
  from `$LETTUCE_PARAMETERS_FILE` (container) or `$LETTUCE_PARAMS_FILE`
  (native), writes output to `$LETTUCE_OUTPUT_DIR` / `$LETTUCE_OUTPUT_FILE`,
  reports progress to `$LETTUCE_PROGRESS_FILE` (see **Report progress**
  below), and exits 0 on success.
- Any **leaf-specific glue** (sample inputs, helper scripts, dataset
  download steps).
- A short **README** explaining what the leaf computes, what hardware it
  needs, and how to test it locally.

The underlying model or source code typically lives in its own upstream
repository (think a research codebase, a published paper's reference
implementation). This directory holds the **wrapper** that adapts it to
the Lettuce contract — usually thin, Dockerfile-shaped, and specific to
this head.

## Convention

```
leafs/
  <name>/
    README.md              # what this leaf computes, expected output, hardware
    Dockerfile             # how the image is built (container leafs)
    entrypoint.{sh,py,go}  # honors the LETTUCE_* env vars
    params.example.json    # sample work-unit input for local testing
    test/                  # optional: smoke-test scripts and fixtures
```

For native-binary leafs, replace `Dockerfile` with a `build.{sh,ps1}` that
produces the per-platform binaries and the SHA-256 manifest the head
requires.

## Report progress

Both native and container leaves **should** report progress. Periodically
write a single number `0`–`100` (the percent complete) to the file named by
`$LETTUCE_PROGRESS_FILE`; the volunteer reads it and `lettuce-volunteer status`
shows live progress and an ETA instead of a flat `0%`. A leaf that never writes
the file always shows `0%` until it finishes — a real reporting gap, not a
cosmetic one.

The volunteer runtime sets `$LETTUCE_PROGRESS_FILE` for **both** runtimes:

- **native** → `<work-dir>/progress.txt`
- **container** → `/work/output/progress.txt`

Guidance:

- Write from the entrypoint's main loop (per iteration / step / item). For a
  high-iteration loop, throttle to roughly every few seconds rather than every
  iteration.
- Make it **best-effort**: if the env var is unset or a write fails, swallow
  the error — progress reporting must never fail the work unit.
- Prefer an **atomic** write (write a temp file in the same directory, then
  rename over the target) so the volunteer never reads a half-written value.

Working reference implementations to copy:
[`guides/examples/monte-carlo-pi/main.go`](../guides/examples/monte-carlo-pi/main.go)
(Go, native) and
[`guides/examples/nbody-gravity/simulate.py`](../guides/examples/nbody-gravity/simulate.py)
(Python, container) both do exactly this. Deployed leaves such as the Beyblade
Arena native engine follow the same per-iteration pattern.

## Checkpointing (optional, for long work units)

Without checkpointing, a work unit that is interrupted — the volunteer is
stopped or updated, the machine reboots, or the unit is reassigned to a
different volunteer — **restarts from the beginning** and redoes all the work it
had already done. For short units that is fine. For long ones (tens of minutes
or more) it wastes the volunteer's compute and resets the progress bar. A leaf
that checkpoints resumes from roughly where it left off instead.

Checkpointing is **opt-in** and requires the leaf to cooperate — the
infrastructure cannot snapshot arbitrary in-progress computation for you. Two
parts:

1. **Enable it in the leaf's `fault_tolerance_config`:** set
   `checkpointing_enabled: true` and `checkpoint_interval_seconds` (minimum
   `60`). This is how much work is at risk: the volunteer archives the
   checkpoint on this interval, so at most one interval is ever lost.
2. **Honor the checkpoint contract in the entrypoint** (below).

### The contract

The runtime gives every runtime a per-unit checkpoint **directory** and creates
it before the leaf starts:

- `$LETTUCE_CHECKPOINT_DIR` — a directory the leaf reads from and writes its
  resumable state into. native → `<work-dir>/checkpoint`; container →
  `/work/checkpoint` (bind-mounted); wasm → `/work/checkpoint`.
- `$LETTUCE_CHECKPOINT_FILE` — a convenience path **inside** that directory
  (`.../checkpoint.dat`) for leaves whose state is a single file. Either way the
  whole directory is what gets captured and restored, so use whichever you like.

The leaf is responsible for two things:

- **Save** its resumable state into the directory periodically (e.g. "completed
  N of M items" plus any partial results). On the next interval the volunteer
  archives the directory to the head.
- **Resume on startup**: when the directory already contains a checkpoint, load
  it, continue from there instead of from zero, and immediately re-emit the
  matching `$LETTUCE_PROGRESS_FILE` value so progress does not reset. The same
  directory is populated whether the unit resumed on the same volunteer (the
  work dir is preserved across a restart) or was reassigned to a new one (the
  head's latest checkpoint is restored into it before the leaf starts), so a
  single resume path handles both.

Guidance:

- Write checkpoints **atomically** — write a temp file in the checkpoint
  directory and rename it over the target. A checkpoint can be captured (or the
  process killed) at any moment, so a half-written file must never be left
  behind for the next run to load.
- Keep it **best-effort**: a failed checkpoint write must never fail the work
  unit; the unit just falls back to restarting from the last good checkpoint.
- Stay within `max_checkpoint_size_bytes` (default 100 MB) — the head rejects a
  larger checkpoint.
- On a graceful stop the leaf is sent `SIGTERM` with a short grace window before
  it is killed. Trapping `SIGTERM` to flush one final checkpoint before exiting
  reduces the work lost on a clean stop to nearly zero; it is optional, since
  the interval already bounds the loss.

## How a leaf gets created

The agent-guided flow:

1. **`design-lettuce-leaf`** (skill in `.claude/skills/`) — scopes the
   computation into a `leaf-spec.md`.
2. **`create-lettuce-leaf`** (skill in `.claude/skills/`) — writes the
   wrapper into `leafs/<name>/`, builds and hosts the image/binary,
   creates the leaf via the head's REST API.

For operators running a managed-hosting platform on top of this head
(such as SciOS Compute), there's usually an additional platform-side
registration step that lives in the platform's own repo, not here.

## What does **not** live here

- The full model code (lives in its own upstream repo).
- Trained model weights, large datasets, or volunteer-side caches.
- Platform-specific glue (registration scripts, dashboard helpers).
