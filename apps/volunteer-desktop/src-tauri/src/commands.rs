use std::time::Duration;

use serde::{Deserialize, Serialize};
use tauri::AppHandle;

use crate::api::ManagementClient;
use crate::autostart;
use crate::container_runtime::{ContainerRuntimeStatus, SetupRequest, SetupResponse};
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

#[derive(Debug, Serialize)]
pub struct DaemonConnection {
    pub port: u16,
    pub token: String,
}

#[derive(Debug, Deserialize)]
pub struct InitConfig {
    pub cpu_cores: Option<u32>,
    pub memory_mb: Option<u32>,
    pub gpu_vram_pct: Option<u32>,
    pub disk_gb: Option<u32>,
    pub schedule_mode: Option<String>,
    pub idle_threshold_mins: Option<u32>,
    pub schedule_start_hour: Option<u32>,
    pub schedule_end_hour: Option<u32>,
    pub server_url: Option<String>,
    pub enabled_leafs: Option<Vec<String>>,
}

#[tauri::command]
pub async fn get_daemon_info() -> Result<DaemonConnection, String> {
    let info = sidecar::read_daemon_json()?;
    Ok(DaemonConnection {
        port: info.port,
        token: info.token,
    })
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
        }
    }

    if let Some(leafs) = &config.enabled_leafs {
        if !leafs.is_empty() {
            args.push("--enabled-leafs".into());
            args.push(leafs.join(","));
        }
    }

    let binary = sidecar::find_sidecar_binary()?;

    let output = std::process::Command::new(&binary)
        .args(&args)
        .output()
        .map_err(|e| format!("Failed to run init: {}", e))?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(format!("Init failed: {}", stderr));
    }

    // Start the daemon after init
    sidecar::start_sidecar().map_err(|e| format!("Failed to start daemon: {}", e))?;

    // Wait for daemon to become available
    sidecar::wait_for_daemon(Duration::from_secs(30))?;

    Ok(())
}

#[tauri::command]
pub async fn quit_app(app: AppHandle) -> Result<(), String> {
    let _ = tokio::task::spawn_blocking(|| sidecar::suspend_and_quit_sidecar()).await;
    app.exit(0);
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
pub async fn check_update(app: AppHandle) -> Result<Option<updater::UpdateInfo>, String> {
    updater::check_for_updates(&app).await
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

#[tauri::command]
pub async fn check_podman_prerequisites() -> Result<PodmanPrerequisites, String> {
    Ok(podman_installer::check_prerequisites())
}

#[tauri::command]
pub async fn install_podman(
    app: AppHandle,
    cpus: Option<i32>,
    memory_mb: Option<i32>,
    disk_gb: Option<i32>,
) -> Result<String, String> {
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

/// Total physical system memory in MB. The setup wizard uses this to size the
/// memory slider to real hardware instead of a hardcoded 8 GB assumption — the
/// old hardcode capped advertised memory at ~7.4 GB, below the floor of
/// large-memory leaves (e.g. extract2 needs ≥28 GB), so the volunteer was never
/// matched to that work. Returns 0 on failure; the caller falls back.
#[tauri::command]
pub fn get_system_memory_mb() -> u64 {
    use sysinfo::System;
    let mut sys = System::new();
    sys.refresh_memory();
    sys.total_memory() / 1024 / 1024
}
