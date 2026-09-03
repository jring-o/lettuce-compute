# Volunteering compute to Lettuce

This guide gets you from "downloaded the binary" to "completing work units" — or
helps you self-diagnose why not. You run `lettuce-volunteer`, attach to one or
more **heads** (Lettuce servers), and your machine crunches **work units** for
their **leafs** (computations). Results are validated and credit is tracked
automatically.

> **Prefer an app?** **Lettuce Compute** is the desktop version of this same program —
> a setup wizard, task and history views, replayable visualizations, and a tray icon, for
> Windows, macOS and Linux. Get it from this repository's GitHub Releases (the release
> tagged `desktop-v…`). Everything in this guide still applies underneath: the app bundles
> `lettuce-volunteer` and shares the same account and data directory, so the app and the
> terminal client are interchangeable.

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
work across all of them. `--server` (on `attach` and `init`, and the address
field in the desktop app) takes the head's host name; a pasted URL such as
`https://head.example.com/` is accepted too — the scheme and path are dropped,
and a port in the address (`head.example.com:8443`) is used for both gRPC and
HTTP. An `http://` address means the head is reached without TLS.

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
| `not fetching work: disk-gated …` (`reason=…data dir…`) | Free space on the data-dir volume doesn't cover what an enabled leaf declares it needs (its disk requirement + a 2 GB floor), or is below the floor entirely. The reason names the numbers. | Free space, or `--data-dir` on a roomier volume. With several leafs enabled, only the ones that fit are fetched; this WARN fires when none fit. |
| `not fetching work: disk-gated …` (`reason=…image store…`) | The container image-store volume (named in the reason) can't hold the fresh image pull an enabled leaf needs, even if the data dir has room. | Free space there, repoint the engine's store (Docker `data-root` / Podman `graphroot`) to a roomier disk, or enlarge the Podman-machine disk. `doctor` prints the path. |
| `not fetching work: disk-gated …` (`reason=disk budget…`) | Lettuce's own footprint (work folders + downloaded images) plus the leaf's need would exceed your `max_disk_gb` allowance. | Free space (superseded images are reclaimed automatically), disable an unused leaf, or raise `resource_limits.max_disk_gb` — the message names the value that clears the gate. |
| `no runnable leafs: every attached leaf needs a container runtime …` | The head's leafs are container leafs and you have no working Docker/Podman. | Set up a container runtime (below), or attach a head with native leafs. |
| `connected but getting no work after repeated polls …` | The head's queue is empty right now, or filters exclude you. | Usually normal — wait. The head tells you when to check back; see "How the volunteer paces its work" below. If persistent, check `doctor` and your leaf preferences. |
| `connected but getting no work: every attached leaf needs a runtime this volunteer has not trusted its head to run …` | Every enabled leaf needs a runtime you declined for this head at attach time (or that this machine lacks). The volunteer does not even ask for those leafs — the head would refuse. | If you accept running that head's code: `lettuce-volunteer heads trust <head> <runtime>` and restart. Otherwise enable a leaf you can run, or attach another head. |
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
holds your **identity keypair** (`identity.key`/`.pub`), the per-machine **host
ids** each head issues you (see [Running one identity on several
machines](#running-one-identity-on-several-machines)), config, logs, and per-unit
work files.

- **What a fetch requires free is the leaf's own declared disk need, not your
  whole allowance.** Before fetching work for a leaf whose container image is
  not yet downloaded, the daemon checks that the leaf's declared disk
  requirement (plus a 2 GB safety floor) is free on **both** the data-dir
  volume **and** the container image-store volume (where the multi-GB image
  layers land — see ["Where container images actually
  live"](#where-container-images-actually-live-vmlxc-users-read-this) below).
  A leaf whose image is already downloaded needs only workspace headroom. When
  every enabled leaf is blocked this way the daemon stays idle and logs a
  one-time WARN naming the numbers and an example leaf; with a mix, the
  affordable leafs keep working while the too-big one is skipped.
  `lettuce-volunteer doctor` reports free space, Lettuce's own measured usage,
  and — per leaf — whether fetching is currently gated, including the
  usage-plus-need arithmetic against your allowance. While the daemon runs,
  those per-leaf verdicts (and the usage figure feeding them) are quoted from
  the **running daemon itself** — the exact gate the fetcher enforces,
  including whether a leaf's container image is already downloaded, which
  lowers what the fetch still needs. With the daemon stopped, doctor computes
  a conservative reading instead and says so: on a container host the usage
  figure is reported as partial rather than the budget called healthy, and
  each per-leaf disk verdict is labelled as assuming a fresh image download.
  If your home volume is small, point `--data-dir` at a roomier disk; if the
  image store is small, see the remedies below.
- **`max_disk_gb` is the capacity you offer, in both directions.** It is the
  disk budget your client advertises to each head — a head only sends you a
  leaf whose declared disk requirement fits inside it, so a leaf needing 15 GB
  is never dispatched to a volunteer allowing 10 GB, no matter how much space
  is actually free. And the daemon keeps Lettuce's own footprint (work folders
  plus downloaded container images) within the same number. Raising it to
  qualify for a bigger leaf is safe: it does **not** raise what must be free
  (that is always the leaf's own requirement, above). `lettuce-volunteer
  doctor` prints the allowance next to your memory and CPU limits and names any
  leaf blocked by it, and `leafs list` marks it `WILL FETCH: no` with the
  remedy underneath — including when the leaf's requirement fits your allowance
  but the daemon's own fetch gate is skipping it right now because current
  usage plus the leaf's need would exceed it. The suggested `max_disk_gb`
  value is computed from **today's usage as well as the leaf's requirement**
  (when the daemon is running), so one paste clears the gate — no
  raise-and-check-again loop. Raise it with
  `lettuce-volunteer config set resource_limits.max_disk_gb <n>` and restart the
  daemon, which is when the new figure is re-advertised.
- **`max_gpu_vram_pct` is the same kind of trap, and sharper, because it is a
  percentage.** A head does not compare a GPU leaf's memory requirement against
  your card's size — it compares it against the *share of the card you allow*,
  which defaults to **50%**. So an 8 GB card offers 4096 MB and clears a leaf
  needing 4 GB; a 6 GB card offers 3072 MB and is never sent that leaf, even
  though the card is bigger than the requirement. `lettuce-volunteer doctor` and
  `leafs list` name any leaf blocked this way, print the requirement next to what
  your machine offers and the card it came from, and suggest the percentage that
  would clear it; where no percentage can (the card really is too small) they say
  that instead. Raise it with
  `lettuce-volunteer config set resource_limits.max_gpu_vram_pct <n>` and restart.
  A leaf may also require a particular make of card, which the same commands
  report.
- **Moving the data dir changes your identity** unless you copy
  `identity.key`/`.pub` across — a new keypair is a new volunteer.

### Running one identity on several machines

Your keypair **is your account** — credit pools to it, so running the **same**
`identity.key`/`.pub` on every machine you own is the intended setup, not a
trick. To track your machines independently, **each head issues every machine its
own host id** when it registers, and your volunteer stores that id (one per
attached head) in its data dir. So a beefy rig and a laptop on one key each get
their **own** work budget and pace (the rig is never throttled to the laptop's
share), and each advertises its own runtimes — so a native-only box is never
handed container work just because another of your machines runs containers. You
don't manage these host ids: the head hands them out automatically, and if one is
deleted the head simply issues a fresh one the next time that machine registers.
(There's nothing to copy between machines — each registers and gets its own id;
two machines sharing one id would just look like a single machine to the head.) A
head may cap how many machines it meters separately per account (10 by default);
past that, additional machines still run and earn credit — they just share one
work budget instead of getting their own. For honest verification, your own
machines are still treated as one account for redundancy, so they won't
corroborate each other's results — that needs genuinely different contributors.

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
`chown`/`chmod` (or re-copy) fix. Copy only the identity keypair — leave each
machine to get its own head-issued host ids (see above).

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
- The daemon checks free space on **this store volume** before fetching a
  container leaf whose image needs downloading — not just the data dir — sized
  by that **leaf's declared disk requirement** (never your whole `max_disk_gb`),
  so a roomy `~/.lettuce` no longer lets a too-small image store sail through
  the gate and then fail mid-pull with "no space left on device". It is **containerd-snapshotter aware**: when `docker
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
  roughly `work_buffer_hours` of work per concurrent task. Once that buffer has
  filled it makes **zero** work requests until the remaining work has drained
  below about half the target, then refills back to the target in one round — so
  fetches come in well-spaced batches instead of a constant trickle of one-unit
  top-ups at the target line. Buffered work is reserved for you by the head (not
  yet started), so it is cheap to hand back if you stop, and it is only
  downloaded/prepared right before it runs.
- **GPU work is buffered per GPU, not per task slot.** A unit that needs a GPU
  runs one at a time per physical GPU, however many concurrent tasks you allow.
  So on a machine with one GPU and eight task slots the buffer holds at most
  `work_buffer_hours` of GPU units (2 h by default), not eight times that, and
  requests for a GPU leaf are sized to that smaller target; CPU leafs still fill
  the full per-slot buffer beside it. Without this bound such a machine hoarded
  GPU units only one slot could ever run and handed most of them back unrun at
  the deadline.
- **It only asks for work it could be handed.** A leaf whose runtime this
  machine does not have, or that you have not trusted its head to run
  (`lettuce-volunteer heads trust <head> <runtime>` opts in), is never requested
  — the head would refuse it anyway. If every attached leaf is excluded this
  way, the "getting no work" warning says which runtime is missing or untrusted
  instead of blaming an empty queue (see the table near the top of this guide).
- **"Full" also means usable.** If a slot is idle but nothing in the buffer can
  start beside what's already running (say every buffered unit needs more memory
  than you have left), the buffer does not count as full: the volunteer keeps
  requesting — restricted to leafs whose units could actually fill that idle
  slot — and any arriving work it can't use is handed straight back to the head
  for immediate re-dispatch instead of sitting in the buffer until its deadline.
  If a slot stays idle this way for more than about ten minutes, the volunteer
  logs a warning naming what is blocking the buffered work.
- **The buffer fills correctly even for fast leafs.** The head tells your
  volunteer roughly how long one unit of each leaf takes, so a leaf with very
  short units is requested in a single large batch (up to a safety ceiling)
  instead of a trickle of tiny requests — your buffer fills to its
  `work_buffer_hours` target and your CPUs stay busy between polls. You don't
  configure this; longer-unit leafs are simply requested fewer at a time. If the
  head's per-unit figure turns out wrong, the volunteer corrects it from the
  units that actually arrive and caps the next request at what the last round
  could really hold, so one mis-sized batch never turns into an endless
  request-and-return loop. Returned units cost the work unit nothing: the head
  records them as unused give-backs (not failures) and simply avoids re-offering
  the same unit to the same machine for a few minutes.
- **Buffered units start first-fit, not strictly first-fetched.** A free slot
  takes the oldest buffered unit that fits in the memory you've allowed
  (`resource_limits.max_memory_mb`). If the oldest unit needs more memory than
  is currently free, smaller units behind it start instead of the slot sitting
  idle — the waiting unit logs a single `waiting for capacity` line, keeps its
  place in line, and only a bounded number of units may jump it before the
  volunteer holds the queue so it can run.
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
| `work_buffer_hours` | `2.0` | How many hours of work to keep buffered per concurrent task (per GPU for GPU-required units, which run one per GPU). Larger = fewer, larger requests and more resilience to a head being briefly unreachable; smaller = leaner. `0` falls back to a small fixed unit count. |
| `max_concurrent_tasks` | `1` | How many work units run at once. The buffer target scales with this, except that GPU units are bounded by the number of GPUs when that is smaller. |

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
  max_throttle_minutes: 30     # resume and re-check after this long frozen (negative = wait indefinitely)
```

> **Which sensors these apply to.** The CPU thresholds are compared against CPU
> sensors only, and the GPU thresholds against GPU sensors only. Machines expose
> plenty of other temperatures — the SSD, the WiFi chip, the chipset — and those
> run hot by design, so judging them against a CPU's danger point would freeze
> your work while your CPU was perfectly cool. They are still honoured, but only
> against the danger point the component itself declares to the kernel. A sensor
> that declares none can never pause your work.

> **`max_throttle_minutes` stops a freeze lasting forever — but never on your
> CPU or GPU.** If your processor is genuinely hot, work stays suspended for as
> long as it stays hot, however long that is; that is the whole point of the
> feature and this setting does not shorten it. The ceiling applies only when
> some *other* part is holding the pause — a warm drive, a chipset — because
> suspending Lettuce cannot cool a component its own work isn't heating, so
> without a ceiling the daemon would wait for a change that never comes. After
> this long it resumes, logs loudly why, and ignores that particular sensor for a
> couple of hours before trusting it again. Set it negative to wait indefinitely
> instead.

> **These don't throttle *how much* runs.** Thermal pause is all-or-nothing
> hardware protection, not a per-leaf or concurrency dial. To cap how much work
> runs at once, use `max_concurrent_tasks` (above) and `resource_limits.*` (CPU
> cores, memory, GPU VRAM) — those govern admission; the thermal thresholds only
> decide *whether* work runs at all based on temperature.

> **Hard to observe with very short work units.** Temperatures are sampled every
> `poll_interval_seconds` (default 10s) and only sustained load crosses the pause
> point, so a few-second unit usually finishes before anything triggers. To see it
> on demand, set low thresholds — e.g. `lettuce-volunteer config set
> thermal.cpu_resume_threshold 45` and then `… config set thermal.cpu_pause_threshold
> 50` (valid range 30–105; set the **resume** threshold first — each `config set` is
> validated on its own, so lowering the pause threshold below the current resume
> threshold is rejected) — lower `thermal.poll_interval_seconds`,
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

> **Breaking release — update required.** This release moves machine **host ids**
> from client-generated to **head-issued** (see [Running one identity on several
> machines](#running-one-identity-on-several-machines)), a deliberate hard cutover
> with no backward path. **A volunteer older than this release keeps presenting a
> host id the head never issued, so an upgraded head stops handing it work** — it
> logs that its build is outdated at each retry. Run `lettuce-volunteer update`,
> then restart the daemon; the updated client re-registers and receives a fresh id
> automatically.

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
