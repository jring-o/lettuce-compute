---
name: deploy-lettuce-head
description: >-
  Guide a non-technical user through deploying a Lettuce head (server) end to end:
  provisioning a server, pointing a domain, generating secrets, the Docker Compose
  deploy, and verification. The agent does everything it can itself over SSH and asks
  the user only for human-only steps (server signup, domain, DNS, screenshots), one
  step at a time, verifying each before moving on. Use when the user wants to deploy,
  set up, stand up, install, or host a Lettuce server or head.
---

# Deploy a Lettuce Head (agent-guided)

You are helping a **non-technical researcher** stand up a Lettuce **head** (a Lettuce
server). They are driving you, a terminal agent. Your job: do as much as possible
yourself, and make the rest effortless for them.

> A **head** is a Lettuce server. A **leaf** is one computation running on it. This skill
> deploys the head. Creating a leaf is the separate `create-lettuce-leaf` skill.

## Source of truth

The verified, exact procedure is in **`guides/head-setup.md`** (follow **Path B —
Production**). Read that file before you start and use its precise commands, ports, env
var names, and checks. This skill tells you *how to run it for the user*; the guide is
*what to run*. If they ever disagree, re-read the guide — its facts win.

## How to behave (re-read this every time before acting)

1. **One step at a time.** Never paste a wall of commands or a long numbered plan. Do — or
   ask for — exactly one thing. Confirm it worked. Then move to the next.
2. **Do everything you can yourself.** You have a terminal and SSH. Generate secrets, write
   files, install software, run the deploy, run the checks. Only involve the user for things
   that genuinely need a human (see *Who does what*).
3. **Plain language.** No jargon dumps. Before each action, say in one human sentence what
   you're about to do and why. Afterward, say what happened in one sentence.
4. **Verify before proceeding.** After each step, run its check. If it fails, diagnose from
   the real output and fix it. Never move forward on a broken step.
5. **When you need the user, make it trivial.** Tell them exactly where to click or what to
   type. Then ask them to paste the output back, or share a screenshot. Wait for it.
6. **Always offer an "I don't know" choice.** Whenever you ask the user to pick between
   options — provider, registrar, password manager, anything — include an explicit
   **"Not sure / help me choose"** option. If they take it, explain the trade-offs in plain
   language and recommend one for their situation, then proceed with that recommendation
   unless they push back.
7. **Confirm before anything that costs money or is hard to undo** (creating a paid server,
   deleting data).
8. **Never print secrets into the chat.** Generate them on the server and write them straight
   into `.env`. (See *Secret handling*.)
