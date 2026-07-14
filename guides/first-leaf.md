# Creating Your First Leaf

A **leaf** is a single computation running on your head. This guide takes a compute
program from your laptop to running on volunteers' machines, through the head you
deployed in **[head-setup.md](head-setup.md)**.

There are two complete walkthroughs:

- **[Path 1 — Native binary](#path-1--native-binary-leaf)** — the simplest leaf. A small
  compiled program. Start here.
- **[Path 2 — Container](#path-2--container-leaf)** — package any language (Python, R, …)
  as a container image. Use this when cross-compiling a native binary is painful.

> **This guide targets the head you deployed** (the production path in head-setup.md).
> The local dry-run stack doesn't host binaries, run a registry, or create an admin user,
> so a few steps below can't run against it — see [Testing locally](#testing-locally) at
> the end.

---

## How a leaf works

A leaf moves through states as you build it, and its work units move through states as
volunteers compute them:

```
Leaf:        DRAFT ──(configure)──► CONFIGURING ──(activate)──► ACTIVE
Work units:  QUEUED ──► ASSIGNED ──► COMPLETED ──► VALIDATED
```

The flow you'll follow:

```
1. build the compute artifact        (on your machine)
2. host it on your head              (binaries/ dir, or the registry)
3. create the leaf                   POST /api/v1/leafs
4. configure it                      POST .../configure  +  PUT /api/v1/leafs/{id}
5. activate it                       POST .../activate
6. generate work units               POST .../work-units/generate
7. volunteers compute                (they run the lettuce-volunteer CLI)
8. collect / aggregate results       GET .../results  /  POST .../aggregate
```

---

## Reporting progress (recommended)

Have your entrypoint report progress so contributors can see how far along a work
unit is. Periodically write a single number `0`–`100` (the percent complete) to the
file named by the `$LETTUCE_PROGRESS_FILE` environment variable. The volunteer reads
it and `lettuce-volunteer status` shows live progress and an ETA — without it, a
running unit shows a flat `0%` until it finishes.

The volunteer sets `$LETTUCE_PROGRESS_FILE` for **both** runtimes:

- **native** → `<work-dir>/progress.txt`
- **container** → `/work/output/progress.txt`

Both example programs in this guide already do it — write progress from the main loop,
make it best-effort (swallow any error; never fail the unit), and ideally write
atomically (temp file + rename) so a reader never sees a half-written value:

```go
// guides/examples/monte-carlo-pi/main.go (native, Go) — throttled to ~every 5s
progressFile := os.Getenv("LETTUCE_PROGRESS_FILE")
if progressFile != "" && time.Since(lastProgress) >= 5*time.Second {
    pct := float64(i+1) / float64(dartsPerTrial) * 100
    os.WriteFile(progressFile, []byte(fmt.Sprintf("%.1f", pct)), 0644)
    lastProgress = time.Now()
}
```

```python
# guides/examples/nbody-gravity/simulate.py (container, Python) — throttled to ~every 5s
progress_file = os.environ.get("LETTUCE_PROGRESS_FILE")
if progress_file and time.time() - last_progress >= 5.0:
    with open(progress_file, "w") as f:
        f.write(f"{(step + 1) / num_steps * 100:.1f}")
    last_progress = time.time()
```

---

## Checkpointing (optional, for long work units)

By default an interrupted work unit — the volunteer is stopped or updated, the
machine reboots, or the unit is reassigned to another volunteer — **starts over
from the beginning** and redoes all the work it had already done. For short
units that is fine. For long ones it wastes the contributor's compute and resets
the progress bar. A leaf that checkpoints resumes from roughly where it left off.

Checkpointing is **opt-in** and needs the leaf to cooperate; the infrastructure
cannot snapshot arbitrary in-progress computation for you. Enable it in the
leaf's `fault_tolerance_config` with `checkpointing_enabled: true` and
`checkpoint_interval_seconds` (minimum `60`) — that interval bounds how much work
is ever at risk — then honor the contract in the entrypoint:

- The runtime creates a per-unit checkpoint **directory** and names it in
  `$LETTUCE_CHECKPOINT_DIR` (native → `<work-dir>/checkpoint`; container →
  `/work/checkpoint`; wasm → `/work/checkpoint`). `$LETTUCE_CHECKPOINT_FILE` is a
  convenience path inside it for single-file state — the whole directory is what
  gets archived and restored.
- **Save** resumable state into that directory periodically (e.g. "completed N of
  M items" plus partial results). Write it **atomically** (temp file in the same
  directory, then rename over the target) so a checkpoint captured — or the
  process killed — mid-write never leaves a torn file for the next run.
- **Resume on startup**: if the directory already holds a checkpoint, load it,
  continue from there, and re-emit the matching `$LETTUCE_PROGRESS_FILE` value so
  progress does not reset. The same directory is populated whether the unit
  resumed on the same volunteer (its work dir is preserved) or was reassigned to
  a new one (the head's latest checkpoint is restored into it first), so one
  resume path covers both.

On a graceful stop the leaf is sent `SIGTERM` with a short grace window before it
is killed; trapping it to flush one final checkpoint reduces the work lost on a
clean stop to nearly zero (optional — the interval already bounds the loss).
Keep checkpoint writes best-effort (never fail the unit on a write error) and
within `max_checkpoint_size_bytes` (default 100 MB).

---

## Updating a leaf's artifact — versions & rollback

When you change a leaf's compute artifact (a new native binary, or a new container
image), publish it as an **immutable version** so already-RUNNING volunteers pick it up
automatically — no restart, no manual `podman pull`:

```
# 1. Update the artifact as usual: scp the new binary + set the new checksum, OR push
#    a NEW immutable image tag/digest; then PUT the new execution_config.
# 2. Publish it as a named, immutable version (activates it by default):
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/versions \
  -H "Authorization: Bearer $LETTUCE_ADMIN_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"version_label":"native-go-2.0","notes":"3D rewrite"}'
```

The head stamps the current version into every new assignment, so a running volunteer
runs the new artifact on its **next work request** (within the leaf-snapshot TTL, ~30s)
— no restart. Old in-flight work finishes on its pinned version, and redundant copies
of one work unit always run the same version (homogeneous redundancy).

- **List history:** `GET /api/v1/leafs/{id}/versions`
- **Roll back / re-point:** `POST /api/v1/leafs/{id}/versions/{version_id}/activate`
- **Container leaves must be immutable:** publishing **rejects a `:latest` image** —
  pin a digest (`repo@sha256:…`) or an immutable tag (`:2.0`). A re-pushed `:latest` is
  never re-pulled by volunteers, the exact bug this versioning fixes.
- **Don't garbage-collect a published image's blobs while units still reference it.**
  Work units are pinned to the artifact version that was current when they were
  generated. If you push a new image and a registry GC removes the **old version's
  blobs**, its manifest may still resolve (200) while its layers 404 — volunteers
  pinned to it can no longer pull, and the failure can surface only as a confusing
  `container create: no such image` at run time. If you intend the old image to go
  away, first move the affected QUEUED units onto the new version (regenerate them, or
  re-point them and restart the head); otherwise keep the old blobs (disable registry
  blob GC / `retention=all`). This bit a live head on 2026-06-27.

---

## Before you start

You'll run the API calls from anywhere with `curl` (your laptop or the server). Set two
variables — your head's URL and the admin API key you generated in head-setup.md, Step 6:

```bash
HEAD="https://your-domain.com"
ADMIN_KEY="<your LETTUCE_ADMIN_API_KEY from .env>"
```

Every leaf is owned by a user. Get your admin user's ID — run this **on the server**, from
the `lettuce-compute` directory:

```bash
CREATOR_ID=$(docker compose -f compose.production.yaml exec -T postgres \
  psql -U lettuce -d lettuce -tA -c "SELECT id FROM users WHERE role='ADMIN' ORDER BY created_at LIMIT 1")
echo "$CREATOR_ID"
```

That prints a UUID (e.g. `3f1c…`). The admin user was created automatically on first boot
from `LETTUCE_ADMIN_EMAIL`/`LETTUCE_ADMIN_PASSWORD`, so its ID is unique to your head.

---

## Path 1 — Native binary leaf

We'll use the **[monte-carlo-pi](examples/monte-carlo-pi/)** example: a tiny Go program
that estimates π by throwing random darts. Each work unit is one trial; the head averages
them into a final estimate of π.

### Step 1 — Build the binary

The program reads `LETTUCE_PARAMS_FILE` and writes its result to `LETTUCE_OUTPUT_FILE`.
Cross-compile for whatever platforms your volunteers run (Linux and Windows shown):

```bash
cd guides/examples/monte-carlo-pi
GOOS=linux   GOARCH=amd64 go build -o monte-carlo-pi-linux .
GOOS=windows GOARCH=amd64 go build -o monte-carlo-pi.exe   .
```

### Step 2 — Test it locally (recommended)

```bash
echo '{"seed": 42}' > params.json
LETTUCE_PARAMS_FILE=params.json LETTUCE_OUTPUT_FILE=out.json \
  LETTUCE_PROGRESS_FILE=progress.txt ./monte-carlo-pi-linux
cat progress.txt   # a number 0-100 (100 when done) — what `status` displays
cat out.json       # {"result": 3.14..., "seed": 42, ...}
```

If `out.json` has a `result` near 3.14, the binary honors the contract, and
`progress.txt` confirms it reports progress (see [Reporting progress](#reporting-progress-recommended)).

### Step 3 — Host the binary on your head

Copy the binaries into the head's `binaries/` directory — Caddy serves that directory at
`/binaries/`:

```bash
scp monte-carlo-pi-linux monte-carlo-pi.exe \
  root@your-domain.com:~/lettuce-compute/binaries/
```

Confirm they're reachable (expect `HTTP/2 200`):

```bash
curl -sI https://your-domain.com/binaries/monte-carlo-pi-linux | head -1
```

Now compute the SHA-256 of each binary. NATIVE leafs **require** a checksum per
platform — volunteers verify the download against it before running it and refuse
to execute an unverified or tampered binary (this protects volunteers from a
corrupted download, a MITM, or a compromised artifact host):

```bash
# Linux
sha256sum monte-carlo-pi-linux monte-carlo-pi.exe

# macOS
shasum -a 256 monte-carlo-pi-linux monte-carlo-pi.exe

# Windows PowerShell (one file at a time; output is UPPERCASE — lowercase it before pasting)
Get-FileHash -Algorithm SHA256 monte-carlo-pi-linux | ForEach-Object { $_.Hash.ToLower() }
Get-FileHash -Algorithm SHA256 monte-carlo-pi.exe   | ForEach-Object { $_.Hash.ToLower() }
```

Each digest is a **64-character lowercase hex** string — keep them for Step 5.
Compute them on the **exact file you uploaded** (the digest must match the
bytes served at the URL). If you computed on Windows, make sure you converted
the hash to lowercase: the server rejects uppercase or non-hex values at
configure time.

### Step 4 — Create the leaf

```bash
LEAF_ID=$(curl -s -X POST $HEAD/api/v1/leafs \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d "{
    \"name\": \"Monte Carlo Pi\",
    \"description\": \"Estimate the value of pi by random dart-throwing.\",
    \"research_area\": [\"mathematics\"],
    \"task_pattern\": \"MONTE_CARLO\",
    \"visibility\": \"PUBLIC\",
    \"creator_id\": \"$CREATOR_ID\"
  }" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "Leaf: $LEAF_ID"
```

The leaf starts in `DRAFT`. (`research_area` values are slugs — `mathematics`, `physics`,
`biology`, `chemistry`, `climate`, `computer-science`, `engineering`, `ml-ai`, …)

### Step 5 — Configure it

Move the leaf to `CONFIGURING`, then set how it runs and how results are validated:

```bash
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/configure \
  -H "Authorization: Bearer $ADMIN_KEY" > /dev/null

curl -s -X PUT $HEAD/api/v1/leafs/$LEAF_ID \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{
    "execution_config": {
      "runtime": "NATIVE",
      "binaries": {
        "linux_amd64":   "'"$HEAD"'/binaries/monte-carlo-pi-linux",
        "windows_amd64": "'"$HEAD"'/binaries/monte-carlo-pi.exe"
      },
      "binary_checksums": {
        "linux_amd64":   "PASTE_SHA256_OF_monte-carlo-pi-linux_HERE",
        "windows_amd64": "PASTE_SHA256_OF_monte-carlo-pi.exe_HERE"
      },
      "max_memory_mb": 256,
      "max_disk_mb": 64,
      "max_cpu_seconds": 30
    },
    "validation_config": {
      "redundancy_factor": 1,
      "agreement_threshold": 1.0,
      "comparison_mode": "NUMERIC_TOLERANCE",
      "numeric_tolerance": 0.01,
      "max_retries": 3
    },
    "fault_tolerance_config": {
      "deadline_multiplier": 3.0,
      "max_reassignments": 3
    },
    "data_config": {
      "transfer_strategy": "INLINE",
      "aggregation_format": "JSON",
      "aggregation_config": {
        "aggregator_type": "all",
        "output_field": "result",
        "confidence_level": 0.95
      },
      "max_input_size_bytes": 1048576,
      "max_output_size_bytes": 10485760
    }
  }' > /dev/null
```

> The `'"$HEAD"'` bits splice your `$HEAD` variable into the JSON. If that quoting looks
> fragile, just paste the full `https://your-domain.com/binaries/...` URLs directly.

What the key settings mean:

| Setting | Meaning |
|---|---|
| `runtime: NATIVE` | Volunteers run a compiled binary (matched to their OS/arch). |
| `binaries` | Download URLs per platform. Volunteers fetch the one that matches them. |
| `binary_checksums` | SHA-256 (lowercase hex) of each binary. **Required for NATIVE.** Volunteers verify the download and refuse to run a binary whose hash doesn't match. |
| `max_cpu_seconds: 30` | Volunteer kills the binary after 30s of CPU time. |
| `redundancy_factor: 1` | Each work unit goes to one volunteer. Set 2+ and the unit is sent to that many volunteers **in parallel**; their results are cross-checked and the unit validates once `agreement_threshold` of them agree (e.g. `3` + `0.67` ⇒ 2 of 3). This is the simple knob: it sets **both** how many copies to dispatch and how many must agree. To decouple them, use `target_copies` + `min_quorum` below. |
| `target_copies` / `min_quorum` (optional) | Split `redundancy_factor` into how many copies to **dispatch** (`target_copies`) vs how many agreeing results **validate** (`min_quorum`), with `min_quorum ≤ target_copies`. Setting `target_copies` higher than `min_quorum` over-dispatches and validates as soon as a quorum agrees, **without waiting for the stragglers** — it absorbs vanished/slow volunteers without a serial re-dispatch round-trip (e.g. `target_copies: 3, min_quorum: 2` ⇒ send 3, validate on the first 2 that agree; the extra copy is dropped without penalty). Omit both ⇒ `target_copies = min_quorum = redundancy_factor` (unchanged behavior). |
| `comparison_mode: NUMERIC_TOLERANCE` | Results agree if within `numeric_tolerance`. Monte Carlo is stochastic, so exact match won't do. |
| `agreement_threshold: 1.0` | Fraction of the redundant copies that must agree to validate (the quorum). `1.0` = unanimous. |
| `max_total_copies` / `max_error_copies` (optional) | Hard caps that bound a non-converging unit. `max_total_copies` is the dead-letter ceiling (default `target_copies + 6`): once this many copies have been created with the quorum still unmet, the unit is parked `FAILED`. `max_error_copies` bounds timed-out/abandoned/disagreeing copies (default unlimited); when set it must be at least `target_copies`, so an honest run of expiries alone cannot trip it. Both operator-tunable per leaf; omit for the defaults. |
| `deadline_multiplier: 3.0` | Sets each work unit's timeout. By default `deadline_seconds = 3600 × multiplier` (so `3.0` = 3h, `0.5` = 30min); set an explicit `deadline_seconds` (next row) to give an absolute deadline instead. Any value, no cap. **Stamped at generation** — changing it only affects newly generated units. A copy not returned by its deadline is redispatched to another volunteer (no per-attempt cap; a hopeless unit eventually dead-letters after `redundancy_factor + 6` total copies). `max_reassignments` is a deprecated no-op, kept only so older configs still validate. |
| `deadline_seconds` (optional) | An absolute per-work-unit deadline in seconds that **overrides** `deadline_multiplier` when set — use it to match the deadline to how long a unit really takes (and to how long your volunteers tend to pause), instead of the fixed 3600s baseline. Must be > 0; for no hard deadline use `no_deadline: true` instead. At activation the head logs the resolved deadline and **warns when it is shorter than `max_cpu_seconds`** — the case where a unit that uses its full CPU budget could never be returned in time. |
| `aggregation_config.output_field: "result"` | The JSON field the aggregator reads from each result (the π estimate). |
| `max_output_size_bytes: 10485760` | Hard cap (bytes) on a single result payload — the server **rejects** larger submissions. Must be > 0; size it to the largest reasonable result for this leaf. |

### Step 6 — Activate and generate work units

```bash
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/activate \
  -H "Authorization: Bearer $ADMIN_KEY" > /dev/null

curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/work-units/generate \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"parameter_space": {"num_trials": 100}}' | python3 -m json.tool
```

This creates 100 work units, each a Monte Carlo trial with its own seed.

### Step 7 — Verify

```bash
# Leaf should be ACTIVE
curl -s $HEAD/api/v1/leafs/$LEAF_ID | python3 -m json.tool | grep '"state"'

# Work units should be QUEUED
curl -s "$HEAD/api/v1/leafs/$LEAF_ID/work-units?state=QUEUED&limit=3" \
  -H "Authorization: Bearer $ADMIN_KEY" | python3 -m json.tool
```

Or open `https://your-domain.com/dashboard/leafs` and you'll see the leaf listed as
ACTIVE with queued work.

### Step 8 — Compute and aggregate

Attach a volunteer (your own machine works, to see it end-to-end):

```bash
lettuce-volunteer init --server your-domain.com
lettuce-volunteer start
```

Once work units finish, aggregate the trials into a final answer:

```bash
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/aggregate \
  -H "Authorization: Bearer $ADMIN_KEY" | python3 -m json.tool
```

You should see a `mean` close to **3.14159** with a tight confidence interval. That's your
first leaf, computed by volunteers and aggregated by your head.

---

## Path 2 — Container leaf

When cross-compiling is painful, ship a container image instead. We'll use the
**[nbody-gravity](examples/nbody-gravity/)** example — a Python + NumPy gravitational
simulation. (Any language works; the container just has to honor the contract.)

The container contract differs slightly from native: read parameters from
`LETTUCE_PARAMETERS_FILE`, write results to `LETTUCE_OUTPUT_DIR/output.json`.

Build and push with **Podman** (the runtime volunteers use). `docker` is a drop-in
alternative for every command below.

### Step 1 — Build the image

Tag it with your head's domain so it can be pushed to the head's registry:

```bash
cd guides/examples/nbody-gravity
podman build -t your-domain.com/nbody:latest .
```

### Step 2 — Test it locally (recommended)

```bash
mkdir -p /tmp/nbody/input /tmp/nbody/output
echo '{"num_bodies":100,"spread":5.0,"velocity_scale":0.5,"mass_distribution":"uniform","timestep":0.01,"num_steps":1000}' \
  > /tmp/nbody/input/parameters.json

podman run --rm \
  -v /tmp/nbody/input:/work/input:ro \
  -v /tmp/nbody/output:/work/output \
  -e LETTUCE_PARAMETERS_FILE=/work/input/parameters.json \
  -e LETTUCE_OUTPUT_DIR=/work/output \
  -e LETTUCE_PROGRESS_FILE=/work/output/progress.txt \
  your-domain.com/nbody:latest

cat /tmp/nbody/output/progress.txt   # a number 0-100 (100 when done) — what `status` displays
cat /tmp/nbody/output/output.json
```

### Step 3 — Push to your head's registry

Your head runs an image registry at `/v2/`. Push needs the registry credentials from
head-setup.md, Step 6 (volunteers pull anonymously):

```bash
podman login your-domain.com -u lettuce        # password = your REGISTRY password
podman push your-domain.com/nbody:latest
```

### Step 4 — Create the leaf

```bash
LEAF_ID=$(curl -s -X POST $HEAD/api/v1/leafs \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d "{
    \"name\": \"N-Body Gravity\",
    \"description\": \"Gravitational N-body star cluster simulation.\",
    \"research_area\": [\"physics\"],
    \"task_pattern\": \"PARAMETER_SWEEP\",
    \"visibility\": \"PUBLIC\",
    \"creator_id\": \"$CREATOR_ID\"
  }" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "Leaf: $LEAF_ID"
```

### Step 5 — Configure it (CONTAINER runtime)

Container leafs use `image` instead of `binaries`. The simulation is deterministic, so
results can be compared with `EXACT`:

```bash
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/configure \
  -H "Authorization: Bearer $ADMIN_KEY" > /dev/null

curl -s -X PUT $HEAD/api/v1/leafs/$LEAF_ID \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{
    "execution_config": {
      "runtime": "CONTAINER",
      "image": "your-domain.com/nbody:latest",
      "gpu_required": false,
      "max_memory_mb": 512,
      "max_disk_mb": 256,
      "max_cpu_seconds": 300
    },
    "validation_config": {
      "redundancy_factor": 1,
      "agreement_threshold": 1.0,
      "comparison_mode": "EXACT",
      "max_retries": 3
    },
    "fault_tolerance_config": {
      "deadline_multiplier": 3.0,
      "max_reassignments": 3
    },
    "data_config": {
      "transfer_strategy": "INLINE",
      "max_input_size_bytes": 1048576,
      "max_output_size_bytes": 10485760
    }
  }' > /dev/null
```

> The `image` value is just the registry path **without** the `https://` scheme — e.g.
> `your-domain.com/nbody:latest`. Volunteers without a container runtime skip container
> leafs automatically.

### Step 6 — Activate and generate work units

Start small. The parameter space is expanded into a **Cartesian product** — one work unit
per combination:

```bash
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/activate \
  -H "Authorization: Bearer $ADMIN_KEY" > /dev/null

curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/work-units/generate \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{
    "parameter_space": {
      "num_bodies": [100, 200],
      "spread": [5.0],
      "velocity_scale": [0.5],
      "mass_distribution": ["uniform"],
      "timestep": [0.01],
      "num_steps": [1000]
    }
  }' | python3 -m json.tool
```

That's 2 × 1 × 1 × 1 × 1 × 1 = **2 work units**. Add more values to any list to grow the
sweep.

### Step 7 — Verify

Same as the native path — check the leaf is `ACTIVE` and work units are `QUEUED`, then
attach a volunteer with a container runtime to compute them:

```bash
curl -s $HEAD/api/v1/leafs/$LEAF_ID | python3 -m json.tool | grep '"state"'
curl -s "$HEAD/api/v1/leafs/$LEAF_ID/work-units?state=QUEUED" \
  -H "Authorization: Bearer $ADMIN_KEY" | python3 -m json.tool
```

---

## Operating your leaf

```bash
# Pause (stop handing out new work) / resume
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/pause  -H "Authorization: Bearer $ADMIN_KEY"
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/resume -H "Authorization: Bearer $ADMIN_KEY"

# Add more work units later (same endpoint, new parameters)
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/work-units/generate \
  -H "Content-Type: application/json" -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"parameter_space": {"num_trials": 50}}'

# Collect validated results
curl -s "$HEAD/api/v1/leafs/$LEAF_ID/results" \
  -H "Authorization: Bearer $ADMIN_KEY" | python3 -m json.tool

# Archive when you're done (after collecting results)
curl -s -X POST $HEAD/api/v1/leafs/$LEAF_ID/archive -H "Authorization: Bearer $ADMIN_KEY"
```

---

## Testing locally

The local dry-run stack from head-setup.md (`compose.yaml`) is great for confirming the
server runs, but it can't do the full leaf flow above, because it has **no Caddy/binaries
directory, no registry, and no admin user**. To experiment entirely locally you'd need to:

1. **Create an admin user** so you have a `creator_id`: add `LETTUCE_ADMIN_EMAIL` and
   `LETTUCE_ADMIN_PASSWORD` to the `infrastructure` service in `compose.yaml`, then
   `docker compose up -d`. Look up the ID with the same `psql` query as above (using
   `docker compose` without the `-f compose.production.yaml` flag).
2. **Host the binary yourself** — e.g. `python3 -m http.server` in a directory, and point
   `binaries` at `http://<your-ip>:8000/...`.
3. Use `HEAD="http://localhost:8080"` and the dev admin key
   `dev-admin-key-not-for-production`.

In practice, most people just run the walkthrough against their real head. When in doubt,
deploy the head (it's ~20 minutes) and create your first leaf there.
