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
6. **Confirm before anything that costs money or is hard to undo** (creating a paid server,
   deleting data).
7. **Never print secrets into the chat.** Generate them on the server and write them straight
   into `.env`. (See *Secret handling*.)

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
Tell the user what's coming, plainly: "We'll get you a server, point your domain at it, and
I'll install and configure everything. About 20 minutes. I'll handle the technical parts and
only ask you for a few things I can't do for you." Then ask two questions: **Do you already
have (a) a server, and (b) a domain name?** Branch on their answers.

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

### 5 — [you] Secrets + `.env`
Ask the user **only** for the human-meaningful values: their **admin email**, an **admin
password** they'll remember (dashboard login), and a **head name** (what volunteers see).
You already know the domain. Generate everything else (`POSTGRES_PASSWORD`,
`NEXTAUTH_SECRET`, `LETTUCE_ADMIN_API_KEY`, `DASHBOARD_API_KEY`) with `openssl rand -base64 32`
**on the server**, and the registry password + its hash (head-setup.md Step 6). Write `.env`
directly on the server and `chmod 600` it. Show the user the **registry password once** and
tell them to save it (needed later to push container images).

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
  limiting works behind Caddy. Only override it if the user has a non-standard
  proxy network.
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
**Check:** `ls -l keys/signing.key` shows the file exists and is non-empty. Tell the user
to back it up privately (losing it changes the head's signing identity and breaks
verification of every prior attestation).

### 8 — [you] Start the stack
head-setup.md Step 9. **If the server has ≤ 1 GB RAM, build images one at a time** to avoid
the build being killed. **Check:** `docker compose -f compose.production.yaml ps` shows
postgres, infrastructure, dashboard, registry, caddy all up.

### 9 — [you] Verify
head-setup.md Step 10:
- `curl https://your-domain.com/api/v1/health` → `"status":"healthy"`, `"database":"connected"`.
- Bootstrap log shows `admin user created via bootstrap` and `dashboard API key created via bootstrap`.
- Ask the user to open `https://your-domain.com/sign-in`, log in with their email + password,
  and **share a screenshot** of the dashboard so you both confirm it works.

### 10 — Done → offer the leaf
Tell them their head is live and where: `https://your-domain.com` (dashboard), admin console
at `/dashboard/leafs`. Then: "Want me to help you create your first computation now? I'll
walk you through it." → hand off to the **`create-lettuce-leaf`** skill.

## Secret handling

- Generate secrets on the server; write them straight into `.env`. **Never echo a full secret
  value into the chat.** The one exception is the registry push password (the user needs it
  later) — show it once and tell them to store it in a password manager.
- `chmod 600 .env`; never `git add` it. `keys/signing.key` stays on the server — the user
  backs it up privately.
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
