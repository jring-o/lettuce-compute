use std::sync::{Mutex, OnceLock};
use std::time::Duration;

use serde::{Deserialize, Serialize};
use sysinfo::System;
use tauri::AppHandle;

use crate::api::ManagementClient;
use crate::autostart;
use crate::container_runtime::{
    self, ContainerRuntimeDetection, ContainerRuntimeStatus, SetupRequest, SetupResponse,
};
use crate::podman_installer::{self, PodmanPrerequisites};
use crate::sidecar;
use crate::updater;

// --- Remote server commands (bypass browser CORS via Rust HTTP client) ---

#[derive(Debug, Serialize, Deserialize)]
pub struct HealthResponse {
    pub status: String,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct HeadLeaf {
    pub slug: String,
    pub name: String,
    #[serde(default)]
    pub research_area: serde_json::Value,
    #[serde(default)]
    pub state: String,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct HeadInfo {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub leafs: Vec<HeadLeaf>,
}

fn normalize_url(url: &str) -> String {
    let trimmed = url.trim();
    if trimmed.starts_with("http://") || trimmed.starts_with("https://") {
        trimmed.to_string()
    } else {
        format!("https://{trimmed}")
    }
}

#[tauri::command]
pub async fn test_server_connection(url: String) -> Result<HealthResponse, String> {
    let base = normalize_url(&url);
    let resp = reqwest::get(format!("{base}/api/v1/health"))
        .await
        .map_err(|e| format!("Connection failed: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("Server returned {}", resp.status()));
    }
    resp.json::<HealthResponse>()
        .await
        .map_err(|e| format!("Invalid response: {e}"))
}

#[tauri::command]
pub async fn fetch_head_info(url: String) -> Result<HeadInfo, String> {
    let base = normalize_url(&url);
    let resp = reqwest::get(format!("{base}/api/v1/head"))
        .await
        .map_err(|e| format!("Failed to fetch head info: {e}"))?;
    if !resp.status().is_success() {
        return Err(format!("Server returned {}", resp.status()));
    }
    resp.json::<HeadInfo>()
        .await
        .map_err(|e| format!("Invalid response: {e}"))
}

fn daemon_client() -> Result<ManagementClient, String> {
    let info = sidecar::read_daemon_json()?;
    Ok(ManagementClient::from_daemon_info(&info))
}

/// One daily computing window, applied with `lettuce-volunteer schedule set`
/// after `init`. Hours are whole hours 0–23; `to_hour <= from_hour` wraps past
/// midnight (equal hours mean the whole day). `days` are the CLI's own names
/// (`mon` … `sun`).
#[derive(Debug, Deserialize)]
pub struct ScheduleWindow {
    pub from_hour: u32,
    pub to_hour: u32,
    #[serde(default)]
    pub days: Vec<String>,
}

/// Choices the setup wizard hands to `lettuce-volunteer init`.
#[derive(Debug, Deserialize)]
pub struct InitConfig {
    pub cpu_cores: Option<u32>,
    pub memory_mb: Option<u32>,
    pub gpu_vram_pct: Option<u32>,
    pub disk_gb: Option<u32>,
    /// `init --schedule-mode`: `always`, `idle` or `scheduled`. The wizard
    /// sends `always` together with `schedule_window` for a scheduled setup,
    /// because a non-interactive `init --schedule-mode scheduled` has no
    /// window to write and fails the CLI's validation.
    pub schedule_mode: Option<String>,
    pub idle_threshold_mins: Option<u32>,
    /// When present, `schedule set` runs after `init` and before the daemon
    /// starts, which switches the mode to `SCHEDULED` with this window.
    #[serde(default)]
    pub schedule_window: Option<ScheduleWindow>,
    pub server_url: Option<String>,
    /// Runtimes the volunteer trusts the head at `server_url` to run on this
    /// machine beyond the always-allowed WASM sandbox: any of `container`,
    /// `native` (case-insensitive). Empty means WASM only. Sent as `--trust`
    /// whenever `--server` is sent.
    #[serde(default)]
    pub trust: Vec<String>,
    pub enabled_leafs: Option<Vec<String>>,
}

/// Build the `--trust` value for `lettuce-volunteer init`. The accepted names
/// mirror the CLI's `parseTrustRuntimes`: `container` and `native` opt in,
/// `wasm` and `none` are no-ops (WASM is always allowed). An empty selection is
/// sent as `none` so the head is recorded as an explicit WASM-only decision.
fn trust_flag_value(trust: &[String]) -> Result<String, String> {
    let mut chosen: Vec<String> = Vec::new();
    for raw in trust {
        let name = raw.trim().to_ascii_lowercase();
        match name.as_str() {
            "" | "none" | "wasm" => {}
            "container" | "native" => {
                if !chosen.contains(&name) {
                    chosen.push(name);
                }
            }
            other => {
                return Err(format!(
                    "Unknown runtime trust value {other:?} (valid: container, native)"
                ))
            }
        }
    }
    if chosen.is_empty() {
        return Ok("none".into());
    }
    chosen.sort();
    Ok(chosen.join(","))
}

const WEEKDAYS: [&str; 7] = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"];

/// The argument list for `lettuce-volunteer schedule set` describing
/// `window`: `--from HH:00 --to HH:00 --days mon,tue,...`. Hours must be
/// 0–23 and every day must be one of the CLI's names; the days are emitted in
/// week order without duplicates (the `--days` syntax is a comma list, see
/// `parseScheduleDays` in the CLI's `schedule.go`).
fn schedule_set_args(window: &ScheduleWindow) -> Result<Vec<String>, String> {
    if window.from_hour > 23 || window.to_hour > 23 {
        return Err(format!(
            "Schedule hours must be 0-23 (got from {} to {})",
            window.from_hour, window.to_hour
        ));
    }
    let mut chosen: Vec<&str> = Vec::new();
    for raw in &window.days {
        let name = raw.trim().to_ascii_lowercase();
        match WEEKDAYS.iter().find(|d| **d == name) {
            Some(day) => {
                if !chosen.contains(day) {
                    chosen.push(day);
                }
            }
            None => {
                return Err(format!(
                    "Unknown schedule day {raw:?} (valid: {})",
                    WEEKDAYS.join(", ")
                ))
            }
        }
    }
    if chosen.is_empty() {
        return Err("A schedule window needs at least one day".into());
    }
    chosen.sort_by_key(|d| WEEKDAYS.iter().position(|w| w == d));
    Ok(vec![
        "schedule".into(),
        "set".into(),
        "--from".into(),
        format!("{:02}:00", window.from_hour),
        "--to".into(),
        format!("{:02}:00", window.to_hour),
        "--days".into(),
        chosen.join(","),
    ])
}

/// Apply a schedule window with `lettuce-volunteer schedule set`. Must run
/// after `init` (the CLI loads config.yaml first) and before `start` (the
/// daemon reads the schedule at boot).
fn apply_schedule_window(window: &ScheduleWindow) -> Result<(), String> {
    let args = schedule_set_args(window)?;
    let output = sidecar::run_sidecar(&args)?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(format!("Schedule setup failed: {}", stderr.trim()));
    }
    Ok(())
}

#[tauri::command]
pub async fn is_initialized() -> Result<bool, String> {
    let needs_wizard = sidecar::ensure_initialized()?;
    Ok(!needs_wizard)
}

#[tauri::command]
pub async fn run_init(config: InitConfig) -> Result<(), String> {
    let mut args = vec!["init".to_string()];

    if let Some(cores) = config.cpu_cores {
        args.push("--cpu-cores".into());
        args.push(cores.to_string());
    }

    if let Some(mem) = config.memory_mb {
        args.push("--memory-mb".into());
        args.push(mem.to_string());
    }

    if let Some(gpu) = config.gpu_vram_pct {
        args.push("--gpu-vram-pct".into());
        args.push(gpu.to_string());
    }

    if let Some(disk) = config.disk_gb {
        args.push("--disk-gb".into());
        args.push(disk.to_string());
    }

    if let Some(mode) = &config.schedule_mode {
        args.push("--schedule-mode".into());
        args.push(mode.clone());
    }

    if let Some(threshold) = config.idle_threshold_mins {
        args.push("--idle-threshold".into());
        args.push(threshold.to_string());
    }

    if let Some(url) = &config.server_url {
        if !url.is_empty() {
            args.push("--server".into());
            args.push(url.clone());
            args.push("--trust".into());
            args.push(trust_flag_value(&config.trust)?);
        }
    }

    if let Some(leafs) = &config.enabled_leafs {
        if !leafs.is_empty() {
            args.push("--enabled-leafs".into());
            args.push(leafs.join(","));
        }
    }

    let output = sidecar::run_sidecar(&args)?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(format!("Init failed: {}", stderr));
    }

    if let Some(window) = &config.schedule_window {
        apply_schedule_window(window)?;
    }

    // Start the daemon after init
    sidecar::start_sidecar().map_err(|e| format!("Failed to start daemon: {}", e))?;

    // Wait for daemon to become available
    sidecar::wait_for_daemon(Duration::from_secs(30))?;

    Ok(())
}

#[tauri::command]
pub async fn is_autostart_enabled(app: AppHandle) -> Result<bool, String> {
    autostart::is_autostart_enabled(&app)
}

#[tauri::command]
pub async fn set_autostart(app: AppHandle, enabled: bool) -> Result<(), String> {
    autostart::set_autostart(&app, enabled)
}

#[tauri::command]
pub async fn regenerate_keypair() -> Result<String, String> {
    daemon_client()?.regenerate_keypair().await
}

#[tauri::command]
pub async fn install_update(app: AppHandle) -> Result<(), String> {
    updater::install_update(&app).await
}

#[tauri::command]
pub async fn get_container_runtime_status() -> Result<ContainerRuntimeStatus, String> {
    daemon_client()?.get_container_runtime_status().await
}

#[tauri::command]
pub async fn setup_container_runtime(
    cpus: Option<i32>,
    memory_mb: Option<i32>,
    disk_gb: Option<i32>,
) -> Result<SetupResponse, String> {
    let req = if cpus.is_some() || memory_mb.is_some() || disk_gb.is_some() {
        Some(SetupRequest {
            cpus,
            memory_mb,
            disk_gb,
        })
    } else {
        None
    };
    daemon_client()?.setup_container_runtime(req).await
}

#[tauri::command]
pub async fn start_container_runtime() -> Result<SetupResponse, String> {
    daemon_client()?.start_container_runtime().await
}

#[tauri::command]
pub async fn stop_container_runtime() -> Result<SetupResponse, String> {
    daemon_client()?.stop_container_runtime().await
}

/// Probe this machine for a container engine without the daemon (the setup
/// wizard runs before one exists). Works on every platform; see
/// `container_runtime::detect`.
#[tauri::command]
pub async fn detect_container_runtime() -> Result<ContainerRuntimeDetection, String> {
    tokio::task::spawn_blocking(container_runtime::detect)
        .await
        .map_err(|e| e.to_string())
}

#[tauri::command]
pub async fn check_podman_prerequisites() -> Result<PodmanPrerequisites, String> {
    Ok(podman_installer::check_prerequisites())
}

/// Windows only: install the bundled Podman MSI if needed, then create and
/// start its machine. Other platforms have no bundled installer, so the call
/// is refused up front rather than failing on the WSL check.
#[tauri::command]
pub async fn install_podman(
    app: AppHandle,
    cpus: Option<i32>,
    memory_mb: Option<i32>,
    disk_gb: Option<i32>,
) -> Result<String, String> {
    if !cfg!(target_os = "windows") {
        return Err(
            "The bundled Podman installer is only available on Windows. Install Podman Desktop or Docker Desktop (macOS) or Podman from your distribution's packages (Linux)."
                .into(),
        );
    }

    let prereqs = podman_installer::check_prerequisites();

    if !prereqs.wsl_available {
        return Err("WSL2 is required but not available. Please enable WSL2 first: open PowerShell as Administrator and run 'wsl --install', then restart your computer.".into());
    }

    // Install Podman if not already installed
    let podman_path = if prereqs.podman_installed {
        prereqs.podman_path.unwrap()
    } else {
        podman_installer::install_podman_msi(&app)?
    };

    // Init and start the machine
    let cpu = cpus.unwrap_or(2);
    let mem = memory_mb.unwrap_or(4096);
    let disk = disk_gb.unwrap_or(20);

    podman_installer::init_podman_machine(&podman_path, cpu, mem, disk)?;
    podman_installer::start_podman_machine(&podman_path)?;

    Ok(podman_path)
}

/// Restart the volunteer daemon (graceful stop, forced after 30 s, then start).
/// Blocks for up to about 70 s in the worst case, off the async runtime.
#[tauri::command]
pub async fn restart_daemon() -> Result<(), String> {
    tokio::task::spawn_blocking(sidecar::restart_daemon)
        .await
        .map_err(|e| e.to_string())?
        .map(|_| ())
}

/// The data directory the app and its bundled client use: `~/.lettuce`, or
/// the `LETTUCE_DATA_DIR` override (see `sidecar::data_dir`). Shown in
/// Settings so a volunteer running a second profile can see which one this is.
#[tauri::command]
pub fn get_data_dir() -> String {
    sidecar::data_dir().to_string_lossy().into_owned()
}

/// The bundled CLI's version string (`lettuce-volunteer --version`).
#[tauri::command]
pub async fn get_client_version() -> Result<String, String> {
    tokio::task::spawn_blocking(sidecar::client_version)
        .await
        .map_err(|e| e.to_string())?
}

/// Host-wide resource usage read by the app itself. The daemon's
/// `GET /api/v1/metrics` reports zeros for these (it has no platform collector).
#[derive(Debug, Clone, Serialize)]
pub struct SystemMetrics {
    pub cpu_usage_pct: f64,
    pub memory_used_mb: u64,
    pub memory_total_mb: u64,
}

/// One `sysinfo::System` kept for the life of the process. CPU usage is the
/// change between two samples, so a fresh `System` per call would always read
/// zero; keeping one means each call measures since the previous call.
fn shared_system() -> &'static Mutex<System> {
    static SYSTEM: OnceLock<Mutex<System>> = OnceLock::new();
    SYSTEM.get_or_init(|| {
        let mut sys = System::new();
        // Prime the first sample so the very first reading is real, not zero.
        sys.refresh_cpu_usage();
        std::thread::sleep(sysinfo::MINIMUM_CPU_UPDATE_INTERVAL);
        sys.refresh_cpu_usage();
        Mutex::new(sys)
    })
}

