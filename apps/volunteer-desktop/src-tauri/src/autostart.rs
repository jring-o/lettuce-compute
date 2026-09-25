use tauri::AppHandle;
use tauri_plugin_autostart::ManagerExt;

/// Passed on the command line of the login-time launch so `main.rs` keeps the
/// window hidden and the app lives in the tray until the user opens it.
pub const MINIMIZED_FLAG: &str = "--minimized";

/// Arguments the autostart entry (registry value, LaunchAgent or .desktop
/// file) launches the app with.
pub fn launch_args() -> Vec<&'static str> {
    vec![MINIMIZED_FLAG]
}

/// What a launch does to the login entry.
#[derive(Debug, PartialEq, Eq)]
enum LaunchAction {
    /// Register the entry again, so its stored command line carries the
    /// current arguments (installs that enabled autostart before the
    /// `--minimized` flag existed registered without it).
    Refresh,
    /// Leave it as it is.
    LeaveAlone,
}

/// A launch only ever refreshes an entry that is already on. Turning it on or
/// off is the volunteer's choice, made in the setup wizard or in Settings ›
/// General › Start on boot; an entry that is off, or whose state cannot be
/// read, stays as it is.
fn launch_action(entry_enabled: &Result<bool, String>) -> LaunchAction {
    match entry_enabled {
        Ok(true) => LaunchAction::Refresh,
        Ok(false) | Err(_) => LaunchAction::LeaveAlone,
    }
}

/// Run at every launch: refresh the login entry if it is on (see
/// `launch_action`).
pub fn setup_autostart(app: &AppHandle) {
    let enabled = is_autostart_enabled(app);
    match (launch_action(&enabled), &enabled) {
        (LaunchAction::Refresh, _) => match app.autolaunch().enable() {
            Ok(()) => log::info!("start at login: on (entry refreshed)"),
            Err(e) => log::warn!("start at login: on, but refreshing the entry failed: {e}"),
        },
        (LaunchAction::LeaveAlone, Err(e)) => log::warn!("start at login: {e}"),
        (LaunchAction::LeaveAlone, Ok(_)) => log::info!("start at login: off"),
    }
}

/// Check if autostart is currently enabled.
pub fn is_autostart_enabled(app: &AppHandle) -> Result<bool, String> {
    app.autolaunch()
        .is_enabled()
        .map_err(|e| format!("Failed to check autostart: {}", e))
}

/// Enable or disable autostart. Disabling an entry that does not exist is
/// done already: on Windows the plugin's `disable()` fails when there is no
/// registry value to delete, and the setup wizard switches it off explicitly
/// when the volunteer unticks the box, which is usually the case.
pub fn set_autostart(app: &AppHandle, enabled: bool) -> Result<(), String> {
    let autolaunch = app.autolaunch();
    let result = if enabled {
        autolaunch
            .enable()
            .map_err(|e| format!("Failed to enable autostart: {}", e))
    } else if let Ok(false) = autolaunch.is_enabled() {
        Ok(())
    } else {
        autolaunch
            .disable()
            .map_err(|e| format!("Failed to disable autostart: {}", e))
    };
    match &result {
        Ok(()) => log::info!("start at login switched {}", if enabled { "on" } else { "off" }),
        Err(e) => log::warn!("{e}"),
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_launch_never_turns_the_login_entry_on() {
        // Including the very first launch of a new install or profile: the
        // entry is created only when the volunteer says so in the wizard.
        assert_eq!(launch_action(&Ok(false)), LaunchAction::LeaveAlone);
        assert_eq!(
            launch_action(&Err("Failed to check autostart: no access".into())),
            LaunchAction::LeaveAlone
        );
    }

    #[test]
    fn a_launch_refreshes_an_entry_that_is_on() {
        assert_eq!(launch_action(&Ok(true)), LaunchAction::Refresh);
    }
}
