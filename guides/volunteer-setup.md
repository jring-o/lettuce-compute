# Volunteering compute to Lettuce

This guide gets you from "downloaded the binary" to "completing work units" — or
helps you self-diagnose why not. You run `lettuce-volunteer`, attach to one or
more **heads** (Lettuce servers), and your machine crunches **work units** for
their **leafs** (computations). Results are validated and credit is tracked
automatically.

> **First tool to reach for:** `lettuce-volunteer doctor`. It checks your
> identity, disk, container runtime (it actually pings the socket), and every
> attached head — and tells you exactly what's wrong and how to fix it. Run it
> whenever something isn't working.

---

## Quick start

```bash
./lettuce-volunteer init                       # generate identity + config
./lettuce-volunteer attach --server head.example.com
./lettuce-volunteer doctor                     # confirm you're ready
./lettuce-volunteer start                      # start computing
```

You can `attach` to multiple heads run by different operators; the CLI spreads
work across all of them.

## Logs — attach this when asking for help

Everything is logged as JSON to **both** stderr and a rotating file at
`~/.lettuce/logs/volunteer.log` (under your `--data-dir`). No shell redirection
needed — if something goes wrong, attach that file. It rotates at 10 MB and
keeps 5 backups, so it's safe to leave running at `--log-level debug`. See the
[README "Logs" section](../README.md#logs) for the config keys.

On startup the daemon prints a readiness line — the runtimes it can run, free
disk vs your allowance, and how many leafs you're eligible for — and raises a
`WARN` for the common silent stalls below.

---

## "Why am I getting no work?"

Map the message in your log (or from `doctor`) to the cause and fix:

| Log message / symptom | Cause | Fix |
|---|---|---|
| `not fetching work: not enough free disk space …` (`volume=data dir`) | Free space on the data-dir volume is below your `max_disk_gb` allowance. | Free space, lower `resource_limits.max_disk_gb`, or `--data-dir` on a roomier volume. |
| `not fetching work: not enough free disk space …` (`volume=image store`) | The container image-store volume (the `path=…` in the log) can't hold a fresh image pull, even if the data dir has room. | Free space there, repoint the engine's store (Docker `data-root` / Podman `graphroot`) to a roomier disk, or enlarge the Podman-machine disk. `doctor` prints the path. |
| `no runnable leafs: every attached leaf needs a container runtime …` | The head's leafs are container leafs and you have no working Docker/Podman. | Set up a container runtime (below), or attach a head with native leafs. |
| `connected but getting no work after repeated polls …` | The head's queue is empty right now, or filters exclude you. | Usually normal — wait. The head tells you when to check back; see "How the volunteer paces its work" below. If persistent, check `doctor` and your leaf preferences. |
| `no work for leaf (empty assignments)` repeating | You're a native-only box and the leaf is container-only. | Install a container runtime, or this leaf isn't for you. |
| `no available runtime for work unit (requires CONTAINER)` then abandon | You advertised CONTAINER but it doesn't actually work. | Fix the container runtime; `doctor` will tell you why it's unusable. |
| `docker is not available … Is the docker daemon running?` | Rootless Podman socket isn't started. | `systemctl --user enable --now podman.socket` (see below). |
| `permission denied … /run/user/1000/podman/podman.sock` | Socket owned by a different user, or you ran under `sudo`. | Run lettuce as your **normal user**, not sudo; the socket owner must match. |

---

## Setting up a container runtime

Many leafs ship as OCI images and need Docker or Podman. **Native** and **WASM**
leafs never need this — they always run.

### Verify first (every OS)

Before touching lettuce, confirm your runtime works **as the same user that runs
lettuce**:

```bash
podman run --rm docker.io/library/hello-world     # or: docker run --rm hello-world
```

If that fails, fix it before going further — lettuce can't do better than your
own `run` command can.

### Linux (rootless Podman — recommended)

```bash
systemctl --user enable --now podman.socket   # start the user socket
loginctl enable-linger "$USER"                # keep it alive when logged out
```

- **Rootless is recommended — run lettuce as your normal user, not `sudo`.**
  Lettuce prefers the rootless socket at `$XDG_RUNTIME_DIR/podman/podman.sock`
  (i.e. `/run/user/<uid>/podman/podman.sock`); its owner must be the user running
  lettuce.
- **Rootful Podman also works, no symlink needed.** When no rootless socket is
  present, lettuce now probes the system socket `/run/podman/podman.sock`
  (root-owned) and uses it automatically, so a host running only rootful Podman is
  auto-detected. The process must be able to read that socket — run lettuce as
  root or as a user granted access to it.
- **Point lettuce at any socket with `CONTAINER_HOST`.** Set
  `CONTAINER_HOST=unix:///path/to/podman.sock` (or `DOCKER_HOST`) to override the
  auto-detected socket — for a non-standard location, with no symlink hack.
- `cgroups v2 not available, falling back to prlimit/affinity` is **benign** —
  resource caps are still applied; it isn't a fault.

### Windows / macOS

Install **Podman Desktop** or **Docker Desktop** and make sure the machine/VM is
started. If you have the Podman CLI, the bundled lettuce binary will create and
start a Podman machine for you on first `start`.

---

## Disk and the data directory

The data dir defaults to `~/.lettuce` (override with `--data-dir <path>`). It
holds your **identity keypair** (`identity.key`/`.pub`), a per-machine
`host.id`, config, logs, and per-unit work files.

- Before fetching any work, the daemon requires `max_disk_gb` free on **both**
  the data-dir volume **and** the container image-store volume (where the
  multi-GB image layers land — see ["Where container images actually
  live"](#where-container-images-actually-live-vmlxc-users-read-this) below).
  Whichever is short, the daemon stays idle and logs a one-time WARN naming that
  volume; `lettuce-volunteer doctor` reports the free space on each. If your home
  volume is small, point `--data-dir` at a roomier disk; if the image store is
  small, see the remedies below.
- **Moving the data dir changes your identity** unless you copy
  `identity.key`/`.pub` across — a new keypair is a new volunteer.

### Running one identity on several machines

Your keypair **is your account** — credit pools to it, so running the **same**
`identity.key`/`.pub` on every machine you own is the intended setup, not a
trick. Each machine separately generates a stable `host.id` (kept in its own data
dir) the first time it runs, so the head tracks your machines independently: a
beefy rig and a laptop on one key each get their **own** work budget and pace
(the rig is never throttled to the laptop's share), and each advertises its own
runtimes — so a native-only box is never handed container work just because
another of your machines runs containers. You don't manage `host.id`; it's
created automatically and you can leave it alone. (Don't copy `host.id` between
machines — let each generate its own; copying just makes two machines look like
one.) For honest verification, your own machines are still treated as one account
for redundancy, so they won't corroborate each other's results — that needs
genuinely different contributors.

### Moving the data dir to another user (keep the same identity)

Your identity is just two files — `identity.key` (the private key) and
`identity.pub` — so it is independent of the username or the path. Copying them to
another account or machine keeps the **same** volunteer identity (and its accrued
credit); the username change itself does **not** break anything. The only things
that break a relocation are a private key the running user can't read, or a
partial copy. Supported steps:

1. Copy `identity.key` and `identity.pub` (and, if you want the same settings,
   `config.yaml`) into the new data dir.
2. Give the **running user** ownership: `chown $(id -un) identity.key identity.pub`.
3. Lock down the private key: `chmod 600 identity.key`.

Do **not** generate fresh files with `lettuce-volunteer init` to "fix" a key that
won't load — `init` creates a **new** identity and abandons the credit on your old
one. If the daemon or `doctor` reports the keypair is present but unreadable, it is
a permission/ownership or partial-copy problem; the error now names the exact
`chown`/`chmod` (or re-copy) fix. Let each machine create its own `host.id` — don't
copy it across (see above).

### Where container images actually live (VM/LXC users, read this)

Image layers — the multi-GB part — do **not** live in the data dir; they live in
the container runtime's store:

- Classic Docker: `/var/lib/docker` (overlay2).
- **Docker 29+ fresh installs use the containerd snapshotter**, so layers live in
  `/var/lib/containerd/io.containerd.content.v1.content/blobs/`, while
  `/var/lib/docker/image` is nearly empty — `docker image ls` still lists the
  image, which misleads people hunting for it. To put images on a big/separate
  volume, mount the **actual** store path (`docker info` → "Docker Root Dir" /
  the containerd root), or switch Docker to the `overlay2` driver.
- Set `max_disk_gb` **below the volume's *reported* free space** — a "64 GB"
  loopback/btrfs volume reports only ~60.6 GB free when empty, so a 60 GB setting
  can fail on an empty disk.
- The daemon checks free space on **this store volume** before fetching a
  container leaf — not just the data dir — so a roomy `~/.lettuce` no longer lets
  a too-small image store sail through the gate and then fail mid-pull with "no
  space left on device". It is **containerd-snapshotter aware**: when `docker
  info` reports `driver-type: io.containerd.snapshotter.v1`, the daemon also
  checks the containerd root (default `/var/lib/containerd`) — where the blobs and
  snapshots actually land — rather than trusting "Docker Root Dir", which on such
  hosts is the wrong filesystem. `lettuce-volunteer doctor` prints the path(s) it
  checks and their free space, and notes when the containerd snapshotter is in
  use. Lettuce can only **detect** these paths — it can't move them: relocate the
  store by repointing the engine (Docker `data-root`, the containerd `root` in
  `/etc/containerd/config.toml`, or rootless Podman `graphroot`) or, on
  Windows/macOS, enlarge the Podman-machine disk (`podman machine init
  --disk-size`).

### Virtualized hosts (Proxmox, LXC, VMs)

- Separate raw volumes for rootfs vs data vs the image store is the normal "data
  separate from OS" pattern — the disk gate checks **both** the data-dir volume
  and the image-store volume, so a small `/var` no longer slips past a roomy
  `$HOME`. Remember Docker 29 puts images under `/var/lib/containerd`; run
  `lettuce-volunteer doctor` to see the exact store path it found and its free
  space.
- **Easiest path in LXC: Docker with your user in the `docker` group.** Rootless
  Podman in LXC works but is fiddlier (user-namespace mapping, nesting, and
  socket ownership all have to line up — see the walkthrough below). Pick Docker
  unless you specifically want rootless. **Rootful Podman** is a third option:
  lettuce auto-detects its system socket (`/run/podman/podman.sock`), which
  sidesteps the rootless user-namespace plumbing entirely — run the volunteer as
  root (or a user with access to that socket).
- A full container runtime needs to be **VM-only** on some setups: in an
  unprivileged LXC guest, rootless engines must write user-namespace id-maps
  through the container → guest → host, which not every host permits. If LXC
  fights you, run the volunteer in a VM instead.

#### Rootless Podman in an unprivileged LXC guest (Proxmox)

Validated on Proxmox VE 9.1 with an Arch Linux guest by a community tester; the
shape is the same for most LXC/distro combinations. Do the basic setup first and
confirm `podman run --rm hello-world` works **before** starting the volunteer —
then add one fix at a time.

On the **Proxmox host**:

```bash
# 1. Give the host enough sub-uids/gids to map into.
#    /etc/subuid and /etc/subgid:   root:100000:200000

# 2. Create the LXC, then enable the features it needs (Options > Features):
#    keyctl=1,nesting=1

# 3. Map the guest's uid/gid range to host ids, and allow the tun device
#    (needed by the rootless network helpers). In /etc/pve/lxc/<VMID>.conf:
lxc.idmap: u 0 100000 165536
lxc.idmap: g 0 100000 165536
lxc.cgroup2.devices.allow: c 10:200 rwm
lxc.mount.entry: /dev/net dev/net none bind,create=dir
```

Inside the **LXC guest**:

```bash
# Confirm the id-map took effect (expect: 0 100000 165536):
cat /proc/self/uid_map

# Install podman, create a dedicated user, and give it sub-uids/gids
# (/etc/subuid and /etc/subgid):   <lettuce-user>:100000:65536

# Reboot, then log in AS the user (su - from root does NOT set up the session
# correctly). As the user, verify the rootless plumbing:
ls -la /run/user/$UID          # the runtime dir must exist
env | grep XDG_RUNTIME_DIR     # must be set (see fix below if empty)
podman run --rm hello-world    # must succeed before going further
```

Common fixes if a check above fails:

- **`/run/user/<uid>` missing after login:** `sudo loginctl enable-linger
  <lettuce-user>`, then reboot.
- **`newuidmap`/`newgidmap` errors mentioning `id_map`:**
  `sudo setcap cap_setuid+ep /usr/bin/newuidmap` and
  `sudo setcap cap_setgid+ep /usr/bin/newgidmap`.
- **`XDG_RUNTIME_DIR` unset / the volunteer can't reach the socket:** the
  volunteer manages Podman over its API socket, so the socket service must be
  running and `XDG_RUNTIME_DIR` must point at it. Add
  `export XDG_RUNTIME_DIR=/run/user/$UID` to your shell profile, `source` it,
  then `systemctl --user enable --now podman.socket`. (`podman run` works
  without the socket, but the volunteer does not — see
  ["Setting up a container runtime"](#setting-up-a-container-runtime).)

---

## How the volunteer paces its work

Your volunteer does **not** poll on a fixed schedule. Instead:

- **The head decides when you check back (server-directed retry delay).** Every
  work request comes back with a delay your volunteer obeys before its next
  request to that head — even when there's no work right now. A quiet head asks
  you back quickly; a busy head stretches the delay out so a large fleet creates
  far less request noise. You don't configure this; the head does, and your
  volunteer follows it.
- **It keeps a client work buffer measured in hours, not units.** Rather than
  fetching one unit at a time, the volunteer requests work in batches and holds
  roughly `work_buffer_hours` of work per concurrent task. While that buffer is
  full it makes **zero** work requests — it just runs what it has. Buffered work
  is reserved for you by the head (not yet started), so it is cheap to hand back
  if you stop, and it is only downloaded/prepared right before it runs.
- **The buffer fills correctly even for fast leafs.** The head tells your
  volunteer roughly how long one unit of each leaf takes, so a leaf with very
  short units is requested in a single large batch (up to a safety ceiling)
  instead of a trickle of tiny requests — your buffer fills to its
  `work_buffer_hours` target and your CPUs stay busy between polls. You don't
  configure this; longer-unit leafs are simply requested fewer at a time.
- **Buffered work is reserved, not heartbeated.** There are no per-task
  keep-alive messages. When you fetch a unit the head reserves it for you for a
  bounded window; while it sits in your buffer the head won't hand that same unit
  to anyone else. If you hold a unit far longer than its reservation window
  (because your buffer is deep and the unit waits a long time for a free slot),
  the head may reclaim and re-offer it — your volunteer notices and quietly drops
  the stale copy rather than running work the head no longer believes is yours,
  so there's no duplicated work.

### Deadlines, not heartbeats

Liveness is now **deadline-based**. Once one of your slots actually *starts* a
unit, a clock begins: if the result isn't submitted before the unit's deadline,
the head assumes the volunteer is gone and reassigns the unit to someone else.
There are no per-task heartbeats to keep a running unit "alive" — just start work
promptly and submit by the deadline.

Two things make this volunteer-friendly:

- **Time spent waiting in your buffer does not count against the deadline.** The
  deadline clock starts when a free slot picks the unit up and begins running it,
  not when you fetched it. A unit can sit in a deep `work_buffer_hours` buffer for
  a while and still get its full run window.
- **If you stop or crash, nothing is lost.** A reserved-but-never-started unit is
  re-offered once its reservation window passes; a started-but-never-finished unit
  is reassigned once its deadline passes. At worst a unit is re-dispatched, never
  permanently stranded.
- **Finishing slightly late still counts.** If a slot paused mid-unit (say on a
  scheduled pause) and you submit just after the deadline, the head still accepts
  the finished result as long as the unit hasn't already been validated by someone
  else — so you keep the credit instead of losing work you already did.

### Tuning the buffer

| Config key | Default | What it does |
|---|---|---|
| `work_buffer_hours` | `2.0` | How many hours of work to keep buffered per concurrent task. Larger = fewer, larger requests and more resilience to a head being briefly unreachable; smaller = leaner. `0` falls back to a small fixed unit count. |
| `max_concurrent_tasks` | `1` | How many work units run at once. The buffer target scales with this. |

```bash
./lettuce-volunteer config set work_buffer_hours 4
```

> **Replaces `work_buffer_size`.** Earlier releases sized the buffer as a unit
> count via `work_buffer_size`. That key is gone; use `work_buffer_hours`.

### Thermal protection

`lettuce-volunteer` watches CPU/GPU temperature and **freezes all work when the
machine gets too hot**, resuming once it cools. The thresholds live under the
`thermal:` block in `~/.lettuce/config.yaml` and are **temperatures in °C — not
workload limits.** When the temperature reaches a pause threshold the daemon
suspends every running unit *and* stops fetching; when it falls back below the
matching resume threshold it resumes everything. The gap between the two is
hysteresis so it doesn't flap on and off — each `*_pause_threshold` must be
greater than its `*_resume_threshold`.

```yaml
thermal:
  enabled: true                # master switch for thermal protection
  cpu_pause_threshold: 85      # °C — freeze ALL work when the CPU reaches this
  cpu_resume_threshold: 75     # °C — resume once the CPU drops below this
  gpu_pause_threshold: 80      # °C — freeze ALL work when the GPU reaches this
  gpu_resume_threshold: 70     # °C — resume once the GPU drops below this
  poll_interval_seconds: 10    # how often temperatures are sampled
```

> **These don't throttle *how much* runs.** Thermal pause is all-or-nothing
> hardware protection, not a per-leaf or concurrency dial. To cap how much work
> runs at once, use `max_concurrent_tasks` (above) and `resource_limits.*` (CPU
> cores, memory, GPU VRAM) — those govern admission; the thermal thresholds only
> decide *whether* work runs at all based on temperature.

> **Hard to observe with very short work units.** Temperatures are sampled every
> `poll_interval_seconds` (default 10s) and only sustained load crosses the pause
> point, so a few-second unit usually finishes before anything triggers. To see it
> on demand, set low thresholds — e.g. `lettuce-volunteer config set
> thermal.cpu_pause_threshold 50` and `… config set thermal.cpu_resume_threshold
> 45` (valid range 30–105, pause > resume) — lower `thermal.poll_interval_seconds`,
> run a longer CPU-heavy leaf, and **restart the daemon** (config is read at
> startup, not hot-reloaded). Watch the log for `thermal throttle activated` /
> `thermal throttle released`.

### Scheduling — run only at certain times

By default the volunteer runs whenever the daemon is running (mode `ALWAYS`). If
you'd rather it compute only at certain times — for example overnight, when the
room is cool and you're not using the machine — use the `schedule` command.

```bash
# Run only overnight, every day ("dusk till dawn"): 20:00 to 06:00.
lettuce-volunteer schedule set --from 20:00 --to 06:00

# Weeknights only.
lettuce-volunteer schedule set --from 19:00 --to 07:00 --days mon-fri

# Layer a SECOND window on top (e.g. weeknights overnight, plus all day on weekends).
lettuce-volunteer schedule add --from 00:00 --to 00:00 --days sat,sun

# See the current schedule, or go back to running always.
lettuce-volunteer schedule show
lettuce-volunteer schedule clear
```

`--days` accepts single days and ranges (`mon-fri`, `sat,sun`, `mon,wed,fri`,
`mon-sun`). Windows are **whole-hour** and **may wrap past midnight**, so
`--from 20:00 --to 06:00` is one continuous overnight window. `schedule set`
**replaces** the schedule with one window; `schedule add` **appends** another, so
you can run different hours on different days (the volunteer runs whenever the
current time falls in *any* window; `--from` equal to `--to` means all 24 hours).
Pairs nicely with thermal protection above: schedule the heavy hours for when
it's coolest.

> **Restart the daemon after changing the schedule.** Like the rest of the
> config, the schedule is read at startup, not hot-reloaded:
> `lettuce-volunteer stop && lettuce-volunteer start`.

> **Fixed clock hours, not true sunset/sunrise.** "Dusk till dawn" here means the
> fixed window you give it; the volunteer does not track your location's actual
> sunset/sunrise (which drift through the year). Pick hours that cover your
> darkest/coolest stretch.

Two other modes exist in `~/.lettuce/config.yaml` under `scheduling:`. Set
`scheduling.mode` to `WHEN_IDLE` to run only after the machine has been idle for
`scheduling.idle_threshold_mins` minutes. For finer control than whole-hour
windows you can instead set a 5-field cron expression
(`lettuce-volunteer config set scheduling.cron_expression "* 20-23,0-5 * * *"` is
the cron equivalent of the overnight window above); when both a window and a cron
expression are present, the window wins.

> **Breaking release — update required.** This release changes the
> volunteer⇄head work protocol. **A volunteer older than this release cannot talk
> to the new head.** Run `lettuce-volunteer update`, then restart the daemon.

## Choosing what you work on

By default your volunteer spreads work across every head you've attached and
every leaf each head offers, in proportion to how far behind each one is. You
can nudge those proportions — or opt out of specific leafs — with two command
groups. Both write to `~/.lettuce/config.yaml` and take effect on the **next
daemon start**.

### Prioritize a head

If you're attached to several heads and want more of your machine's time on one
of them:

```bash
./lettuce-volunteer heads list                  # names, addresses, current weights
./lettuce-volunteer heads weight lbry.science 200
```

Heads are picked by how far each is below its target share, so a head at weight
`200` receives roughly twice the share of one at the default `100`. Weight is a
*ratio*, not a cap — a higher number just means "send more of my work here."

### Prioritize, enable, or disable leafs

Within a head you can do the same per leaf, and opt a leaf in or out entirely:

```bash
./lettuce-volunteer leafs list                  # leafs across your heads + their state
./lettuce-volunteer leafs weight beyblade-arena 200   # more of this leaf
./lettuce-volunteer leafs disable some-leaf     # never run this one
./lettuce-volunteer leafs enable some-leaf      # run it again
./lettuce-volunteer leafs reset                 # back to the head's defaults
```

Add `--server <name>` to any `leafs` command to scope it to one head; omit it to
apply across all of them.

> **Capability still wins.** These preferences only re-rank work you can already
> run — they can't make you eligible for a leaf your machine can't handle (e.g. a
> GPU leaf on a GPU-less box). Use `doctor` to see what you're eligible for.

## Updating

```bash
./lettuce-volunteer update     # downloads + verifies the latest release, then restart the daemon
```

If you hit a problem this guide doesn't cover, run `lettuce-volunteer doctor`,
then attach `~/.lettuce/logs/volunteer.log` when you report it.