9. **When the user must save a secret, make it impossible to lose.** Don't just say "save
   it." Give them explicit storage instructions: a **password manager** (Bitwarden — free;
   1Password; or KeePass — free, offline) and the **exact label** to file it under (e.g.
   *Lettuce admin — https://your-domain.com*). Then call **`AskUserQuestion`** to confirm
   they've actually saved it, with options like *Yes — saved* / *Not yet, wait while I
   save it* / *Help me pick a password manager*. Do not proceed past a save step until
   they confirm. Never drop sensitive files into a user-project-looking folder (e.g.
   `Documents/<name>/`) by default — use a temp directory and have them move it into a
   vault, then delete the temp copy.

## Who does what

| The USER does (you give exact instructions + verify) | YOU do (over SSH) |
|---|---|
| Create a server and give you its public IP | Install Docker; open the firewall (22/80/443) |
| Own a domain; create the two DNS A records | Clone the repo on the server |
| Make sure you can SSH in (add their SSH key) | Generate all machine secrets; write `.env` (chmod 600) |
| Paste output / share screenshots when asked | Set the domain in the Caddyfile; generate the signing key |
| Choose human-meaningful values (admin email, passwords, head name) | Start the stack; verify health, TLS, and bootstrap |

## The flow

Do these in order. For each: who acts, the action, and the **check** that must pass before
you continue. Use the exact commands from `guides/head-setup.md`.

### 0 — Orient

**First, before any chat: sever the local clone's upstream.** This skill is normally
invoked from inside a local clone of `lettuce-compute` (the user opened it in their
editor and ran the prompt from `README.md`). Anything that ends up in this folder during
setup — a scratch note, a temp file, even a misplaced secret — is a leak risk if
`git push` ever runs from here. The fix is to detach this *local* working copy from the
public upstream **immediately**:

```bash
git remote -v                # see what's connected (run in the current working directory)
git remote remove origin     # if origin points to jring-o/lettuce-compute (or a fork of it)
git remote -v                # verify — should print nothing
```

- If there are already no remotes, say so and move on (idempotent — fine to re-run).
- If `origin` points somewhere *other* than `jring-o/lettuce-compute` (e.g. the user's own
  fork they actively work in), **ask before removing it** — offer *Remove it* /
  *Keep it (I know what I'm doing)* / *Help me decide*.
- This only affects the **local** working copy. The clone you'll later make on the
  **server** (Step 4) keeps its `origin` so the user can `git pull` updates in the future.

Then tell the user what's coming, plainly: "We'll get you a server, point your domain at
it, and I'll install and configure everything. About 20 minutes. I'll handle the technical
parts and only ask you for a few things I can't do for you. (I just disconnected this
local folder from the public Lettuce repo as a safety precaution — that way nothing we
do here can accidentally get pushed back.)"

Then give them the escape hatch, in their own words: *"If at any point I say something you
don't understand — a command, an acronym, a button name, anything — copy what I said,
paste it back to me, and tell me 'I don't know what this means'. I'll explain it in plain
language and help you decide. There are no dumb questions, and this offer stands for the
whole session."* Say this once, up front, so they know it's always available.

Then ask two questions: **Do you already have (a) a server, and (b) a domain name?** Branch
on their answers.

### 1 — [user] Get a server
If they don't have one, walk them through it click-by-click (default to **DigitalOcean**
unless they prefer another provider; head-setup.md Step 1 lists alternatives): Ubuntu 22.04,
2 GB RAM, **add their SSH key during creation**. Ask them to paste the server's **public IP**.
**Check:** you can reach it — `ssh root@<IP> 'echo ok; lsb_release -d'` returns `ok` and an
Ubuntu version. If SSH fails, help them add your public key, or drop to *Degraded mode*.

### 2 — [user] Point the domain
Have them create **two** DNS A records at their registrar, both → the server IP:
`your-domain.com` and `viz.your-domain.com` (head-setup.md Step 2). Give registrar-neutral
instructions; offer specifics if they name their registrar.
**Check:** `dig +short your-domain.com` and `dig +short viz.your-domain.com` both return the
IP. DNS can lag — if it hasn't propagated, tell them that's normal and re-check shortly.
**Do not proceed until both resolve** (Caddy needs them for TLS).

### 3 — [you] Install Docker + firewall
Over SSH, run head-setup.md Steps 3–4.
**Check:** `ssh root@<IP> 'docker --version'` succeeds and `ufw status` shows 22, 80, 443.

### 4 — [you] Clone the repo on the server
`ssh root@<IP> 'git clone https://github.com/jring-o/lettuce-compute.git'` (head-setup.md
Step 5). **Check:** `~/lettuce-compute/compose.production.yaml` exists.

Leave the **server** clone's `origin` connected — the user will want it to `git pull`
future updates. The leak risk lived in the *local* clone (already detached in Step 0);
the server clone is a fresh checkout that should never contain anything the user is
authoring or committing in the first place. `.env` and `keys/signing.key` live alongside
the code but are protected by `chmod 600` and the absence of any `git add` step.

### 5 — [you] Secrets + `.env`
Ask the user **only** for the human-meaningful values: their **admin email**, an **admin
password** (dashboard login — they'll use it every sign-in), and a **head name** (what
volunteers see).

**Before they finalize the admin password, give explicit storage instructions:** "Save
this in a password manager — Bitwarden (free), 1Password, or KeePass (free, offline). The
entry should be labeled **Lettuce admin — https://your-domain.com**, with your email as
the username." Then call **`AskUserQuestion`** with options like *Yes — saved* / *Not
yet, wait while I save it* / *Help me pick a password manager*. Do not continue until
they confirm.

