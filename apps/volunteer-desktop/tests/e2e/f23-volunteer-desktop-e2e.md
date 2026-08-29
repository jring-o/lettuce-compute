# F23 Volunteer Desktop App — E2E Test Scenarios

> Tauri does not yet have mature E2E test automation tooling. These scenarios
> are structured for manual verification and future automation when tooling
> matures. Each scenario includes preconditions, steps, and expected results.

## Scenario 1: Fresh Install

**Preconditions:** No `~/.lettuce/config.yaml` exists.

| # | Action | Expected Result |
|---|--------|-----------------|
| 1 | Launch app | First-run setup wizard shown |
| 2 | Accept default resources, "Always On" schedule, skip server | Wizard completes |
| 3 | Observe post-wizard state | `~/.lettuce/config.yaml` created, daemon starts, management API reachable at port from `daemon.json` |
| 4 | Check main UI | Tab layout with Overview tab active |
| 5 | Check status bar | Shows "Active -- waiting for tasks" |
| 6 | Check `~/.lettuce/.first_launch_done` | Marker file exists |
| 7 | Check OS autostart | Autostart enabled (registry key on Windows, LaunchAgent on macOS, .desktop on Linux) |

## Scenario 2: System Tray Lifecycle

**Preconditions:** App running with daemon active.

| # | Action | Expected Result |
|---|--------|-----------------|
| 1 | Observe tray icon | Green (active) or gray (no work) |
| 2 | Right-click tray > "Pause" | Daemon pauses, tray icon turns yellow, menu shows "Resume" |
| 3 | Right-click tray > "Resume" | Daemon resumes, tray icon turns green, menu shows "Pause" |
| 4 | Right-click tray > "Open Dashboard" | Main window shown and focused |
| 5 | Right-click tray > "Settings" | Main window shown, Settings tab active |
| 6 | Right-click tray > "Quit" | Daemon stops, daemon.json removed, app exits cleanly |

## Scenario 3: Project Management

**Preconditions:** App running, at least one infrastructure server available.

| # | Action | Expected Result |
|---|--------|-----------------|
| 1 | Go to Projects tab | Empty state or existing servers shown |
| 2 | Click "Add Server", enter address | Server appears in server list with connection status |
| 3 | Browse available projects | At least one project shown if server has projects |
| 4 | Click "Attach" on a project | Project moves to "My Projects" section |
| 5 | Click "Detach" on attached project | Confirmation dialog shown |
| 6 | Confirm detach | Project removed from "My Projects" |
| 7 | Click remove on a server | Server removed from list, config persisted |

## Scenario 4: Resource Configuration

**Preconditions:** App running with config loaded.

| # | Action | Expected Result |
|---|--------|-----------------|
| 1 | Open Settings tab | Resource limits section visible with current values |
| 2 | Adjust CPU cores slider | Value updates, config saved via API, no page reload needed |
| 3 | Close and reopen settings | CPU cores value persisted |
| 4 | Change schedule to "Scheduled" | Weekly grid appears |
| 5 | Drag cells in weekly grid | Schedule ranges saved to config |
| 6 | Toggle "Start on boot" | Autostart state changes (verify via `is_autostart_enabled` command) |
| 7 | Toggle back | Autostart state reverts |

## Scenario 5: Notification Delivery

**Preconditions:** App running, daemon active.

| # | Action | Expected Result |
|---|--------|-----------------|
| 1 | Open Settings > General > Notifications | All notification toggles visible with current values |
| 2 | Enable "Work unit completed" toggle | Toggle on, preference saved via `PUT /api/v1/config` |
| 3 | Complete a work unit (mock or real) | OS notification: "Task Complete" |
| 4 | Set credit milestone threshold to 1 | Threshold saved |
| 5 | Earn 1 credit | OS notification: "Credit Milestone! You've earned 1 total credit!" |
| 6 | Disable "Credit milestones" toggle | Toggle off |
| 7 | Earn another credit | No credit milestone notification fires |
| 8 | Disconnect all servers | OS notification: "Attention Required -- All servers disconnected" |
| 9 | Disable "Errors requiring attention" | Toggle off |
| 10 | Reconnect and disconnect again | No error notification fires |

## Scenario 6: History and Export

**Preconditions:** At least one completed work unit in history.

| # | Action | Expected Result |
|---|--------|-----------------|
| 1 | Open History tab | Timeline shows completed work unit entries |
| 2 | Verify entry details | Project name, completion time, duration, credit, validation status all correct |
| 3 | Use project filter dropdown | Only matching project entries shown |
| 4 | Clear filter | All entries shown again |
| 5 | Click "Export CSV" | File downloads with headers: work_unit_id, leaf_name, completed_at, duration_seconds, credit_earned, validation_status |
| 6 | Open CSV | Data matches displayed entries |

## Update Banner (Cross-cutting)

**Preconditions:** Update endpoint configured and a newer version available.

| # | Action | Expected Result |
|---|--------|-----------------|
| 1 | Launch app | After ~10s delay, OS notification: "Update Available" |
| 2 | Open app window | Blue update banner below tab bar: "Update available: v{version}" |
| 3 | Click "Later" (X) | Banner dismissed for this session |
| 4 | Relaunch app | Banner reappears |
| 5 | Click "Install Now" | Progress bar shows download progress (0-100%) |
| 6 | Download completes | App restarts with new version |