#[tauri::command]
pub async fn system_metrics() -> Result<SystemMetrics, String> {
    tokio::task::spawn_blocking(|| {
        let mut sys = shared_system()
            .lock()
            .map_err(|_| "system metrics state is poisoned".to_string())?;
        sys.refresh_cpu_usage();
        sys.refresh_memory();
        Ok(SystemMetrics {
            cpu_usage_pct: f64::from(sys.global_cpu_usage()),
            memory_used_mb: sys.used_memory() / 1024 / 1024,
            memory_total_mb: sys.total_memory() / 1024 / 1024,
        })
    })
    .await
    .map_err(|e| e.to_string())?
}

/// Total physical system memory in MB. The setup wizard uses this to size the
/// memory slider to real hardware instead of a hardcoded 8 GB assumption — the
/// old hardcode capped advertised memory at ~7.4 GB, below the floor of
/// large-memory leaves (e.g. extract2 needs ≥28 GB), so the volunteer was never
/// matched to that work. Returns 0 on failure; the caller falls back.
#[tauri::command]
pub fn get_system_memory_mb() -> u64 {
    let mut sys = System::new();
    sys.refresh_memory();
    sys.total_memory() / 1024 / 1024
}

#[cfg(test)]
mod tests {
    use super::{schedule_set_args, trust_flag_value, ScheduleWindow};

