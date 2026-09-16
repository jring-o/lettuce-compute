---
name: create-lettuce-leaf
description: >-
  Guide a non-technical user through creating a leaf (a computation) on their Lettuce
  head: turning their code into a compute binary or container that honors the Lettuce
  contract, hosting it on the head, then creating, configuring, activating the leaf and
  generating work units. The agent does the technical work itself and asks the user only
  for what to compute, which parameters to sweep, and screenshots/approvals, one step at
  a time, verifying each. Use when the user wants to create a leaf, set up or run a
  computation/job/experiment on Lettuce, get their code running on volunteers, or add a
  computation to their head.
---

# Create a Lettuce Leaf (agent-guided)

You are helping a **non-technical researcher** turn a computation into a running **leaf** on
their Lettuce **head** (server). They are driving you, a terminal agent. Do as much as
possible yourself.

> A **leaf** is one computation; a **head** hosts many. This needs a running head first —
> if they don't have one, use the `deploy-lettuce-head` skill before this.

## Source of truth

The verified, exact procedure is in **`guides/first-leaf.md`** — the native path, the
container path, the compute contract, and the configs. Read it before you start and use its
commands. If anything here disagrees, re-read the guide — its facts win. Minimal working
examples are in `guides/examples/` (`monte-carlo-pi` native, `nbody-gravity` container).

## How to behave (re-read this every time before acting)

1. **One step at a time.** Never paste a wall of commands or a long plan. One action, confirm
   it worked, then the next.
2. **Do everything you can yourself.** You have a terminal. Write/adapt the code, build, test,
   host, and make every API call. Involve the user only where a human is needed (see below).
3. **Plain language.** One sentence on what you're about to do and why, before doing it.
4. **Verify before proceeding.** After each step, run its check. Fix failures from the real
   output; never move forward on a broken step.
5. **Make user steps trivial.** Exact instructions, then ask for paste-back or a screenshot.
6. **Always offer an "I don't know" choice.** Whenever you ask the user to pick between
   options — native vs. container, validation mode, research area, parameter values,
   anything — include an explicit **"Not sure / help me choose"** option. If they take it,
   explain the trade-offs in plain language and recommend one for their situation, then
   proceed with that recommendation unless they push back.
7. **Confirm before generating a large sweep** (it can create thousands of work units) or
   anything hard to undo.
8. **Never print secrets into the chat** (see *Secret handling*).
9. **When the user must save a secret, make it impossible to lose.** Don't just say "save
   it." Give explicit storage instructions (password manager: Bitwarden free, 1Password,
   or KeePass) with the exact label to use, then call **`AskUserQuestion`** to confirm
   they've saved it before continuing. For this skill, the main one is the **registry
   push password** they saved during head setup — if they can't find it, help them rotate
   it on the server before attempting a container push, rather than guessing.

## Who does what

| The USER does (you instruct + verify) | YOU do |
|---|---|
| Describe what they want to compute (and share their code, if any) | Adapt their code to the compute contract |
| Decide which parameter values to run | Build the binary or container image; test it locally |
| Approve before a big sweep; paste output / screenshots | Host it on the head; create/configure/activate the leaf; generate work units; verify |

## Before you start — gather head details yourself

You deployed the head, so collect these without bothering the user. Run API calls **on the
server over SSH** so the admin key never leaves it:

```bash
ssh root@<IP> 'set -a; . ~/lettuce-compute/.env; set +a; \
  curl -s -H "Authorization: Bearer $LETTUCE_ADMIN_API_KEY" https://<domain>/api/v1/health'
```

- **Domain / HEAD url:** from `.env` `PLATFORM_URL`.
- **Admin key:** stays in `.env` on the server; source it inside the SSH command as above.
- **`creator_id`:** the admin user's id — run the `psql` query from first-leaf.md
  *Before you start* on the server.

## The flow

### 0 — Orient

