use std::time::Duration;

use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Emitter};
use tauri_plugin_notification::NotificationExt;
use tauri_plugin_updater::UpdaterExt;

use crate::api::ManagementClient;
use crate::sidecar;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct UpdateInfo {
    pub version: String,
    pub body: Option<String>,
}

/// Event payload emitted to the frontend when an update is available.
#[derive(Debug, Clone, Serialize)]
struct UpdateAvailableEvent {
    version: String,
    body: Option<String>,
}

/// Event payload emitted during download progress.
#[derive(Debug, Clone, Serialize)]
pub struct UpdateProgressEvent {
    pub progress_pct: u32,
}

/// Check for updates and return info if available.
pub async fn check_for_updates(app: &AppHandle) -> Result<Option<UpdateInfo>, String> {
    let updater = app
        .updater()
        .map_err(|e| format!("Updater not available: {}", e))?;

    match updater.check().await {
        Ok(Some(update)) => Ok(Some(UpdateInfo {
            version: update.version.clone(),
            body: update.body.clone(),
        })),
        Ok(None) => Ok(None),
        Err(e) => Err(format!("Update check failed: {}", e)),
    }
}

/// Download and install an available update.
pub async fn install_update(app: &AppHandle) -> Result<(), String> {
    let updater = app
        .updater()
        .map_err(|e| format!("Updater not available: {}", e))?;

    let update = updater
        .check()
        .await
        .map_err(|e| format!("Update check failed: {}", e))?
        .ok_or_else(|| "No update available".to_string())?;

    let app_clone = app.clone();

    // Download with progress tracking
    let mut downloaded: u64 = 0;

    let bytes = update
        .download(
            |chunk_len, content_len| {
                downloaded += chunk_len as u64;
                let pct = if let Some(total) = content_len {
                    if total > 0 {
                        ((downloaded as f64 / total as f64) * 100.0) as u32
                    } else {
                        0
                    }
                } else {
                    0
                };
                let _ = app_clone.emit(
                    "update:progress",
                    UpdateProgressEvent { progress_pct: pct },
                );
            },
            || {},
        )
        .await
        .map_err(|e| format!("Download failed: {}", e))?;

    // Install the update (this will restart the app)
    update
        .install(bytes)
        .map_err(|e| format!("Installation failed: {}", e))?;

    // Request restart
    app.restart();
}

/// Start periodic update checking (on launch + every 6 hours).
pub fn start_update_poll(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        // Check on launch after a brief delay
        tokio::time::sleep(Duration::from_secs(10)).await;
        do_update_check(&app).await;

        // Then check every 6 hours
        loop {
            tokio::time::sleep(Duration::from_secs(6 * 60 * 60)).await;
            do_update_check(&app).await;
        }
    });
}

/// Check if update notifications are enabled in the user's config.
async fn is_update_notification_enabled() -> bool {
    let info = match sidecar::read_daemon_json() {
        Ok(info) => info,
        Err(_) => return true, // default to enabled if can't read config
    };

    #[derive(Deserialize)]
    struct NotifPrefs {
        updates: bool,
    }
    #[derive(Deserialize)]
    struct ConfigResp {
        notifications: NotifPrefs,
    }

    let client = ManagementClient::from_daemon_info(&info);
    match client.get_json::<ConfigResp>("/api/v1/config").await {
        Ok(resp) => resp.notifications.updates,
        Err(_) => true,
    }
}

async fn do_update_check(app: &AppHandle) {
    match check_for_updates(app).await {
        Ok(Some(info)) => {
            // Only send OS notification if the user has update notifications enabled
            if is_update_notification_enabled().await {
                let _ = app
                    .notification()
                    .builder()
                    .title("Update Available")
                    .body(format!(
                        "Lettuce Compute v{} is available. Open the app to install.",
                        info.version
                    ))
                    .show();
            }

            // Always emit the frontend event so the in-app banner shows
            let _ = app.emit(
                "update:available",
                UpdateAvailableEvent {
                    version: info.version,
                    body: info.body,
                },
            );
        }
        Ok(None) => {}
        Err(e) => {
            eprintln!("Update check failed: {}", e);
        }
    }
}
