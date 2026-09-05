use std::sync::Arc;
use std::time::Duration;

use tauri::image::Image;
use tauri::menu::{MenuBuilder, MenuItem, MenuItemBuilder, PredefinedMenuItem};
use tauri::tray::{TrayIcon, TrayIconBuilder};
use tauri::{AppHandle, Emitter, Manager};
use tokio::sync::Mutex;

use crate::api::{ManagementClient, StatusResponse};
use crate::sidecar;

#[derive(Debug, Clone, PartialEq)]
enum TrayState {
    Active,
    Paused,
    Stopped,
}

// Embedded tray icons as raw PNG bytes
const ICON_GREEN: &[u8] = include_bytes!("../icons/tray/icon-green.png");
const ICON_YELLOW: &[u8] = include_bytes!("../icons/tray/icon-yellow.png");
const ICON_GRAY: &[u8] = include_bytes!("../icons/tray/icon-gray.png");

fn load_tray_icon(state: &TrayState) -> Image<'static> {
    let bytes = match state {
        TrayState::Active => ICON_GREEN,
        TrayState::Paused => ICON_YELLOW,
        TrayState::Stopped => ICON_GRAY,
    };
    Image::from_bytes(bytes).expect("embedded tray icon")
}

/// Human wording for the daemon's `paused_reason`. "scheduled" means outside
/// the configured computing hours; other reasons are shown as sent so an
/// unfamiliar value is still visible.
fn paused_text(reason: Option<&str>) -> String {
    match reason {
        None | Some("") => "Paused".into(),
        Some("scheduled") => "Paused — outside your schedule".into(),
        Some(other) => format!("Paused — {other}"),
    }
}

fn status_text(status: &Option<StatusResponse>) -> String {
    match status {
        Some(s) => match s.state.as_str() {
            "active" if !s.active_tasks.is_empty() => {
                let task = &s.active_tasks[0];
                format!(
                    "Computing: {} — {} task{} active",
                    task.leaf_name,
                    s.active_tasks.len(),
                    if s.active_tasks.len() == 1 { "" } else { "s" }
                )
            }
            "active" => "Active — waiting for tasks".into(),
            "paused" => paused_text(s.paused_reason.as_deref()),
            _ => "Stopped".into(),
        },
        None => "Stopped".into(),
    }
}

/// The pause menu item for a status: its text and whether clicking it does
/// anything. The daemon's resume undoes a user pause and nothing else — a
/// schedule or thermal pause answers 409 "not paused" — so "Resume" is offered
/// for that reason alone and any other pause names itself, disabled (TB-72).
fn pause_menu(status: &Option<StatusResponse>) -> (String, bool) {
    match status {
        Some(s) if s.state == "paused" => match s.paused_reason.as_deref() {
            Some("user") => ("Resume".into(), true),
            Some("scheduled") => ("Paused by your schedule".into(), false),
            other => (paused_text(other), false),
        },
        _ => ("Pause".into(), true),
    }
}

fn determine_state(status: &Option<StatusResponse>) -> TrayState {
    match status {
        Some(s) => match s.state.as_str() {
            "active" => TrayState::Active,
            "paused" => TrayState::Paused,
            _ => TrayState::Stopped,
        },
        None => TrayState::Stopped,
    }
}

/// Holds references to menu items we need to update dynamically.
#[derive(Clone)]
pub struct TrayMenuItems {
    pub status_item: MenuItem<tauri::Wry>,
    pub pause_item: MenuItem<tauri::Wry>,
}

/// Tooltip text: the app's own version plus, once known, the bundled CLI's.
fn tooltip_text(app_version: &str, client_version: Option<&str>) -> String {
    match client_version {
        Some(v) => format!("Lettuce Compute {app_version} · client {v}"),
        None => format!("Lettuce Compute {app_version}"),
    }
}