**First, before any chat: sever the local clone's upstream (if it's still connected).**
This skill is normally invoked from inside a local clone of `lettuce-compute`, and during
leaf creation you'll be adapting code, writing scratch files, and possibly handling the
registry password in this folder. An accidental `git push` from here must not be able to
reach the public upstream:

```bash
git remote -v                # see what's connected (run in the current working directory)
git remote remove origin     # if origin still points to jring-o/lettuce-compute (or a fork)
git remote -v                # verify — should print nothing
```

- If there are already no remotes, you're good — say "already disconnected" and move on.
  (The user likely ran the `deploy-lettuce-head` skill earlier, which sets this up.)
- If `origin` points somewhere other than `jring-o/lettuce-compute` (e.g. their own fork
  they actively work in), **ask before removing it** — offer *Remove it* / *Keep it (I
  know what I'm doing)* / *Help me decide*.
- This only affects the **local** working copy. The clone on the head's server keeps its
  `origin` so the user can `git pull` updates in the future.

Then tell the user plainly what's coming: "Now that your head is running, I'll help you
turn a computation into a **leaf** that volunteers can run. I'll do the technical work —
you just tell me what you want to compute and approve a few choices along the way."

Then give them the escape hatch, in their own words: *"If at any point I say something you
don't understand — a command, an acronym, a button name, anything — copy what I said,
paste it back to me, and tell me 'I don't know what this means'. I'll explain it in plain
language and help you decide. There are no dumb questions, and this offer stands for the
whole session."* Say this once, up front, so they know it's always available.

### A — What slug? Read the spec.

**First ask:** *"What's the slug of the leaf you want to build?"* (One
word, kebab-case, the directory name under `leafs/`.)

Then check whether the design has already been done:

```bash
ls lettuce-compute/leafs/<slug>/leaf-spec.md 2>/dev/null
```

- **If the spec exists:** read it. It answers what's being computed,
  the parameter sweep, the output schema, the validation strategy, the
  runtime, the cost shape, and the aggregation. Don't re-ask any of
  those questions; confirm the spec with the user in one sentence
  (*"I see a spec for ising-model — Metropolis MC sweep over
  lattice_size × temperature × seed, WASM runtime, redundancy 1, EXACT
  comparison. Sound right?"*) and proceed to Step B already knowing
  most of the answers.
- **If the spec doesn't exist:** offer to run `design-lettuce-leaf`
  first. It's the conversation that produces `leaf-spec.md`. If the
  user wants to skip the design step entirely, proceed below — but
  flag that you'll be improvising decisions the spec would have
  pinned down.

Everything you write — the entrypoint, Dockerfile, build script,
sample params, READMEs — goes into `lettuce-compute/leafs/<slug>/`.
Don't scatter files across the repo.

### A.1 — What are we computing? (fallback if no spec)
Ask, in plain terms, what they want to compute. Three cases:
1. **They have code** → you adapt it to the contract (Step C).
2. **They have a method but no code** → you write it with them, honoring the contract.
3. **They just want to try** → offer an example (`monte-carlo-pi` native, or `nbody-gravity`
   container) and use it.

### B — Which runtime? (you decide, tell them why)

There are three, and the choice decides **how many volunteers can ever run this leaf**. Each
volunteer grants trust per head and per runtime, so the runtime is a reach decision before it
is a technical one. Say this to the user in plain language rather than just picking.

- **WASM** — reaches the most volunteers, because it is sandboxed and is the one runtime every
  volunteer allows by default with no action from them. Also the only runtime browser
  volunteers can run at all. Choose it when the code compiles to WebAssembly (Go, Rust, C/C++
  via WASI), needs no network, and fits in ~4 GB of memory.
- **CONTAINER** — Python / R / Julia, or heavy library and system deps, with no
  cross-compilation. Each volunteer must opt this head in with
  `lettuce-volunteer heads trust <head> container`, so reach is narrower than WASM.
- **NATIVE** — a compiled binary running directly on the volunteer's machine with no sandbox.
  **Off by default for every volunteer**, and each one must explicitly grant
  `lettuce-volunteer heads trust <head> native`. Reaches the fewest volunteers. Choose it only
  when WASM cannot work and a container is impractical.

Constraints that decide it for you: **network access requires CONTAINER** (WASI has no network
APIs, and the head rejects `network_access` on a WASM leaf), and **a GPU leaf must be CONTAINER
or WASM** — the head rejects `gpu_required` on NATIVE, because native binaries get no device
passthrough.

If NATIVE is genuinely the right answer, tell the user the consequence in one sentence: their
volunteers will fetch nothing from this leaf until each of them runs the `heads trust` command,
and until then it will look like the leaf is broken.

### C — Make it honor the contract (the important part)
The program must:
- **NATIVE:** read params from `$LETTUCE_PARAMS_FILE` (JSON), write results to
  `$LETTUCE_OUTPUT_FILE` (JSON), exit 0.
- **CONTAINER:** read `$LETTUCE_PARAMETERS_FILE` (`/work/input/parameters.json`), write
  `$LETTUCE_OUTPUT_DIR/output.json`, exit 0.
- **WASM:** same variable names as native, at fixed paths inside the sandbox's virtual
  filesystem — `$LETTUCE_PARAMS_FILE` is `/work/params.json` and `$LETTUCE_OUTPUT_FILE` is
  `/work/output.dat`. Compile with WASI support and read and write those paths normally.

**`guides/first-leaf.md` documents the native and container paths only; there is no WASM
walkthrough yet.** For a WASM leaf, take the contract above and the hosting step from the
native path (it is the same `binaries/` directory and the same checksum step), and tell the
user plainly that you are following the contract from source rather than a written guide.

**Always add progress reporting (both runtimes — do this every time, not as an
afterthought).** Whenever you write or wrap an entrypoint, make it periodically write a
single number `0`–`100` (percent complete) to `$LETTUCE_PROGRESS_FILE`. The volunteer reads
it so `lettuce-volunteer status` shows live progress + ETA; a leaf that skips it shows a
flat `0%` until it finishes (this is exactly the gap that left the Beyblade container leaf
progress-less while the native one worked). It's only a few lines — never drop it for being
"optional polish." The runtime sets the env var for both runtimes (native default
`<work-dir>/progress.txt`, container `/work/output/progress.txt`). Write it from the main
loop (per iteration/step/item; throttle a high-iteration loop to ~every few seconds), make
it **best-effort** (swallow any write error and never fail the unit), and ideally write
**atomically** (temp file in the same dir, then rename) so a reader never sees a partial
value.

If the user's code doesn't do this, **wrap it**: add a thin entrypoint that reads the params
file, calls their existing function, writes the output JSON, and writes progress as it goes —
leaving their actual computation intact. Mirror the patterns in `guides/examples/` — both
[`monte-carlo-pi/main.go`](../../../guides/examples/monte-carlo-pi/main.go) (Go/native) and
[`nbody-gravity/simulate.py`](../../../guides/examples/nbody-gravity/simulate.py)
(Python/container) read the params file, write output JSON, **and** report progress.
**Check:** it runs locally and produces a valid output JSON, and writes a climbing
`progress.txt` (Step D).

### D — Build + test locally
- **NATIVE:** cross-compile for the volunteers' OSes. If Go (or the toolchain) isn't
  installed, build inside a container (e.g. `golang:1.22`). Test with a sample params file
  (first-leaf.md Step 2).
- **CONTAINER:** `podman build`; test with `podman run` + the contract env vars
  (first-leaf.md Path 2, Step 2).
**Check:** the output JSON has the expected fields. Don't proceed until it does.

### E — Host it on the head
- **NATIVE:** `scp` the binaries into the server's `~/lettuce-compute/binaries/`.
  **Check:** `curl -sI https://<domain>/binaries/<file>` returns `200`.

  **Then compute the SHA-256 of each binary.** Native leafs **require** a
  `binary_checksums` entry per platform: the leaf-config validator rejects the
  configure call with a `required_for_native` error if any URL in `binaries`
  lacks a matching checksum, and volunteers refuse to execute a download whose
  hash doesn't match. Compute the digest on the **exact bytes you uploaded** —
  the simplest path is to run it on the server right after the `scp`:

  ```bash
  # On the server (Linux), in ~/lettuce-compute/binaries/
  sha256sum <file>
  ```

  If for any reason you must compute locally first:

  ```bash
  # Linux
  sha256sum <file>
  # macOS
  shasum -a 256 <file>
  # Windows PowerShell — lowercase the hash before pasting
  Get-FileHash -Algorithm SHA256 <file> | ForEach-Object { $_.Hash.ToLower() }
  # Windows cmd
  certutil -hashfile <file> SHA256
  ```

  Keep the **64-char lowercase hex** digests for Step F (uppercase or non-hex is
  rejected at configure time).

- **CONTAINER:** `podman login <domain> -u lettuce` (registry password from head setup),
  then push under an **immutable tag** — a version like `:v1`, or a digest. **Do not use
  `:latest` or a bare image name.** Publishing an artifact version rejects both, because a
  re-pushed floating tag is never re-pulled by volunteers that already cached it, so some
  machines would silently keep computing the old code. Use `podman push <domain>/<image>:v1`
  and bump the tag on every rebuild.

- **WASM:** `scp` the `.wasm` module into the server's `~/lettuce-compute/binaries/`, exactly
  like a native binary, and take its SHA-256 the same way. **Compute the checksum even though
  the head does not demand one for WASM:** the volunteer client refuses to execute a module it
  cannot verify, so a WASM leaf that passes configure with no `binary_checksums["wasm"]` fails
  on every command-line volunteer that fetches it.

### F — Create, configure, activate the leaf
Run these on the server over SSH (key stays put). Use first-leaf.md Steps 4–6 (native) or
Path 2 Steps 4–5 (container). Help the user choose: a **name**, a one-line **description**, a
**research_area** slug (`mathematics`, `physics`, …), a **visibility**, and the validation
mode.

**Visibility** decides who can be handed this leaf's work. `PUBLIC` is listed in the head's
catalog and dispatched to every attached volunteer. `UNLISTED` and `PRIVATE` are handed out
**only** to volunteers who name the leaf explicitly with
`lettuce-volunteer attach --leaf <leaf-id>`. Default to `PUBLIC` unless the user wants a
private test run; if they pick a hidden one, tell them they must give the leaf id to anyone
who is meant to compute it, or nothing will ever be dispatched. `PUBLIC` also requires at
least one `research_area`.

**Validation mode and redundancy go together.** `redundancy_factor` (or the explicit
`target_copies`) is how many volunteers independently compute each unit, and `min_quorum` is
how many must agree before the unit validates.

- **EXACT** — byte-identical outputs. Right for genuinely deterministic code. If the output
  carries anything that varies run to run, such as a wall-clock timing, list those paths in
  `validation_config.ignore_fields` or honest volunteers will be recorded as disagreeing.
- **NUMERIC_TOLERANCE** — selected numeric fields within `numeric_tolerance`. Right for
  floating-point and stochastic work.

**On a redundant NUMERIC_TOLERANCE leaf the head refuses the configure call unless you scope
the comparison.** Set `compare_fields` (the paths that must agree), or `ignore_fields` (the
paths to skip), or assert `compare_all_fields: true` if every field really is deterministic.
This is not optional and there is deliberately no silent default: comparing every field
included nondeterministic runtime metadata, and honest results were being rejected over a
one-millisecond difference in a timing field. Pick the fields that carry the science.

Two more constraints the validator enforces, so get them right the first time:
`agreement_threshold` must be **greater than 0.5** on any leaf dispatching two or more copies,
and `min_quorum` must be less than or equal to `target_copies`.

For NATIVE, put each binary's URL under `execution_config.binaries` **and** its SHA-256 under
`execution_config.binary_checksums` (same platform key, e.g. `linux_amd64`); a missing or
malformed checksum is rejected at configure time and at run time.

`data_config.max_output_size_bytes` must be **> 0** (validation rejects zero/missing) — set
it to the largest reasonable result for this leaf. The server now **enforces** this on every
result submission and rejects oversize payloads, so leave headroom but don't make it huge.
The first-leaf.md examples use `10485760` (10 MiB) which is a sane default for most leafs.

**Check:** GET the leaf → `state` is `ACTIVE`.

### F.5 — Persist the leaf's identity

Once the head has accepted the leaf and returned an `id` and `slug`,
write `lettuce-compute/leafs/<slug>/.lettuce.json` with:

```json
{
  "leaf_id": "<uuid the head assigned>",
  "slug": "<slug the head assigned>",
  "name": "<name as set on the head>",
  "head_url": "<INFRASTRUCTURE_BASE_URL>",
  "state": "ACTIVE",
  "created_at": "<ISO timestamp>"
}
```

This is the **handoff artifact** for any downstream skill. Platform
operators (e.g. SciOS Compute) will run `register-leaf-on-scios` next,
which reads `.lettuce.json` to find the leaf without re-asking. Even
for non-platform deployments, this file is the durable record of
"which leaf this directory represents."

If the head's slug differs from the user's chosen directory name
(slug normalization), write both — `slug` is the head's canonical
slug; the directory name is a local convenience. Don't rename the
directory.

### G — Generate work units
Translate the user's "I'd like to run these values" into a `parameter_space`. **Start small**
(a handful of units) and confirm the pipeline works before scaling. For `PARAMETER_SWEEP`
it's a **Cartesian product** — compute and state the total count, and get the user's OK
before generating anything large.

**Settle `data_config.generation_mode` before you generate anything, because it is immutable
afterwards.** `eager` means you call the generate endpoint and the head produces the whole
space; `lazy` means the head tops the queue up itself as it drains, which suits a long or
open-ended run. Once any work unit exists the head returns `409 GENERATION_MODE_IMMUTABLE` on
a change, and on a `lazy` leaf the manual generate endpoint returns `409
LAZY_GENERATION_MANAGED` because calling it by hand would re-emit trials the head already
generated and burn real volunteer compute on duplicates.

**Check:** work units are `QUEUED`.

### H — Verify and (optionally) compute
Have the user open `/dashboard/leafs` and screenshot the leaf for a shared confirmation.
Offer to attach a volunteer (their own machine) to crunch a few units end to end, and for
aggregating patterns (e.g. Monte Carlo) run the aggregate and show them the result.

## Secret handling
- Run API calls on the server so the **admin key stays in `.env`** there — never paste it
  into chat.
- The **registry password** was saved by the user during head setup; ask them for it when
  pushing, and don't echo it.

## If a step fails
Diagnose from the real output. Common causes: the program doesn't honor the contract (fix the
wrapper), the `aggregation_config.output_field` doesn't match the result JSON, the container
`image` value wrongly includes `https://` (use the bare `domain/image:tag`), or a volunteer
has no container runtime. See `guides/first-leaf.md`.

## Degraded mode (no terminal, or can't SSH)
Switch to guided mode: give the user **one** command at a time, ask them to paste the **full**
output, interpret it, continue with the same steps and checks. Same procedure — only who
types it changes.

## When done
The leaf is live and computing. The directory
`lettuce-compute/leafs/<slug>/` now holds everything that defines it:
the wrapper code, the spec, sample params, and `.lettuce.json` (the
durable record of the head's `leaf_id` and `slug`).

**Hand off cleanly:**

- For **platform operators** (SciOS Compute or similar) — tell the
  user to `cd` into the platform repo and run the platform's
  registration skill (e.g. `register-leaf-on-scios`), passing the
  same slug. That skill reads `.lettuce.json` and inserts the
  platform-side ownership row.
- For **standalone heads with no platform layer** — the leaf is
  already discoverable on the head's own listing. No further step
  needed; the user can manage it via the head's REST API or
  dashboard.

Remind the user how to **add more work units later**, **pause/resume**,
and **collect results** — all in `guides/first-leaf.md` ("Operating
your leaf").
