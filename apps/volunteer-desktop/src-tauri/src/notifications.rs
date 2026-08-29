use std::collections::HashSet;
use std::sync::Arc;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tauri::AppHandle;
use tauri_plugin_notification::NotificationExt;
use tokio::sync::Mutex;

use crate::api::ManagementClient;
use crate::sidecar;

/// The `notifications` block of `GET /api/v1/config`. Fields the daemon does
/// not send fall back to the app defaults below rather than failing the parse.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default)]
pub struct NotificationPreferences {
    pub credit_milestones: bool,
    pub credit_milestone_threshold: i64,
    pub work_unit_completed: bool,
    pub errors: bool,
    pub updates: bool,
}

impl Default for NotificationPreferences {
    fn default() -> Self {
        Self {
            credit_milestones: true,
            credit_milestone_threshold: 100,
            work_unit_completed: false,
            errors: true,
            updates: true,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
struct MilestoneState {
    highest_milestone: i64,
    triggered: HashSet<i64>,
}

const DEFAULT_MILESTONES: &[i64] = &[100, 500, 1000, 5000, 10000, 50000, 100000];

fn milestones_path() -> std::path::PathBuf {
    sidecar::lettuce_dir().join("milestones.json")
}

fn load_milestone_state() -> MilestoneState {
    let path = milestones_path();
    match std::fs::read_to_string(&path) {
        Ok(data) => serde_json::from_str(&data).unwrap_or_default(),
        Err(_) => MilestoneState::default(),
    }
}

fn save_milestone_state(state: &MilestoneState) {
    let path = milestones_path();
    if let Ok(data) = serde_json::to_string_pretty(state) {
        let _ = std::fs::write(&path, data);
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct ConfigResponse {
    notifications: NotificationPreferences,
}

/// `GET /api/v1/credit`, reduced to the total. Credit is a decimal on the
/// daemon side (heads report fractional credit), so it must be read as f64;
/// milestone arithmetic below works on the whole-credit floor.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct CreditResponse {
    total_credit: f64,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct StatusResponse {
    state: String,
    connected_servers: i64,
    active_tasks: Vec<serde_json::Value>,
}

struct NotificationState {
    prev_credit: i64,
    prev_task_count: usize,
    prev_state: String,
    prev_connected: i64,
    milestones: MilestoneState,
    prefs: NotificationPreferences,
}

pub fn start_notification_poll(app: AppHandle) {
    let state = Arc::new(Mutex::new(NotificationState {
        prev_credit: -1, // sentinel: don't notify on first poll
        prev_task_count: 0,
        prev_state: String::new(),
        prev_connected: 0,
        milestones: load_milestone_state(),
        prefs: NotificationPreferences::default(),
    }));

    tauri::async_runtime::spawn(async move {
        // Wait a bit for daemon to stabilize
        tokio::time::sleep(Duration::from_secs(5)).await;

        loop {
            poll_and_notify(&app, &state).await;
            tokio::time::sleep(Duration::from_secs(5)).await;
        }
    });
}

async fn poll_and_notify(app: &AppHandle, state: &Arc<Mutex<NotificationState>>) {
    let info = match sidecar::read_daemon_json() {
        Ok(info) => info,
        Err(_) => return,
    };

    let client = ManagementClient::from_daemon_info(&info);

    // Fetch preferences from config
    if let Ok(config) = client.get_json::<ConfigResponse>("/api/v1/config").await {
        let mut s = state.lock().await;
        s.prefs = config.notifications;
    }

    // Fetch status
    let status = match client.get_json::<StatusResponse>("/api/v1/status").await.ok() {
        Some(status) => status,
        None => return,
    };

    // Fetch credit
    let credit: Option<CreditResponse> = client.get_json("/api/v1/credit").await.ok();

    let mut s = state.lock().await;

    // Check credit milestones
    if let Some(credit_resp) = &credit {
        let total_credit = credit_resp.total_credit.max(0.0).floor() as i64;
        if s.prefs.credit_milestones && s.prev_credit >= 0 {
            check_credit_milestones(app, &mut s, total_credit);
        }
        s.prev_credit = total_credit;
    }

    // Check work unit completion (task count decreased = something finished)
    let current_task_count = status.active_tasks.len();
    if s.prefs.work_unit_completed
        && s.prev_task_count > 0
        && current_task_count < s.prev_task_count
    {
        send_notification(app, "Task Complete", "A work unit has been completed.");
    }
    s.prev_task_count = current_task_count;

    // Check error conditions
    if s.prefs.errors {
        check_error_conditions(app, &s, &status);
    }

    s.prev_state = status.state;
    s.prev_connected = status.connected_servers;
}

fn check_credit_milestones(app: &AppHandle, state: &mut NotificationState, total_credit: i64) {
    let threshold = state.prefs.credit_milestone_threshold;

    if threshold > 0 {
        // Custom threshold mode: trigger at every multiple
        let prev_multiple = state.prev_credit / threshold;
        let curr_multiple = total_credit / threshold;

        if curr_multiple > prev_multiple && total_credit > 0 {
            let milestone = curr_multiple * threshold;
            if !state.milestones.triggered.contains(&milestone) {
                state.milestones.triggered.insert(milestone);
                state.milestones.highest_milestone = milestone;
                save_milestone_state(&state.milestones);
                send_notification(
                    app,
                    "Credit Milestone!",
                    &format!("You've earned {} total credit!", milestone),
                );
            }
        }
    } else {
        // Default milestone thresholds
        for &milestone in DEFAULT_MILESTONES {
            if total_credit >= milestone
                && state.prev_credit < milestone
                && !state.milestones.triggered.contains(&milestone)
            {
                state.milestones.triggered.insert(milestone);
                state.milestones.highest_milestone = milestone;
                save_milestone_state(&state.milestones);
                send_notification(
                    app,
                    "Credit Milestone!",
                    &format!("You've earned {} total credit!", milestone),
                );
            }
        }
    }
}

fn check_error_conditions(
    app: &AppHandle,
    state: &NotificationState,
    status: &StatusResponse,
) {
    // All servers disconnected (but were previously connected)
    if status.connected_servers == 0 && state.prev_connected > 0 && !state.prev_state.is_empty() {
        send_notification(
            app,
            "Attention Required",
            "All servers disconnected. Check your network connection.",
        );
    }
}

fn send_notification(app: &AppHandle, title: &str, body: &str) {
    let _ = app
        .notification()
        .builder()
        .title(title)
        .body(body)
        .show();
}

