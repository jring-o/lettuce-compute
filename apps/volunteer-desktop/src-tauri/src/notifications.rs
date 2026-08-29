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
    sidecar::data_dir().join("milestones.json")
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

/// One entry of `GET /api/v1/notices`: something the daemon wants the
/// volunteer to know about (a head rejecting results, a leaf failing here,
/// disk running out). `count` is how many times it has repeated.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct Notice {
    pub id: i64,
    pub level: String,
    pub code: String,
    pub message: String,
    pub head: Option<String>,
    pub leaf: Option<String>,
    pub count: i64,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct NoticesResponse {
    notices: Vec<Notice>,
    latest_id: i64,
}

/// The slice of `GET /api/v1/heads` the notifier reads: which heads refuse this
/// client version until the app is updated.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct HeadsResponse {
    heads: Vec<HeadEntry>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
struct HeadEntry {
    name: String,
    update_required: bool,
}

struct NotificationState {
    prev_credit: i64,
    prev_task_count: usize,
    prev_state: String,
    prev_connected: i64,
    milestones: MilestoneState,
    prefs: NotificationPreferences,
    /// Notices: `None` until the first successful poll establishes where "new"
    /// begins, then the highest id seen. Notices already retained by the
    /// daemon when the app starts are shown in the window, not notified.
    notices_since: Option<i64>,
    /// False once the daemon answered 404: this CLI build has no notices route.
    notices_supported: bool,
    /// Notice ids already notified, so a repeat (same id, higher count) is quiet.
    notified_notice_ids: HashSet<i64>,
    /// Heads whose update-required state was notified this session.
    update_required_notified: HashSet<String>,
}

pub fn start_notification_poll(app: AppHandle) {
    let state = Arc::new(Mutex::new(NotificationState {
        prev_credit: -1, // sentinel: don't notify on first poll
        prev_task_count: 0,
        prev_state: String::new(),
        prev_connected: 0,
        milestones: load_milestone_state(),
        prefs: NotificationPreferences::default(),
        notices_since: None,
        notices_supported: true,
        notified_notice_ids: HashSet::new(),
        update_required_notified: HashSet::new(),
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

    // Fetch new notices (only once the baseline is known and the route exists).
    let notices = fetch_notices(&client, state).await;

    // Heads refusing this client version.
    let heads: Option<HeadsResponse> = client.get_json("/api/v1/heads").await.ok();

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

    // Daemon notices: warnings and errors, each id once.
    if let Some(list) = notices {
        for n in notice_titles(&list, &s.prefs) {
            if s.notified_notice_ids.insert(n.0) {
                send_notification(app, &n.1, &n.2);
            }
        }
    }

    // Update required by a head: once per head per app session. This is both
    // an update matter and an error (no work flows until the app is updated),
    // so either preference enables it.
    if let Some(heads) = heads {
        if s.prefs.updates || s.prefs.errors {
            for head in heads.heads.iter().filter(|h| h.update_required) {
                if s.update_required_notified.insert(head.name.clone()) {
                    send_notification(
                        app,
                        "Update required",
                        &format!(
                            "This app is too old for {} — update Lettuce Compute to keep receiving work from it.",
                            head.name
                        ),
                    );
                }
            }
        }
    }

    s.prev_state = status.state;
    s.prev_connected = status.connected_servers;
}

/// Poll `GET /api/v1/notices`. The first successful call only records the
/// daemon's `latest_id` (nothing is notified for notices that predate the
/// app), later calls ask for everything newer. A 404 marks the route as
/// unsupported by this CLI build and no further requests are made.
async fn fetch_notices(
    client: &ManagementClient,
    state: &Arc<Mutex<NotificationState>>,
) -> Option<Vec<Notice>> {
    let (supported, since) = {
        let s = state.lock().await;
        (s.notices_supported, s.notices_since)
    };
    if !supported {
        return None;
    }

    let path = match since {
        Some(id) => format!("/api/v1/notices?since={id}"),
        None => "/api/v1/notices".to_string(),
    };
    let value = match client
        .request_value(reqwest::Method::GET, &path, None, crate::api::DEFAULT_TIMEOUT)
        .await
    {
        Ok(v) => v,
        Err(e) => {
            if e.status == 404 {
                state.lock().await.notices_supported = false;
            }
            return None;
        }
    };
    let resp: NoticesResponse = serde_json::from_value(value).ok()?;

    let mut s = state.lock().await;
    let newest = resp
        .notices
        .iter()
        .map(|n| n.id)
        .fold(resp.latest_id, i64::max);
    match s.notices_since {
        None => {
            // Baseline: remember where "new" starts; do not notify the backlog.
            s.notices_since = Some(newest);
            None
        }
        Some(prev) => {
            if resp.latest_id < prev {
                // The daemon restarted and its ids began again; re-baseline so
                // the retained backlog of the new daemon is not replayed.
                s.notices_since = Some(newest);
                s.notified_notice_ids.clear();
                return None;
            }
            s.notices_since = Some(newest.max(prev));
            Some(resp.notices)
        }
    }
}

/// Which of `notices` deserve an OS notification under `prefs`, as
/// (id, title, body). Only warnings and errors qualify, and only when the
/// "errors requiring attention" preference is on.
fn notice_titles(notices: &[Notice], prefs: &NotificationPreferences) -> Vec<(i64, String, String)> {
    if !prefs.errors {
        return Vec::new();
    }
    notices
        .iter()
        .filter(|n| n.level == "warn" || n.level == "error")
        .map(|n| {
            let title = match n.level.as_str() {
                "error" => "Lettuce needs attention",
                _ => "Lettuce warning",
            };
            let scope: Vec<&str> = [n.head.as_deref(), n.leaf.as_deref()]
                .into_iter()
                .flatten()
                .filter(|s| !s.is_empty())
                .collect();
            let mut body = n.message.clone();
            if !scope.is_empty() {
                body = format!("{} — {}", scope.join(" / "), body);
            }
            if n.count > 1 {
                body = format!("{body} ({}×)", n.count);
            }
            (n.id, title.to_string(), body)
        })
        .collect()
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

#[cfg(test)]
mod tests {
    use super::*;

    fn notice(id: i64, level: &str) -> Notice {
        Notice {
            id,
            level: level.into(),
            code: "X".into(),
            message: format!("message {id}"),
            head: None,
            leaf: None,
            count: 1,
        }
    }

    #[test]
    fn only_warn_and_error_notices_are_notified() {
        let prefs = NotificationPreferences::default();
        let out = notice_titles(
            &[notice(1, "info"), notice(2, "warn"), notice(3, "error")],
            &prefs,
        );
        assert_eq!(out.iter().map(|n| n.0).collect::<Vec<_>>(), vec![2, 3]);
        assert_eq!(out[0].1, "Lettuce warning");
        assert_eq!(out[1].1, "Lettuce needs attention");
    }

    #[test]
    fn errors_preference_silences_notices() {
        let prefs = NotificationPreferences {
            errors: false,
            ..Default::default()
        };
        assert!(notice_titles(&[notice(1, "error")], &prefs).is_empty());
    }

    #[test]
    fn body_carries_scope_and_repeat_count() {
        let mut n = notice(4, "error");
        n.head = Some("lettuce.science".into());
        n.leaf = Some("prime".into());
        n.count = 3;
        let out = notice_titles(&[n], &NotificationPreferences::default());
        assert_eq!(out[0].2, "lettuce.science / prime — message 4 (3×)");
    }
}
