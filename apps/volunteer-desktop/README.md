# Lettuce Compute desktop app

A desktop application for volunteers who would rather not use a terminal. It is a shell around the
volunteer command-line client, `lettuce-volunteer`, whose source lives in this repository under
[`services/volunteer-cli`](../../services/volunteer-cli). The app starts that client as a
background daemon and drives it through the daemon's **management API**, a small HTTP API the
daemon serves on `localhost` only. Everything the app shows or changes goes through that API; the
client, not the app, is what talks to heads, fetches work, runs it, and returns results.

Some vocabulary, used throughout this repository: a **head** is a server that hosts computations; a
**leaf** is one such computation; a **work unit** is one piece of a leaf that a volunteer's machine
runs; a **volunteer** is a person who lends their machine.

## How the pieces fit

```
 ┌───────────────────────────┐      management API        ┌─────────────────────────┐   HTTPS   ┌────────┐
 │ Lettuce Compute (desktop) │ ◄───── localhost only ─────►│ lettuce-volunteer start │ ◄───────► │  head  │
 │  window, tray, wizard,    │   port + token read from    │  the daemon: attaches,  │           │ (many) │
 │  notifications, updater   │     ~/.lettuce/daemon.json  │  fetches, runs, returns │           └────────┘
 └───────────────────────────┘                             └─────────────────────────┘
```

- The app launches `lettuce-volunteer start` from the copy of the client bundled next to it (Tauri
  calls this an *external binary* or *sidecar*). When the daemon starts it writes
  `~/.lettuce/daemon.json` with the port it chose and a random token; the app reads that file and
  authenticates every request with the token. Only processes on the same machine can reach the
  API.
- A head sees a desktop volunteer exactly as it sees a command-line volunteer, because it is the
  same client. The command-line client remains the reference client: anything the app can do, the
  client can do, and the [volunteer setup guide](../../guides/volunteer-setup.md) applies to both.
