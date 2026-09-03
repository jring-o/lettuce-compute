# Lettuce Compute desktop — release-candidate manual test script

The desktop app wraps the `lettuce-volunteer` command-line client: it starts the
volunteer **daemon** (the background process that fetches and runs **work units** from a
**head**), and talks to it through the daemon's local management API. Tauri has no mature
end-to-end automation, so a release candidate is verified by hand with this script.

Run every section in order on a clean machine (or a clean user account) for each platform
you ship. Record the build number, the OS, and the outcome of every numbered step.

## Before you start

- A build of the release candidate installer for the platform under test.
- Network access to `https://lbry.science` (the head used throughout). It hosts the
  `beyblade-arena` leaf, a container leaf with a visualization bundle whose work units
  take roughly 12 minutes on a typical desktop.
- About 45 minutes; the first work unit dominates.
- For the replay step (6.8), the bundled `lettuce-volunteer` client must be a build that
  keeps an extracted copy of each visualization bundle under `~/.lettuce/results/viz/`
  and points each stored result's `viz_bundle_path` there. Older clients recorded the
  path inside the unit's work directory, which is deleted when the unit completes, so
  replay can only report that the files are gone. Check the bundled client's version
  against the release notes before starting.

Whenever a step fails, the first place to look is the daemon log:

```
~/.lettuce/logs/volunteer.log          (Windows: %USERPROFILE%\.lettuce\logs\volunteer.log)
```

It is the same log the CLI writes; the app adds nothing of its own to it. Copy the tail of
the log into the bug report together with the step number.

Other files that help when diagnosing:

| File | What it tells you |
|---|---|
| `~/.lettuce/config.yaml` | What the wizard and Settings actually wrote. |
| `~/.lettuce/daemon.json` | The running daemon's port, token, and PID. Missing = no daemon. |
| `~/.lettuce/history.jsonl` | One line per completed work unit; what the History page reads. |
| `~/.lettuce/results/index.jsonl` | Results kept for replay, each naming the `viz_bundle_path` replay opens. |
| `~/.lettuce/results/viz/<bundle key>/` | The extracted visualization bundles kept for replay (one copy per bundle). |
| `~/.lettuce/cache/viz/` | Downloaded visualization bundle archives. |

## 1. Clean install

Precondition: no `~/.lettuce` directory and no previous install of the app.

| # | Action | Expected |
|---|---|---|
| 1.1 | Install from the release-candidate package (MSI / DMG / AppImage or deb). | Installs without warnings other than the platform's usual unsigned-app prompts (note any). |
| 1.2 | Launch the app. | The main window opens on the setup wizard's welcome step. No daemon is started yet (`~/.lettuce/daemon.json` does not exist). |
| 1.3 | Check the tray. | The Lettuce tray icon is present; its menu shows a status item, Pause, Open Dashboard, Settings, Quit. |

On failure: the app writes nothing before the wizard finishes, so a failure here is an
installer or platform issue — capture the OS dialog text.

## 2. Setup wizard

| # | Action | Expected |
|---|---|---|
| 2.1 | Welcome → Next. | Identity step. |
| 2.2 | Identity: read the explanation, continue. | A key pair is generated on completion (later visible as the public key under Settings). |
| 2.3 | Resources: set CPU cores below the machine's maximum and memory to about half of it. Note the values. | Sliders accept the values; the CPU slider's maximum is the machine's logical CPU count as the operating system reports it and its proposed default is half of that (on Linux and macOS a machine with more than 8 threads must show its real count, not 8 — the web view's own figure is capped there); the memory slider's maximum matches the machine's physical memory. |
| 2.4 | Schedule: select **Always**. Then switch to **When idle** and set an idle threshold. Then switch to **Scheduled** and paint a window of a few hours on two days. Return to **Always** for the rest of the run. | Each mode shows its own controls (threshold field; weekly grid). Switching modes does not lose the resource values from 2.3. |
| 2.5 | Container runtime: follow the step for your platform (Windows: WSL check then Podman install; macOS: Podman machine setup; Linux: bundled rootless Podman; or an existing Docker is detected). | The step ends in a ready state. Windows/macOS: the Podman machine is created with the CPU/memory/disk values from 2.3. |
| 2.6 | Connect: enter `lbry.science`, test the connection. | The head is reached; its name, description, and active leaves (including **beyblade-arena**) are listed. `https://` is added when omitted. |
| 2.7 | Runtime trust for this head: turn **container** trust **on** and leave **native** trust **off**. | Both controls show their state clearly; native stays off. |
| 2.8 | Finish. | The wizard closes; the main window shows the Overview tab. `~/.lettuce/config.yaml` exists with the resource values from 2.3, `scheduling.mode: ALWAYS`, the `lbry.science` server with `trusted_runtimes: [CONTAINER]`, and `~/.lettuce/daemon.json` appears within about a minute. |
| 2.9 | Tray status. | Shows the daemon as active (no work yet). |

