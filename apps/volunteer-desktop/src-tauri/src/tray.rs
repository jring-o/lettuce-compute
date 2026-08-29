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
            "paused" => {
                if let Some(reason) = &s.paused_reason {
                    format!("Paused — {}", reason)
                } else {
                    "Paused".into()
                }
            }
            _ => "Stopped".into(),
        },
        None => "Stopped".into(),
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
pub struct TrayMenuItems {
    pub status_item: MenuItem<tauri::Wry>,
    pub pause_item: MenuItem<tauri::Wry>,
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
        .tooltip("Lettuce Compute")
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
                let _ = client.resume().await;
            } else {
                let _ = client.pause().await;
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

    tauri::async_runtime::spawn(async move {
        loop {
            let status = poll_status().await;
            let new_state = determine_state(&status);

            let mut state = current_state.lock().await;
            if *state != new_state {
                let icon = load_tray_icon(&new_state);
                let _ = tray.set_icon(Some(icon));

                // Update pause/resume menu text
                let label = if new_state == TrayState::Paused {
                    "Resume"
                } else {
                    "Pause"
                };
                let _ = items.pause_item.set_text(label);

                *state = new_state;
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
