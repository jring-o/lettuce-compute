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
