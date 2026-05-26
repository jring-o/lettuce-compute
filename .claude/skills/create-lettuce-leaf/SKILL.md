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
6. **Confirm before generating a large sweep** (it can create thousands of work units) or
   anything hard to undo.
7. **Never print secrets into the chat** (see *Secret handling*).

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

### A — What are we computing?
Ask, in plain terms, what they want to compute. Three cases:
1. **They have code** → you adapt it to the contract (Step C).
2. **They have a method but no code** → you write it with them, honoring the contract.
3. **They just want to try** → offer an example (`monte-carlo-pi` native, or `nbody-gravity`
   container) and use it.

### B — Native or container? (you decide, tell them why)
- Compiles to a static binary easily (Go, Rust, C/C++) → **NATIVE** (simplest).
- Python / R / Julia, or heavy library/system deps → **CONTAINER** (no cross-compilation).

### C — Make it honor the contract (the important part)
The program must:
- **NATIVE:** read params from `$LETTUCE_PARAMS_FILE` (JSON), write results to
  `$LETTUCE_OUTPUT_FILE` (JSON), exit 0. Optional progress → `$LETTUCE_PROGRESS_FILE`.
- **CONTAINER:** read `$LETTUCE_PARAMETERS_FILE` (`/work/input/parameters.json`), write
  `$LETTUCE_OUTPUT_DIR/output.json`, exit 0.

If the user's code doesn't do this, **wrap it**: add a thin entrypoint that reads the params
file, calls their existing function, and writes the output JSON — leaving their actual
computation intact. Mirror the patterns in `guides/examples/`.
**Check:** it runs locally and produces a valid output JSON (Step D).

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
  then `podman push <domain>/<image>:latest`.

### F — Create, configure, activate the leaf
Run these on the server over SSH (key stays put). Use first-leaf.md Steps 4–6 (native) or
Path 2 Steps 4–5 (container). Help the user choose: a **name**, a one-line **description**, a
**research_area** slug (`mathematics`, `physics`, …), and the validation mode — **EXACT** for
deterministic code, **NUMERIC_TOLERANCE** for stochastic.

For NATIVE, put each binary's URL under `execution_config.binaries` **and** its SHA-256 under
`execution_config.binary_checksums` (same platform key, e.g. `linux_amd64`); a missing or
malformed checksum is rejected at configure time and at run time.

`data_config.max_output_size_bytes` must be **> 0** (validation rejects zero/missing) — set
it to the largest reasonable result for this leaf. The server now **enforces** this on every
result submission and rejects oversize payloads, so leave headroom but don't make it huge.
The first-leaf.md examples use `10485760` (10 MiB) which is a sane default for most leafs.

**Check:** GET the leaf → `state` is `ACTIVE`.

### G — Generate work units
Translate the user's "I'd like to run these values" into a `parameter_space`. **Start small**
(a handful of units) and confirm the pipeline works before scaling. For `PARAMETER_SWEEP`
it's a **Cartesian product** — compute and state the total count, and get the user's OK
before generating anything large.
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
The leaf is live and computing. Remind the user how to **add more work units later**,
**pause/resume**, and **collect results** — all in `guides/first-leaf.md` ("Operating your
leaf").