You already know the domain. Generate everything else (`POSTGRES_PASSWORD`,
`NEXTAUTH_SECRET`, `LETTUCE_ADMIN_API_KEY`, `DASHBOARD_API_KEY`) with `openssl rand -base64 32`
**on the server**, and the registry password + its hash (head-setup.md Step 6). Write `.env`
directly on the server and `chmod 600` it.

Show the user the **registry password once** with the same kind of storage instructions:
"Save this in your password manager. Label it **Lettuce registry — <domain>**, username
`lettuce`. You'll need it later when you push container images for computations." Then
call **`AskUserQuestion`** again — same three options — to confirm they've saved it
before moving on. (If they ever lose it, it can be rotated; saving it now avoids that
hassle.)

**Domain-substituted values to set in `.env`** (not random — derived from the user's domain):

- `PLATFORM_URL=https://<domain>`
- `LETTUCE_CORS_ORIGINS=https://<domain>` — fail-closed in production; leaving it
  empty disables cross-origin entirely and the dashboard's browser API calls (when
  served from a different origin) will be blocked.
- `VIZ_ORIGIN=https://viz.<domain>` — **required in production**. The viz-bundle
  route only answers requests whose `Host` matches this origin, so author bundle
  code can never execute as script on the main app origin.
- `LETTUCE_TRUSTED_PROXIES` — **leave unset in `.env`**. `compose.production.yaml`
  already defaults it to the Docker/RFC1918 bridge ranges so per-client rate
  limiting works behind Caddy. As of v0.2.0 this governs per-client limiting on
  **both** the HTTP and the gRPC (volunteer) port, so the default matters more —
  but it's still correct out of the box. Only override it if the user has a
  non-standard proxy network.
- **Dispatch tuning (`LETTUCE_HEAD_*`) — leave unset for a fresh head.** v0.2.0
  added server-directed dispatch knobs (work batching, retry delays, the buffer
  lease). `compose.production.yaml` forwards them all from `.env`, but the
  built-in defaults are fine to launch with. The one to revisit **later**, once
  the head carries real volunteer load, is
  `LETTUCE_HEAD_TARGET_REQUEST_RATE_PER_SEC`: measure the single-head dispatch
  ceiling with `swarm-sim` and set it (the `500` default is intentionally high —
  the 2026-06 reference run measured ~240/sec). Full table: `guides/head-setup.md`
  → "Work dispatch tuning".
- `VIZ_BUNDLE_ALLOWED_ORIGINS` — **leave unset** unless the user hosts viz tarballs
  on additional origins. The default (`PLATFORM_URL` origin, where `/binaries/`
  lives) covers the documented setup.

**Check:** `.env` contains no `change-me`/`generate-with`/`replace-with` placeholders, and its
permissions are `600`. `grep -E '^(PLATFORM_URL|LETTUCE_CORS_ORIGINS|VIZ_ORIGIN)=' .env`
shows all three set to the user's real domain.

### 6 — [you] Set the domain in the Caddyfile
Run the `sed` from head-setup.md Step 7 to replace `your-domain.com` with their domain.
**Check:** `grep your-domain.com Caddyfile` returns nothing (all replaced).

### 7 — [you] Generate the signing key (MUST be before compose up)
head-setup.md Step 8. The infrastructure container **fails to start** with a fatal error
if `keys/signing.key` is missing — it no longer silently auto-generates in production.
Run `openssl genpkey -algorithm ed25519 -out keys/signing.key` in the repo root on the
server (so the file ends up at `./keys/signing.key`, which `compose.production.yaml`
mounts read-only at `/keys/signing.key`).
**Check:** `ls -l keys/signing.key` shows the file exists and is non-empty.

Now **back it up off-server, safely**. The user must own a copy: if the server dies and
the key is gone, the head's signing identity is lost and no prior attestation can ever
be verified against this head again.

1. `scp` it down to a **temp directory** on the user's machine — never into their
   Documents tree or anything that could be (or become) a git repo:
   - Windows: `scp root@<IP>:~/lettuce-compute/keys/signing.key "$env:TEMP\<head-name>-signing.key"`
   - macOS/Linux: `scp root@<IP>:~/lettuce-compute/keys/signing.key /tmp/<head-name>-signing.key`
