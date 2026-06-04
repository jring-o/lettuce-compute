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
| `not fetching work: not enough free disk space …` | Free space is below your `max_disk_gb` allowance, so the daemon won't fetch. | Free space, lower `resource_limits.max_disk_gb`, or `--data-dir` on a roomier volume. |
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

- **Run lettuce as your normal user — never `sudo`.** Lettuce uses the rootless
  socket at `$XDG_RUNTIME_DIR/podman/podman.sock` (i.e.
  `/run/user/<uid>/podman/podman.sock`), **not** the root socket at
  `/run/podman/podman.sock`. The socket's owner must be the user running lettuce.
- `cgroups v2 not available, falling back to prlimit/affinity` is **benign** —
  resource caps are still applied; it isn't a fault.

### Windows / macOS

Install **Podman Desktop** or **Docker Desktop** and make sure the machine/VM is
started. If you have the Podman CLI, the bundled lettuce binary will create and
start a Podman machine for you on first `start`.

---

## Disk and the data directory

The data dir defaults to `~/.lettuce` (override with `--data-dir <path>`). It
holds your **identity keypair** (`identity.key`/`.pub`), config, logs, and
per-unit work files.

- Before fetching any work, the daemon requires `max_disk_gb` free **on the
  data-dir volume**. If your home volume is small, point `--data-dir` at a
  roomier disk.
- **Moving the data dir changes your identity** unless you copy
  `identity.key`/`.pub` across — a new keypair is a new volunteer.

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

### Virtualized hosts (Proxmox, LXC, VMs)

- Separate raw volumes for rootfs vs data vs the image store is the normal "data
  separate from OS" pattern — just remember the disk gate checks the data-dir
  volume, and Docker 29 puts images under `/var/lib/containerd`.
- **Easiest path in LXC: Docker with your user in the `docker` group.** Rootless
  Podman in LXC works but is fiddlier (user-namespace mapping, nesting, and
  socket ownership all have to line up). Pick Docker unless you specifically want
  rootless.
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

> **Breaking release — update required.** This release changes the
> volunteer⇄head work protocol. **A volunteer older than this release cannot talk
> to the new head.** Run `lettuce-volunteer update`, then restart the daemon.

## Updating

```bash
./lettuce-volunteer update     # downloads + verifies the latest release, then restart the daemon
```

If you hit a problem this guide doesn't cover, run `lettuce-volunteer doctor`,
then attach `~/.lettuce/logs/volunteer.log` when you report it.