pub fn setup_tray(app: &AppHandle) -> Result<(TrayIcon, TrayMenuItems), String> {
    let status_item = MenuItemBuilder::with_id("status", "Stopped")
        .enabled(false)
        .build(app)
        .map_err(|e| e.to_string())?;

    let pause_item = MenuItemBuilder::with_id("pause", "Pause")
        .build(app)
        .map_err(|e| e.to_string())?;

    let open_item = MenuItemBuilder::with_id("open", "Open Dashboard")
        .build(app)
        .map_err(|e| e.to_string())?;

    let settings_item = MenuItemBuilder::with_id("settings", "Settings")
        .build(app)
        .map_err(|e| e.to_string())?;

    let quit_item = MenuItemBuilder::with_id("quit", "Quit")
        .build(app)
        .map_err(|e| e.to_string())?;

    let sep1 = PredefinedMenuItem::separator(app).map_err(|e| e.to_string())?;
    let sep2 = PredefinedMenuItem::separator(app).map_err(|e| e.to_string())?;

    let menu = MenuBuilder::new(app)
        .item(&status_item)
        .item(&sep1)
        .item(&pause_item)
        .item(&open_item)
        .item(&settings_item)
        .item(&sep2)
        .item(&quit_item)
        .build()
        .map_err(|e| e.to_string())?;

    let icon = load_tray_icon(&TrayState::Stopped);

    let tray = TrayIconBuilder::new()
        .menu(&menu)
        .icon(icon)
        .tooltip(tooltip_text(&app.package_info().version.to_string(), None))
        .on_menu_event(move |app, event| {
            handle_menu_event(app, event.id().as_ref());
        })
        .build(app)
        .map_err(|e| e.to_string())?;

    let items = TrayMenuItems {
        status_item: status_item.clone(),
        pause_item: pause_item.clone(),
    };

    Ok((tray, items))
}

/// Fill in the bundled CLI's version on the tooltip. `lettuce-volunteer
/// --version` runs a process, so this happens on a background thread and the
/// tooltip keeps the app-only text until it answers.
pub fn start_version_tooltip(app: &AppHandle, tray: TrayIcon) {
    let app_version = app.package_info().version.to_string();
    std::thread::spawn(move || match sidecar::client_version() {
        Ok(v) => {
            let _ = tray.set_tooltip(Some(tooltip_text(&app_version, Some(&v))));
        }
        Err(e) => eprintln!("[warn] could not read the bundled CLI version: {e}"),
    });
}

fn handle_menu_event(app: &AppHandle, id: &str) {
    match id {
        "pause" => {
            let app = app.clone();
            tauri::async_runtime::spawn(async move {
                handle_pause_resume(&app).await;
            });
        }
        "open" => {
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.show();
                let _ = window.set_focus();
            }
            // Auto-resume compute when reopening after "close (X)" paused it.
            tauri::async_runtime::spawn(async move {
                if let Ok(info) = sidecar::read_daemon_json() {
                    let client = ManagementClient::from_daemon_info(&info);
                    let _ = client.resume().await;
                }
            });
        }
        "settings" => {
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.show();
                let _ = window.set_focus();
                let _ = app.emit("navigate:settings", ());
            }
        }
        "quit" => {
            let app = app.clone();
            tauri::async_runtime::spawn(async move {
                handle_quit(&app).await;
            });
        }
        _ => {}
    }
}

async fn handle_pause_resume(_app: &AppHandle) {
    let info = match sidecar::read_daemon_json() {
        Ok(info) => info,
        Err(_) => return,
    };

    let client = ManagementClient::from_daemon_info(&info);

    match client.status().await {
        Ok(status) => {
            if status.state == "paused" {
                // Only a user pause is the daemon's to undo; the menu item is
                // disabled for any other reason, so this is belt and braces.
                if status.paused_reason.as_deref() != Some("user") {
                    return;
                }
                if let Err(e) = client.resume().await {
                    eprintln!("[warn] the daemon refused to resume: {e}");
                }
            } else if let Err(e) = client.pause().await {
                eprintln!("[warn] the daemon refused to pause: {e}");
            }
        }
        Err(_) => {}
    }
}

