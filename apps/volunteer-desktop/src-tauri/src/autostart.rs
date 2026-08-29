use std::path::PathBuf;

use tauri::AppHandle;
use tauri_plugin_autostart::ManagerExt;

use crate::sidecar;

fn first_launch_marker() -> PathBuf {
    sidecar::lettuce_dir().join(".first_launch_done")
}

/// Set up auto-start on first launch. After the first explicit app launch,
/// auto-start is enabled by default so the app starts minimized to tray on boot.
pub fn setup_autostart(app: &AppHandle) {
    let marker = first_launch_marker();
    if !marker.exists() {
        // First launch: create marker and enable autostart
        let _ = std::fs::write(&marker, "");
        let _ = app.autolaunch().enable();
    }
}

/// Check if autostart is currently enabled.
pub fn is_autostart_enabled(app: &AppHandle) -> Result<bool, String> {
    app.autolaunch()
        .is_enabled()
        .map_err(|e| format!("Failed to check autostart: {}", e))
}

/// Enable or disable autostart.
pub fn set_autostart(app: &AppHandle, enabled: bool) -> Result<(), String> {
    let autolaunch = app.autolaunch();
    if enabled {
        autolaunch
            .enable()
            .map_err(|e| format!("Failed to enable autostart: {}", e))
    } else {
        autolaunch
            .disable()
            .map_err(|e| format!("Failed to disable autostart: {}", e))
    }
}
