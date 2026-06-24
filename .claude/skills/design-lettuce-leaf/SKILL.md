---
name: design-lettuce-leaf
description: >-
  Help a researcher translate a domain wish ("I want to simulate X" / "I want
  to know whether Y") into a concrete Lettuce-shaped computation spec — choose
  the model, define parameters that vary across work units, define the output
  schema, pick a validation strategy, and pick a runtime — before any code is
  written. Produces a leaf-spec.md document the `create-lettuce-leaf` skill
  consumes. Use whenever a user arrives with a research question rather than a
  runnable method; trigger phrases include "I want to compute…",
  "I want to simulate…", "I want a leaf that does…", "help me design a leaf",
  "what should my leaf look like".
---

# Design a Lettuce Leaf (agent-guided)

You are helping a researcher turn a research question into a runnable **leaf
specification**. The output of this skill is **not** a leaf — it's a written
spec that the `create-lettuce-leaf` skill will consume to build the binary,
configure the leaf, and queue work units.

Land the design first. Hand off second.

## What this skill IS and IS NOT

It IS:
- A scoping conversation that turns a domain wish into a runnable spec
- A decision-tree for runtime / validation / output schema / parameter sweep
- A writer of a `leaf-spec.md` document the next skill consumes

It is NOT:
- The implementation skill (that's `create-lettuce-leaf` — call it after this)
- A research-design tool for arbitrary scientific questions (you don't decide
  whether the question is interesting, only whether it's *expressible as a
  Lettuce leaf*)
- A replacement for the researcher's domain expertise — when their model
  conflicts with what you'd guess, theirs wins

## How to behave

1. **One step at a time.** Eight numbered steps below. Finish each one — get
   a sentence-or-two answer from the user, confirm your understanding — then
   move on. Never paste the whole flow at once.
2. **Use plain language.** "What are you trying to find out?" not "What is
   the cost function over the parameter manifold?"
3. **Always offer an "I don't know / help me choose" option** on every
   decision. If they take it, explain the trade-offs and recommend one for
   their situation, then proceed with that recommendation unless they push
   back.
4. **Anchor on a worked example.** When a choice is abstract, give a
   concrete example from a different domain. "Like the Ising-model leaf
   sweeps lattice_size × temperature × seed — what are your three axes?"
5. **Refuse silently to invent science.** If the user says "I want to
   simulate beyblades fighting" and they have no model in mind, do not
   guess the physics. Ask them what counts as a fight, what they want to
   compare, what would make a result interesting. Their answers shape the
   model — you don't get to pick it for them.
6. **Stop early if the wish can't be a leaf.** A leaf has to be:
   - Decomposable into independent work units (no shared state across them)
   - Each unit completes in bounded time (~seconds to ~hours, not days)
   - Output fits in `max_output_size_bytes` (default 10 MiB; cap is ~100 MiB)
   - Reproducible enough that validation can compare two runs
   If any of those fails, name it and offer to redesign the question, not the
   spec.
7. **Produce a spec, not a binary.** No code in this skill. The closest you
   get is the output-schema example JSON.

## The flow

### 0. Pick a slug (the leaf's directory name)

Before any design questions: ask the user for a **slug** — the directory
this leaf will live in under `leafs/`. Convention:

- Lowercase, kebab-case.
- Short and stable: `grep`, `ising-model`, `mandelbrot`, `protein-fold`.
- Singular, no project suffixes (`grep`, not `grep-leaf` or `grep-2`).
- Once chosen, don't change it — every skill in the chain keys off this.

Then create the directory:

```bash
mkdir -p lettuce-compute/leafs/<slug>
```

Everything this skill writes goes inside that directory. The slug is the
**only** argument the next two skills (`create-lettuce-leaf`,
`register-leaf-on-scios`) need from the user — they discover the spec
and the leaf identity from files inside it.

### 1. The question (one sentence)

What do they want to know or produce? Write it down as one sentence, in
their words. Examples:

- "I want to map the phase transition of a 2D Ising ferromagnet."
- "I want to know which beyblade design wins the most matches."
- "I want to find pairs of large primes with small gaps between them."
- "I want to render Mandelbrot tiles at deep zoom levels."

If they can't answer this in one sentence yet, *help them write it* — but
the sentence is theirs, not yours. Read it back.

### 2. The model (the method)

How is the question answered computationally? Two cases:

- **They have a model already** ("Monte Carlo Metropolis algorithm",
  "rigid-body 2D collision sim", "Sieve of Eratosthenes + gap scan"):
  write it down, ask one or two clarifying questions, move on.
- **They don't** ("I want simulated beyblades but I don't know what
  simulation"): walk through 2–3 candidate models with their trade-offs.
  Recommend the *simplest* one that answers the question. Mention what
  you're trading away. Land on one model before moving on.

The model is the contract for the rest of the design — it determines what
parameters mean, what "an output" looks like, and whether the computation
is deterministic.

### 3. The work-unit shape (what varies, what stays)

A leaf is many independent runs of the same model with different inputs.
Identify:

- **The axes that vary** across work units (parameters in the parameter
  sweep). Typically 2–4 axes. Each axis is either an explicit list
  (`[100, 200, 300]`) or a range (`{min: 1.0, max: 10.0, step: 0.5}`).
- **What stays fixed** across work units (constants baked into the binary
  or into a single input file): model coefficients, lattice topology,
  shared dataset.

Compute the Cartesian product size and state it: *"3 lattice sizes × 16
temperatures × 5 seeds = 240 work units."* If the product is enormous
(>10⁵), help them subset before moving on.

**The seed axis is special**: stochastic models need independent RNG
seeds per work unit (typically 3–10 per parameter combination) to
average out per-run noise.

### 4. The output schema (what comes back per work unit)

A JSON object. Pin down the **field names, types, and meanings** now.
Examples:

```jsonc
// Ising
{
  "lattice_size": 32,
  "temperature": 2.269,
  "seed": 42,
  "mean_energy": -1.425,
  "mean_magnetization": 0.223,
  "specific_heat": 1.926,
  "binder_cumulant": 0.601,
  "acceptance_rate": 0.194
}
```

Constraints:
- Must fit under `max_output_size_bytes` (default 10 MiB; head enforces
  hard). For most leafs this is trivial; for image/log-heavy output, plan
  ahead (downsample, compress, or chunk into more work units).
- If using `comparison_mode: "EXACT"`, output must be **deterministic**:
  no timestamps, no map iteration order, sorted arrays, no
  floating-point non-determinism.
- Echo back the input parameters in the output (`lattice_size`,
  `temperature`, `seed` in Ising). This is the only way to join results
  back to inputs after aggregation.

State explicitly which fields are the **science** (mean_magnetization,
specific_heat) vs the **provenance** (input echoes, compute_time_ms,
seed).

### 5. Validation strategy

This is the head's "did two volunteers agree?" check. Pick from the
table:

| Computation type | `redundancy_factor` | `comparison_mode` | Notes |
|---|---|---|---|
| Deterministic CPU code (e.g. WASM, fixed-seed Go) | 1 | `EXACT` | Head spot-checks ~5% automatically |
| Stochastic CPU (Monte Carlo, MCMC) | 1 | `NUMERIC_TOLERANCE` | Tolerance ~0.01 of the typical signal |
| GPU computation | 2 | `NUMERIC_TOLERANCE` | GPU math is never bit-identical across hardware |
| High-stakes results | 2–3 | `EXACT` (if deterministic) or `NUMERIC_TOLERANCE` | Two/three volunteers must agree |

**Default for a first leaf:** `redundancy_factor: 1` (no double-running)
with `comparison_mode` picked from determinism.

**Caveat (open question on this head as of 2026-05):** Results with
`redundancy_factor: 1` may park at `validation_status: PENDING`
indefinitely instead of auto-validating. See
`lettuce-compute/TODO.md` #12. If you want results to definitely flip
to `VALIDATED`, use `redundancy_factor: 2` until that's resolved.

### 6. Runtime (NATIVE / CONTAINER / WASM)

| Code shape | Pick |
|---|---|
| Pure Go/Rust/C, no system deps | NATIVE (or WASM, if browser reach matters) |
| Python / R / Julia / heavy library deps | CONTAINER |
| Needs NVIDIA/AMD GPU | CONTAINER (with `gpu_required: true`) |
| Needs >4 GB memory and not WASM | NATIVE or CONTAINER |
| You want browser volunteers to be able to run it | WASM |
| You don't know | CONTAINER (broadest fit; fewest sharp edges) |

**Constraints:**
- NATIVE requires per-platform builds and a per-platform SHA-256 in
  `binary_checksums`. The head rejects NATIVE configs without it.
- CONTAINER images can be huge — the GREP image is ~91 GB on disk and
  requires ≥120 GB free on the volunteer. Note this if your image is
  going to be big.
- WASM has a hard 4 GB memory ceiling and no network.

**Progress reporting (design it in now).** Every leaf should emit progress so
`lettuce-volunteer status` shows live progress and an ETA instead of a flat
`0%`: the entrypoint periodically writes a single number `0`–`100` to
`$LETTUCE_PROGRESS_FILE` (the runtime sets it for every runtime — native
default `<work-dir>/progress.txt`, container `/work/output/progress.txt`).
Decide the **progress signal** now — the loop the entrypoint will count
(seeds, time-steps, items, rows) — and record it in the spec so the next skill
writes it in from the start rather than bolting it on (the gap that shipped the
Beyblade container leaf with no progress). If a work unit is a single
indivisible step with no natural loop, say so and it's fine to skip.

### 7. Cost shape (per-unit duration, total count, deadline strategy)

Three things:

- **Estimated wall-clock per work unit** (in seconds). Researcher guesses;
  you confirm during the smoke-test phase later. This becomes the leaf's
  `estimated_duration_seconds` (it also sizes how much work volunteers buffer).
- **Total work units** (from step 3).
- **Deadline strategy.** Liveness is deadline-based: the head reassigns any
  work unit not submitted by its deadline — there are NO per-task heartbeats.

For long-running leafs (hours+), set `no_deadline: true` so a wall-clock
deadline doesn't kill genuine work; the head then reclaims a unit only after a
generous ceiling (`no_deadline_ceiling_seconds`, default 6 h). (This is the
GREP pattern.)

(The leaf config still carries a legacy `heartbeat_interval_seconds` field that
no longer drives liveness; the next skill fills a safe default, so you don't
design around it.)

### 8. Aggregation (what does "the result" mean across all work units?)

A leaf isn't useful until you can answer: *"What's the answer to the
question in step 1, given N validated work-unit outputs?"*

Typical patterns:

- **Plot / table:** group results by the input axes, render. (Ising:
  C(T) curves for each lattice size.)
- **Aggregate statistic:** sum, mean, or weighted average across all
  units. (Monte Carlo π: mean of all in-circle ratios.)
- **Filter:** keep only the outputs that match some criterion. (Prime
  gaps: keep gaps below a threshold.)
- **Reduce to a single value:** the leaf converges on one number or
  decision.

If the aggregation is a pure post-hoc plot the researcher generates,
say so — no `aggregation_config` needed on the head. If it's something
the head should compute (sum, mean), name the output field and
aggregator type now.

## The output: leaf-spec.md

After step 8, write the spec to **`lettuce-compute/leafs/<slug>/leaf-spec.md`**
(the slug from Step 0). Always this exact path — the next skill reads
from there. Use this template:

```markdown
# Leaf spec — <one-line name>

**Question:** <one sentence from step 1>

**Model:** <method from step 2, with key references if any>

## Work units
- **Axes:** <list each axis with values or range>
- **Fixed:** <list constants>
- **Total:** <Cartesian product count>
- **Seed axis?** <yes/no — how many seeds per combination>

## Output schema
```jsonc
{ ... }   // from step 4, with comments distinguishing science vs provenance
```

## Validation
- `redundancy_factor:` <1 / 2 / 3>
- `comparison_mode:` <EXACT / NUMERIC_TOLERANCE>
- `numeric_tolerance:` <if applicable>
- Determinism notes: <whether output is reproducible>

## Runtime
- Choice: <NATIVE / CONTAINER / WASM>
- Why: <one sentence>
- Estimated artifact size: <MB or GB if large>
- Progress signal: <the loop/iteration the entrypoint reports 0-100 from —
  seeds / time-steps / items; or "none — single indivisible step">

## Cost
- Per-unit estimate: <seconds / minutes>  (→ estimated_duration_seconds)
- Total work units: <N>
- Deadline strategy: <default deadline / no_deadline for long-running>
- `no_deadline:` <true/false>

## Aggregation
- Pattern: <plot / sum / mean / filter / reduce>
- Head-side aggregation: <yes (field, type) / no (researcher post-hoc)>

## Open questions
- <anything the researcher said "I'm not sure" about and needs to
  decide before the binary is written>
```

Read it back. Ask for "looks right / change X / start over." Don't
move on until they bless it.

## Handoff to create-lettuce-leaf

Once `leafs/<slug>/leaf-spec.md` is approved, tell the user:

> *"The design is saved at `lettuce-compute/leafs/<slug>/leaf-spec.md`.
> When you're ready, run `create-lettuce-leaf` and give it the same
> slug — it'll read the spec from there, build the wrapper into the
> same directory, host it on the head, and persist the leaf identity
> back to `leafs/<slug>/.lettuce.json` for the next skill in the chain."*

The handoff is **file-based**: the next skill takes one argument (the
slug) and discovers everything else by reading files in the leaf's
directory. No re-asking, no pasting between sessions.

## Refuse-to-design list

Some wishes don't fit a leaf shape; recognize them early and redirect:

- **Real-time / streaming workloads** (chat, dashboards, anything
  with an SLA on individual responses). Lettuce work units are
  fire-and-forget batch jobs.
- **Anything requiring shared state across work units mid-run**
  (live database writes, cross-volunteer communication). Work units
  are independent — if they need to talk, the design is wrong.
- **Tasks needing inputs >100 MiB per unit unless they can be hosted
  externally** (use `transfer_strategy: EXTERNAL_REFERENCE` and a
  CDN/object-store URL; same pattern as the GREP leafs).
- **Outputs that are inherently huge** (raw video frames, full HDF5
  arrays). Have the binary write to external storage and return
  only a reference + summary.
- **Wishes that aren't actually computational** ("I want to interview
  scientists about X"). Different tool entirely.

When you spot one of these, name what's wrong and offer to redesign
the question — not the leaf.