2. Give explicit storage instructions: "This is a tiny (119-byte) private key. Save it as
   a **secure file attachment** on your *Lettuce admin* password-manager entry, **or**
   into an encrypted vault (e.g. a VeraCrypt container, or an encrypted disk image on
   macOS). **Do not** leave it loose in Documents, Desktop, Downloads, or any project
   folder, and **never** commit it to git."
3. Call **`AskUserQuestion`**: *Yes — backed up into my vault* / *Not yet — wait* /
   *Help me back it up (recommend Bitwarden file attachment)*. Do not continue until
   they confirm.
4. **Delete the temp copy** once they've confirmed the backup is in their vault:
   - Windows: `Remove-Item "$env:TEMP\<head-name>-signing.key"`
   - macOS/Linux: `rm /tmp/<head-name>-signing.key`

### 8 — [you] Start the stack
head-setup.md Step 9. **If the server has ≤ 1 GB RAM, build images one at a time** to avoid
the build being killed. **Check:** `docker compose -f compose.production.yaml ps` shows
postgres, infrastructure, dashboard, registry, caddy all up.

### 9 — [you] Verify
head-setup.md Step 10:
- `curl https://your-domain.com/api/v1/health` → `"status":"healthy"`, `"database":"connected"`.
- Startup logs show the schema migrations applied (`migrations applied successfully`,
  through `00002_work_unit_reservations` — the v0.2.0 reservation columns) and a
  `trusted proxies configured` line. No panics or restart loop.
- Bootstrap log shows `admin user created via bootstrap` and `dashboard API key created via bootstrap`.
- Ask the user to open `https://your-domain.com/sign-in`, log in with their email + password,
  and **share a screenshot** of the dashboard so you both confirm it works.

**Note (v0.2.0):** this is a server-directed-dispatch head. Volunteers attaching
to it must be on a **v0.2.0+** build. An older volunteer is rejected at
registration with `volunteer too old for this head: update to a build that signs
per-request nonces` — that's expected, not a deploy fault. Point contributors at
the v0.2.0 release binaries.

### 10 — Done → offer the leaf
Tell them their head is live and where: `https://your-domain.com` (dashboard), admin console
at `/dashboard/leafs`. Then: "Want me to help you create your first computation now? I'll
walk you through it." → hand off to the **`create-lettuce-leaf`** skill.

## Secret handling

- Generate machine secrets on the server; write them straight into `.env`. **Never echo a
  full secret value into the chat.**
- Three things the **user** must save themselves — for each, give explicit storage
  instructions (password manager + exact label) and verify with **`AskUserQuestion`**
  before continuing (see rule #9 in *How to behave*):
  1. The **admin password** they chose (dashboard sign-in).
  2. The **registry push password** (shown once on the server; needed later for image pushes).
  3. The **signing-key backup** (`keys/signing.key`, scp'd to a temp dir, then moved into
     their vault, then the temp copy deleted).
- `chmod 600 .env`; never `git add` it. The **local** clone (the working folder the user
  opened in their editor) has its `origin` remote removed in Step 0 so that an accidental
  `git push` from their editor cannot leak any setup artifact — scratch files, temp
  copies, anything — to the public upstream. The **server** clone keeps its `origin` so
  the user can `git pull` updates later; it should never contain anything they're
  authoring or committing, so the upstream connection is safe there.
- Prefer SSH keys over having the user paste a root password into chat.

## If a step fails

Diagnose from the real error output, then use the troubleshooting table in
`guides/head-setup.md` (TLS/DNS not ready, caddy won't start = bad `REGISTRY_PASS_HASH`,
out-of-memory build, `"database":"disconnected"`, can't sign in). Fix, re-run the step's
check, and tell the user in plain language what went wrong and that it's handled.

## Degraded mode (no terminal, or can't SSH)

If you can't run commands or reach the server yourself, switch to **guided mode**: give the
user **one** command at a time to run (on their machine, or via their provider's web
console), ask them to paste the **full** output, interpret it, and continue with the same
steps and checks. The procedure is identical — only who types it changes. Keep the same
one-step-at-a-time discipline and never ask them to paste secrets you can avoid.