On failure: 2.5 problems are container-runtime problems — run `podman info` (or
`docker info`) in a terminal; 2.8 problems are daemon start-up problems — the log's first
lines after the start show why the daemon refused to run.

## 3. First work unit and live visualization

Precondition: section 2 complete, the daemon active, machine idle.

| # | Action | Expected |
|---|---|---|
| 3.1 | Overview tab, wait up to 5 minutes. | A **beyblade-arena** unit appears in the active tasks with progress climbing, the head name `lbry.science`, runtime **container**. Before it starts, the log shows the container image and the visualization bundle being downloaded. |
| 3.2 | Look at the visualization panel above the task list. | The unit's live visualization renders inside the panel (not a blank dark box) and updates as the unit runs. |
| 3.3 | Click the task row. | The detail panel opens with CPU, memory, and progress figures; the panel keeps showing the same unit's visualization. |
| 3.4 | Click **Pause** at the top of Overview, wait 10 s, click **Resume**. | The task is shown as suspended (with the reason "user") then running again; progress does not reset; the visualization stops updating while paused. |
| 3.5 | Wait for the unit to finish (about 12 minutes). | The task leaves the active list; the tray and Overview return to "waiting for work" or start the next unit; the log shows the result submitted and `accepted=true`. If notifications are on, a completion notification appears. |
| 3.6 | Credit on Overview. | Credit figures refresh within a minute of completion; they may be decimals (for example `0.5`). |

On failure: a unit that never arrives is almost always a trust or resource-fit problem —
search the log for `trusted` and for `disk`; a visualization panel that stays dark is a
bundle problem — search the log for `PrepareVizBundle`.

## 4. Projects page

| # | Action | Expected |
|---|---|---|
| 4.1 | Open Projects. | `lbry.science` is listed as connected with its leaves; **beyblade-arena** shows its runtime, resource requirements, and the credit earned so far. |
| 4.2 | Change the head's runtime trust: turn **native** on. | The page explains that the change needs a daemon restart to take effect and offers a **Restart** action. |
| 4.3 | Restart from that prompt. | The daemon restarts within about a minute (`daemon.json` is rewritten with a new PID); the page reconnects without a relaunch; the trust change is shown as active. Turn native back **off** and restart again. |
| 4.4 | Disk gate: in Settings, lower the disk limit below what beyblade-arena requires (its `min_disk_mb`), save, and return to Projects. | The leaf shows a clear "blocked for disk" message naming the limit to raise it to; no new unit of that leaf is fetched. Restore the limit; the message clears after the next refresh. |
| 4.5 | Failing leaf: if the head exposes a leaf that fails on this machine (ask the head operator; `lbry.science` keeps one for this purpose when available), enable it. | After its units fail, the leaf is marked as failing with the failure count, the last reason, and — after repeated failures — that it is paused until a stated time. Work from other leaves continues. |
| 4.6 | Detach and re-attach the head. | Detaching asks for confirmation and removes the head; re-attaching (with container trust on) restores it and work resumes. |

On failure: 4.3 — the log shows the old daemon's shutdown and the new one's start; if the
old PID lingers, note the platform. 4.4 — search the log for `disk gate`.

## 5. Settings

| # | Action | Expected |
|---|---|---|
| 5.1 | Change a resource value (for example CPU cores) and save. | The value persists after closing and reopening the app; `config.yaml` matches. |
| 5.2 | Use the **Restart daemon** button. | The daemon restarts (new PID in `daemon.json`); the app reconnects on its own; a running unit resumes from its checkpoint rather than starting over. |
| 5.3 | Theme: choose **Dark**, quit the app from the tray, relaunch. | The app opens in dark theme. Repeat with **Light**, then set **System**. |
| 5.4 | **Launch minimized** (start on boot): turn it on, log out and back in (or reboot). | The app starts with the daemon running and only the tray icon visible; no window opens until you choose Open Dashboard. Turn it off afterwards. |
| 5.5 | Identity section. | The public key is shown; the regenerate option asks for confirmation and warns that credit is tied to the key. Do not confirm. |
| 5.6 | Notifications: turn **Work unit completed** on. | A completion notification appears when the next unit finishes (revisit after section 3 if needed). |

