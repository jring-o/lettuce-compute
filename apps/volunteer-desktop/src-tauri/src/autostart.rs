use std::path::PathBuf;

use tauri::AppHandle;
use tauri_plugin_autostart::ManagerExt;

use crate::sidecar;

/// Passed on the command line of the login-time launch so `main.rs` keeps the
/// window hidden and the app lives in the tray until the user opens it.
pub const MINIMIZED_FLAG: &str = "--minimized";

/// Arguments the autostart entry (registry value, LaunchAgent or .desktop
/// file) launches the app with.
pub fn launch_args() -> Vec<&'static str> {
    vec![MINIMIZED_FLAG]
}

fn first_launch_marker() -> PathBuf {
    sidecar::data_dir().join(".first_launch_done")
}

/// Set up auto-start. On the first explicit app launch it is enabled by
/// default so the app starts minimized to the tray at login. On later
/// launches an already-enabled entry is re-registered so the stored command
/// line carries the current arguments (installs that enabled autostart before
/// the `--minimized` flag existed registered without it).
pub fn setup_autostart(app: &AppHandle) {
    let marker = first_launch_marker();
    if !marker.exists() {
        // First launch: create marker and enable autostart
        let _ = std::fs::write(&marker, "");
        let _ = app.autolaunch().enable();
        return;
    }
    if let Ok(true) = app.autolaunch().is_enabled() {
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