- Beyond the daemon, the app adds what a shell should: a first-run wizard, a tray icon with
  pause/resume, desktop notifications, launch at login, an in-app installer for a container
  runtime on Windows (see [Podman installer](#podman-installer)), and self-updates (see
  [Updater and signing](#updater-and-signing)).

The interface is a React application (`src/`) rendered by [Tauri 2](https://v2.tauri.app) in the
operating system's web view; the Rust side (`src-tauri/src/`) owns process management, the tray,
notifications, and the updater.

## Prerequisites

- **Node.js 24** and npm.
- **Rust** (stable, via [rustup](https://rustup.rs)) and Tauri's per-platform prerequisites, listed
  at <https://v2.tauri.app/start/prerequisites/>. On Windows that is the Visual Studio C++ build
  tools and WebView2 (already present on Windows 10 and 11); on Ubuntu it is
  `libwebkit2gtk-4.1-dev build-essential curl wget file libxdo-dev libssl-dev
  libayatana-appindicator3-dev librsvg2-dev` (plus `patchelf` to build an AppImage).
- **Go**, at the version named in [`services/volunteer-cli/go.mod`](../../services/volunteer-cli/go.mod),
  to compile the bundled client. Not needed if you take the client from a published release
  instead (see below).
- On Windows, PowerShell. Both Windows PowerShell 5.1 and PowerShell 7 run the scripts.

## Development loop

All commands run from `apps/volunteer-desktop`.

```bash
npm ci                                   # web dependencies (once, and after package-lock changes)
./scripts/build-sidecar.sh               # compile the client into src-tauri/binaries/ (Windows: .\scripts\build-sidecar.ps1)
.\scripts\fetch-podman-installer.ps1     # Windows only: download the bundled Podman installer
npm run tauri dev                        # build and open the app with live reload
```

`npm run tauri dev` starts the web dev server, compiles the Rust shell, and opens the window; edits
under `src/` reload in place, edits under `src-tauri/` recompile. The unit tests, the production
web build, and a Rust type-check are what continuous integration runs:

```bash
npm test                                 # vitest, jsdom
npm run build                            # tsc && vite build → dist/
(cd src-tauri && cargo check --locked)
```

`tests/e2e/` holds manual end-to-end scenarios; Tauri has no mature automation for them yet.

### The sidecar script

Tauri expects the bundled client at `src-tauri/binaries/lettuce-volunteer-<rust target triple>`
(with `.exe` on Windows), for example `lettuce-volunteer-x86_64-pc-windows-msvc.exe` or
`lettuce-volunteer-aarch64-apple-darwin`. The directory is git-ignored; `scripts/build-sidecar.ps1`
(Windows) and `scripts/build-sidecar.sh` (macOS, Linux, and Git Bash on Windows) produce the file.

By default the script compiles `services/volunteer-cli` with the same settings as the client's
release workflow (`GOWORK=off`, `CGO_ENABLED=0`, `-trimpath`) and stamps the version that
`git describe --tags` reports for the checkout with the leading `v` removed, which is how the
release workflow turns a tag into a version. Between client releases that stamp looks like
`0.11.1-3-g49d0ac7`; on a commit that carries a client tag it is exactly that release's version.
Only client-style `v*` tags are considered, so desktop tags never leak into the client's version.

| Option | PowerShell | Bash | Purpose |
| --- | --- | --- | --- |
| target | `-Target <triple>` | `--target <triple>` | Cross-compile (defaults to the host triple from `rustc -vV`). |
| published client | `-FromRelease <tag>` | `--from-release <tag>` | Download `lettuce-volunteer-<os>-<arch>` from that GitHub release of this repository and verify its `.sha256` instead of compiling. |
| version | `-Version <string>` | `--version <string>` | Override the stamped version. |

Supported triples: `x86_64-pc-windows-msvc`, `aarch64-pc-windows-msvc`, `x86_64-apple-darwin`,
`aarch64-apple-darwin`, `x86_64-unknown-linux-gnu`, `aarch64-unknown-linux-gnu`. When the target is
the host, the script finishes by running the binary's `--version` so the stamp is visible.

## Building an installer

```bash
npm run tauri build
```

Installers land in `src-tauri/target/release/bundle/` (`msi/` and `nsis/` on Windows, `dmg/` and
`macos/` on macOS, `appimage/` and `deb/` on Linux). From the repository root, `make desktop-build`
runs the sidecar script, `npm ci`, and the build in one go (`make desktop-sidecar` runs the sidecar
script alone).

The configuration turns on `bundle.createUpdaterArtifacts`, so every build also signs the
installers for the updater and needs the private signing key (below) in the environment. The
build reads `TAURI_SIGNING_PRIVATE_KEY` (the key file's contents; the build also accepts the file's
path) and `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`. The key has no password: export the password
variable as an empty string, or the build stops and waits at a `Password:` prompt, where pressing
Enter also works.

```bash
export TAURI_SIGNING_PRIVATE_KEY="$(cat ~/.tauri/lettuce-compute-desktop.key)"
export TAURI_SIGNING_PRIVATE_KEY_PASSWORD=""
npm run tauri build
```

(Windows PowerShell cannot hold an empty environment variable, so there either run the build from
Git Bash as above or press Enter at the prompt.) Without a key the build finishes the installers
and then fails at the signing step. A contributor who does not hold the key can produce an
unsigned local installer by switching the artifacts off for that build:

```bash
npm run tauri build -- --config '{"bundle":{"createUpdaterArtifacts":false}}'
```

Such a build installs and runs normally but cannot serve as an update for existing installs.

On Windows the recommended installer is the **MSI**. An NSIS `.exe` installer is built as well, but
the updater always delivers MSIs (every existing install came from one), and an update delivered to
an NSIS install would install a second copy beside it. Point volunteers at the MSI.

## Podman installer

Leaves that run as containers need a container runtime on the volunteer's machine. On Windows the
app can install one: it ships the official Podman installer inside its own installer and, when the
volunteer opts in during setup, runs it silently and per-user (no administrator prompt) with WSL as
the machine provider, then creates and starts the Podman machine. The code is in
`src-tauri/src/podman_installer.rs`.

The bundled installer is **Podman 5.8.1** (`podman-installer-windows-amd64.msi`, 27,324,416 bytes,
SHA-256 `1ddee378080ce727023d58ff594b692b7174c85fc2e097a0a2250b6347483cbd`, as published in the
`shasums` asset of the [Podman v5.8.1 release](https://github.com/containers/podman/releases/tag/v5.8.1)).
The file is not committed. `scripts/fetch-podman-installer.ps1` downloads it into
`src-tauri/resources/` when it is missing or fails verification, and does nothing otherwise; run it
before building on Windows. `src-tauri/tauri.windows.conf.json` declares the file as a bundle
resource for Windows builds only, so macOS and Linux bundles do not carry it.

To move to a newer Podman, change the version, size, and digest at the top of the script (from the
new release's asset list and `shasums`), run it with `-Force`, re-test the in-app installation on a
machine without Podman, and update the version here. If the Podman installer changes where it puts
`podman.exe`, update the search paths in `podman_installer.rs` as well.

## Updater and signing

The app checks for a new version ten seconds after launch and every six hours, using Tauri's
updater plugin (`src-tauri/src/updater.rs`). A check fetches a small JSON manifest, compares the
version in it with the running version and, if the volunteer accepts, downloads the new installer,
verifies its signature against the public key compiled into the app, and runs it.

### Keys

- The **public key** is in `src-tauri/tauri.conf.json` under `plugins.updater.pubkey`.
- The **private key** lives only on the release operator's machine, outside the repository, at
  `~/.tauri/lettuce-compute-desktop.key` (on Windows, `C:\Users\<you>\.tauri\lettuce-compute-desktop.key`).
  It has no password. It was generated with
  `npx tauri signer generate -w ~/.tauri/lettuce-compute-desktop.key --ci`. `*.key` files are
  git-ignored; never commit it and never paste it into an issue or chat.
- **Back the private key up** somewhere durable (a password manager entry is enough). If it is
  lost, no future build can be signed with it, and every installed copy will refuse every future
  update; the only way forward is a new key pair and a manual reinstall by every volunteer.
- Continuous integration signs with the repository secret **`TAURI_SIGNING_PRIVATE_KEY`**, whose
  value is the full contents of the key file. Create it once:

  ```bash
  gh secret set TAURI_SIGNING_PRIVATE_KEY < ~/.tauri/lettuce-compute-desktop.key
  ```

  No password secret is needed; the workflow passes an empty `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`.

### Where the manifest lives, and why

The updater fetches its manifest from

```
https://github.com/jring-o/lettuce-compute/releases/download/desktop-latest/latest.json
```

Each desktop release is published under its own tag, `desktop-vX.Y.Z`, with its installers, their
`.sig` files, and a generated `latest.json` whose download links point back at that same release.
Installed apps do not read that copy. They read the copy on a release with the fixed tag
**`desktop-latest`**, which holds nothing but the manifest. When a `desktop-vX.Y.Z` release is
published, the workflow re-points the `desktop-latest` tag at the released commit and copies the
new `latest.json` onto the `desktop-latest` release. Rolling back is therefore just re-promoting an
older tag (see below).

The obvious address, `releases/latest/download/latest.json`, cannot be used here. GitHub's "latest
release" is whichever release of the repository is newest, and the volunteer client's releases
(`vX.Y.Z`) share the repository. The client's own `lettuce-volunteer update` command reads that
"latest" release and expects client binaries in it, so desktop releases are always published with
"Set as the latest release" unchecked, and the promotion job enforces that.

## Continuous integration

`.github/workflows/desktop.yml` has three jobs. The existing `ci.yml` and `release.yml` (head
services and the command-line client) are unchanged.

| Job | Runs when | Does |
| --- | --- | --- |
| `desktop-check` | pull requests and pushes to `main` touching `apps/volunteer-desktop/**`, `services/volunteer-cli/**`, or the workflow | On Windows: sidecar script, Podman installer fetch, `npm ci`, `npm test`, `npm run build`, `cargo check --locked`. |
| `desktop-release` | a pushed `desktop-vX.Y.Z` tag, or a manual run | Builds signed installers for Windows (MSI, NSIS), macOS (Apple Silicon and Intel DMGs), and Linux (AppImage, `.deb`) with [tauri-action](https://github.com/tauri-apps/tauri-action), and attaches them, the `.sig` files, and `latest.json` to a **draft** release named `Lettuce Compute desktop vX.Y.Z`. Refuses to build if the tag disagrees with the version in `tauri.conf.json`, `package.json`, and `Cargo.toml`. |
| `desktop-promote` | a `desktop-v*` release being published, or a manual run with the `promote` input set to a published tag | Marks the release "not latest", re-points `desktop-latest`, and uploads the manifest. Refuses drafts. |

## Releases

1. **Bump the version** in `package.json`, `src-tauri/tauri.conf.json`, and `src-tauri/Cargo.toml`
   (`npm version X.Y.Z --no-git-tag-version` updates the first plus `package-lock.json`; after
   editing `Cargo.toml`, `cargo check` in `src-tauri` refreshes `Cargo.lock`). Land it through a
   pull request like any other change.
2. **Cut the client first** if the app should carry an exact client version: tag and release the
   client (`vX.Y.Z`) on the merge commit, then tag the desktop release on the same commit. The
   bundled client is always compiled from the tagged commit; without a client tag there its
   version stamp is a `git describe` string such as `0.11.1-12-gabcdef0`.
3. **Tag and push**: `git tag desktop-vX.Y.Z <commit> && git push origin desktop-vX.Y.Z`. The
   "Desktop app" workflow builds every platform and creates the draft release.
4. **Write the notes and publish.** Open the draft, replace the placeholder body with notes a
   newcomer can follow (what the app does, the behaviour before, what changed and why, the
   behaviour after, and whether volunteers must do anything), and publish it with
   **"Set as the latest release" unchecked**.
5. **Promotion is automatic.** Publishing triggers `desktop-promote`. Confirm with:

   ```bash
   curl -fsSL https://github.com/jring-o/lettuce-compute/releases/download/desktop-latest/latest.json | jq .version
   ```

   An installed copy of the previous version should offer the update within six hours, or
   immediately on its next launch.

To roll back, run the "Desktop app" workflow manually with `promote` set to the previous
`desktop-vX.Y.Z` tag; the manifest returns to that version. To re-run a promotion that failed,
do the same with the current tag.