    fn strings(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn schedule_window_becomes_schedule_set_args() {
        let window = ScheduleWindow {
            from_hour: 20,
            to_hour: 6,
            days: strings(&["sun", "Mon", "sun"]),
        };
        assert_eq!(
            schedule_set_args(&window).unwrap(),
            strings(&["schedule", "set", "--from", "20:00", "--to", "06:00", "--days", "mon,sun"])
        );
    }

    #[test]
    fn schedule_window_rejects_bad_input() {
        assert!(schedule_set_args(&ScheduleWindow {
            from_hour: 24,
            to_hour: 6,
            days: strings(&["mon"]),
        })
        .is_err());
        assert!(schedule_set_args(&ScheduleWindow {
            from_hour: 1,
            to_hour: 2,
            days: strings(&["funday"]),
        })
        .is_err());
        assert!(schedule_set_args(&ScheduleWindow {
            from_hour: 1,
            to_hour: 2,
            days: vec![],
        })
        .is_err());
    }

    #[test]
    fn empty_trust_is_none() {
        assert_eq!(trust_flag_value(&[]).unwrap(), "none");
        assert_eq!(trust_flag_value(&strings(&["wasm", "none", ""])).unwrap(), "none");
    }

    #[test]
    fn trust_is_normalised_and_deduplicated() {
        assert_eq!(
            trust_flag_value(&strings(&["Native", "container", "CONTAINER"])).unwrap(),
            "container,native"
        );
    }

    #[test]
    fn unknown_trust_is_rejected() {
        assert!(trust_flag_value(&strings(&["docker"])).is_err());
    }
}