## 6. History

Precondition: at least one unit completed (section 3).

| # | Action | Expected |
|---|---|---|
| 6.1 | Open History. | The completed unit is listed under today's date with its leaf name, the first characters of its work-unit ID, completion time, head, and CPU time. Its badge reads **Head accepted** (hover: it explains that acceptance is recorded on submission and that validation and credit are decided later on the head). A count of loaded entries and an **End of history** line are shown. |
| 6.2 | Expand the row. | CPU time, wall clock, time paused, head, "Head accepted: Yes", and the full work-unit ID with a working **Copy** button. |
| 6.3 | Leaf filter: choose **beyblade-arena**, then **All Leafs**. | Only that leaf's entries are shown, with the count reading "N matching of M loaded"; clearing restores the list. The dropdown keeps listing every leaf seen. |
| 6.4 | Head accepted filter: choose **Head rejected**. | The list shows "No entries match the current filters" (assuming nothing was rejected); the count still shows how many were loaded. Return to **All submissions**. |
| 6.5 | Date range: **Last 7 days**, **All time**, **Custom** with a range that excludes today. | Entries appear or disappear accordingly. |
| 6.6 | Export **CSV**. | A file `lettuce-history.csv` is saved with the header `work_unit_id,leaf_name,head_name,completed_at,duration_seconds,cpu_seconds,credit_earned,head_accepted` and one row per entry (`head_accepted` is `true`/`false`). |
| 6.7 | Export **JSON**. | `lettuce-history.json` holds an array with the same entries as the daemon reports them. |
| 6.8 | Replay (requires the client build described under "Before you start"): expand the beyblade-arena row and click **View Visualization**. | A dialog opens and replays the unit's result in the visualization. Escape or the close button closes it. The result's `viz_bundle_path` in `~/.lettuce/results/index.jsonl` points under `~/.lettuce/results/viz/`. If the dialog instead reports that the visualization files are no longer on this machine, the step fails: either the bundled client is too old (its recorded path was inside the deleted work directory) or the kept copy is missing — note which. |

On failure: History reads `~/.lettuce/history.jsonl`; compare the file with the page.
Replay reads `~/.lettuce/results/index.jsonl` and opens the `viz_bundle_path` it names;
that directory must exist and hold an `index.html`.

## 7. Tray

| # | Action | Expected |
|---|---|---|
| 7.1 | Tray → **Pause**. | The tray status changes to paused, the menu item becomes **Resume**, Overview shows the daemon paused (reason: user), and an active unit is suspended. |
| 7.2 | Tray → **Resume**. | Everything returns to active; the unit continues. |
| 7.3 | Close the main window with the window's close button. | The window hides to the tray and computing pauses; **Open Dashboard** brings the window back and resumes computing. |
| 7.4 | With a unit running, tray → **Quit**. | The app exits within about 30 s. The daemon has exited (`daemon.json` removed); the unit's process is frozen, not killed, and its work directory is preserved (`~/.lettuce/container-work/<unit id>` still exists). |
| 7.5 | Relaunch the app. | The daemon starts, adopts the frozen unit, and the unit resumes from where it was (progress continues from the previous figure; the log reports the unit resumed). |

On failure: 7.4/7.5 — search the log for `preserving work directory` and `resum`.

## 8. Update banner

Precondition: an update feed with a version newer than the candidate (the release team
provides a staging feed; point the app at it as documented for the build).

| # | Action | Expected |
|---|---|---|
| 8.1 | Launch the app with the staging feed configured. | About 10 s after start, an update notification appears and a banner shows the new version at the top of the window. |
| 8.2 | Dismiss the banner. | It stays hidden for this session and returns on the next launch. |
| 8.3 | Click **Install**. | A progress bar advances to 100 %, the app restarts on the new version (check the version shown in Settings). |
| 8.4 | Confirm the daemon after the update. | `daemon.json` shows the new client version and work continues. |

## Sign-off

A release candidate passes when every step above has the expected result on each shipped
platform, or when each deviation is filed with the step number, the platform, and the log
excerpt, and the release owner has accepted it.