async fn handle_quit(app: &AppHandle) {
    // Suspend all compute, save PIDs, release Job Object, let daemon exit.
    // Frozen processes survive as orphans for next launch.
    // Must use spawn_blocking because suspend_and_quit_sidecar creates its own
    // tokio runtime — you can't call block_on from within an async context.
    let _ = tokio::task::spawn_blocking(|| sidecar::suspend_and_quit_sidecar()).await;
    app.exit(0);
}

pub fn start_status_poll(app: AppHandle, tray: TrayIcon, items: TrayMenuItems) {
    let current_state = Arc::new(Mutex::new(TrayState::Stopped));
    // The pause item as last applied: it depends on the pause reason, not
    // only on the tray state, so it is tracked on its own.
    let mut current_menu: Option<(String, bool)> = None;

    tauri::async_runtime::spawn(async move {
        loop {
            let status = poll_status().await;
            let new_state = determine_state(&status);

            let mut state = current_state.lock().await;
            if *state != new_state {
                let icon = load_tray_icon(&new_state);
                let _ = tray.set_icon(Some(icon));
                *state = new_state;
            }

            // Update the pause/resume menu item: its text and whether it does
            // anything follow the pause reason (TB-72).
            let menu = pause_menu(&status);
            if current_menu.as_ref() != Some(&menu) {
                let _ = items.pause_item.set_text(&menu.0);
                let _ = items.pause_item.set_enabled(menu.1);
                current_menu = Some(menu);
            }

            // Update status text
            let text = status_text(&status);
            let _ = items.status_item.set_text(&text);

            // Emit status to frontend for the status bar
            let _ = app.emit("daemon:status", &status);

            drop(state);
            tokio::time::sleep(Duration::from_secs(2)).await;
        }
    });
}

async fn poll_status() -> Option<StatusResponse> {
    let info = sidecar::read_daemon_json().ok()?;
    let client = ManagementClient::from_daemon_info(&info);
    client.status().await.ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::ActiveTaskInfo;

    fn status(state: &str, reason: Option<&str>) -> Option<StatusResponse> {
        Some(StatusResponse {
            state: state.into(),
            paused_reason: reason.map(String::from),
            ..Default::default()
        })
    }

    #[test]
    fn scheduled_pause_is_worded_for_people() {
        assert_eq!(
            status_text(&status("paused", Some("scheduled"))),
            "Paused — outside your schedule"
        );
        assert_eq!(status_text(&status("paused", Some("thermal"))), "Paused — thermal");
        assert_eq!(status_text(&status("paused", None)), "Paused");
        assert_eq!(status_text(&status("paused", Some(""))), "Paused");
    }

    #[test]
    fn active_and_stopped_text() {
        assert_eq!(status_text(&status("active", None)), "Active — waiting for tasks");
        assert_eq!(status_text(&None), "Stopped");
        let mut s = status("active", None).unwrap();
        s.active_tasks.push(ActiveTaskInfo {
            leaf_name: "Prime Gap".into(),
            ..Default::default()
        });
        assert_eq!(status_text(&Some(s)), "Computing: Prime Gap — 1 task active");
    }

    #[test]
    fn resume_is_offered_for_a_user_pause_only() {
        // TB-72: a schedule pause is not the daemon's resume to undo.
        assert_eq!(pause_menu(&status("paused", Some("user"))), ("Resume".to_string(), true));
        assert_eq!(
            pause_menu(&status("paused", Some("scheduled"))),
            ("Paused by your schedule".to_string(), false)
        );
        assert_eq!(
            pause_menu(&status("paused", Some("thermal"))),
            ("Paused — thermal".to_string(), false)
        );
        assert_eq!(pause_menu(&status("paused", None)), ("Paused".to_string(), false));
        assert_eq!(pause_menu(&status("active", None)), ("Pause".to_string(), true));
        assert_eq!(pause_menu(&None), ("Pause".to_string(), true));
    }

    #[test]
    fn tooltip_includes_client_version_when_known() {
        assert_eq!(tooltip_text("1.0.3", None), "Lettuce Compute 1.0.3");
        assert_eq!(
            tooltip_text("1.0.3", Some("0.9.1")),
            "Lettuce Compute 1.0.3 · client 0.9.1"
        );
    }
}
