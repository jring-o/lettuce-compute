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
| `connected but getting no work after repeated polls …` | The head's queue is empty right now, or filters exclude you. | Usually normal — wait. If persistent, check `doctor` and your leaf preferences. |
| `no work for leaf (NotFound)` repeating | You're a native-only box and the leaf is container-only. | Install a container runtime, or this leaf isn't for you. |
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
- In LXC, the most reliable path reported so far is **Docker-in-LXC with your
  user in the `docker` group**; rootless Podman in LXC (userns/nesting/socket
  ownership) is fiddlier.

---

## Updating

```bash
./lettuce-volunteer update     # downloads + verifies the latest release, then restart the daemon
```

If you hit a problem this guide doesn't cover, run `lettuce-volunteer doctor`,
then attach `~/.lettuce/logs/volunteer.log` when you report it.
