use std::path::PathBuf;
use std::process::Command;

use serde::Serialize;
use tauri::AppHandle;
use tauri::Manager;
use tauri::path::BaseDirectory;

#[derive(Debug, Serialize, Clone)]
pub struct PodmanPrerequisites {
    pub wsl_available: bool,
    pub podman_installed: bool,
    pub podman_path: Option<String>,
    pub needs_install: bool,
}

#[derive(Debug, Serialize, Clone)]
pub struct InstallProgress {
    pub stage: String,
    pub message: String,
    pub done: bool,
    pub error: Option<String>,
}

/// Check if WSL2 is available on this Windows machine.
fn check_wsl_available() -> bool {
    // Try `wsl --status` — returns 0 if WSL is installed and configured
    let output = Command::new("wsl")
        .arg("--status")
        .output();

    match output {
        Ok(o) => o.status.success(),
        Err(_) => {
            // wsl binary not found — try checking if the feature is enabled
            let output = Command::new("wsl")
                .arg("--list")
                .arg("--quiet")
                .output();
            matches!(output, Ok(o) if o.status.success())
        }
    }
}

/// Check if Podman is already installed by looking in common locations.
fn find_podman() -> Option<PathBuf> {
    // Check PATH first
    if let Ok(output) = Command::new("where").arg("podman").output() {
        if output.status.success() {
            let stdout = String::from_utf8_lossy(&output.stdout);
            if let Some(line) = stdout.lines().next() {
                let p = PathBuf::from(line.trim());
                if p.exists() {
                    return Some(p);
                }
            }
        }
    }

    // Check common install locations
    let candidates = [
        // Per-user MSI install (v5.7+)
        dirs::home_dir().map(|h| h.join("AppData").join("Local").join("Programs").join("Podman").join("podman.exe")),
        // Machine-scope install
        Some(PathBuf::from(r"C:\Program Files\RedHat\Podman\podman.exe")),
    ];

    for candidate in candidates.into_iter().flatten() {
        if candidate.exists() {
            return Some(candidate);
        }
    }

    None
}

/// Check prerequisites for Podman installation.
pub fn check_prerequisites() -> PodmanPrerequisites {
    let wsl_available = check_wsl_available();
    let podman_path = find_podman();
    let podman_installed = podman_path.is_some();

    PodmanPrerequisites {
        wsl_available,
        podman_installed,
        podman_path: podman_path.map(|p| p.to_string_lossy().into_owned()),
        needs_install: !podman_installed,
    }
}

/// Silently install the bundled Podman MSI.
pub fn install_podman_msi(app: &AppHandle) -> Result<String, String> {
    // Try BaseDirectory::Resource first
    let msi_path = app
        .path()
        .resolve("resources/podman-installer-windows-amd64.msi", BaseDirectory::Resource)
        .ok()
        .filter(|p| p.exists())
        // Fallback: look relative to the executable
        .or_else(|| {
            std::env::current_exe().ok().and_then(|exe| {
                exe.parent().map(|dir| dir.join("resources").join("podman-installer-windows-amd64.msi"))
            }).filter(|p| p.exists())
        })
        // Fallback: try without resources/ prefix (in case BaseDirectory::Resource already includes it)
        .or_else(|| {
            app.path()
                .resolve("podman-installer-windows-amd64.msi", BaseDirectory::Resource)
                .ok()
                .filter(|p| p.exists())
        })
        .ok_or_else(|| "Podman installer not found in any expected location".to_string())?;

    // Run msiexec with per-user silent install (no admin required)
    // MSIINSTALLPERUSER=1 installs to user profile, no elevation needed
    // Strip \\?\ prefix — msiexec doesn't understand extended-length paths
    let msi_str = msi_path.to_string_lossy().to_string();
    let msi_str = msi_str.strip_prefix(r"\\?\").unwrap_or(&msi_str).to_string();
    let output = Command::new("msiexec")
        .arg("/package")
        .arg(&msi_str)
        .arg("/quiet")
        .arg("/norestart")
        .arg("MSIINSTALLPERUSER=1")
        .arg("MACHINE_PROVIDER=wsl")
        .output()
        .map_err(|e| format!("Failed to run msiexec: {}", e))?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        let stdout = String::from_utf8_lossy(&output.stdout);
        let code = output.status.code().unwrap_or(-1);
        let detail = if !stderr.is_empty() { stderr.to_string() } else { stdout.to_string() };
        return Err(format!(
            "Podman installer failed (exit code {}): {} [path: {}]",
            code, detail, msi_str
        ));
    }

    // Find the newly installed podman
    match find_podman() {
        Some(path) => Ok(path.to_string_lossy().into_owned()),
        None => Err("Podman installer succeeded but podman.exe not found. You may need to restart the app.".into()),
    }
}

/// Initialize the Podman machine with the given resources.
pub fn init_podman_machine(podman_path: &str, cpus: i32, memory_mb: i32, disk_gb: i32) -> Result<(), String> {
    let output = Command::new(podman_path)
        .arg("machine")
        .arg("init")
        .arg(format!("--cpus={}", cpus))
        .arg(format!("--memory={}", memory_mb))
        .arg(format!("--disk-size={}", disk_gb))
        .output()
        .map_err(|e| format!("Failed to run podman machine init: {}", e))?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        // "already exists" is not an error
        if stderr.contains("already exists") {
            return Ok(());
        }
        return Err(format!("podman machine init failed: {}", stderr));
    }

    Ok(())
}

/// Start the Podman machine.
pub fn start_podman_machine(podman_path: &str) -> Result<(), String> {
    let output = Command::new(podman_path)
        .arg("machine")
        .arg("start")
        .output()
        .map_err(|e| format!("Failed to run podman machine start: {}", e))?;

    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        // "already running" is not an error
        if stderr.contains("already running") {
            return Ok(());
        }
        return Err(format!("podman machine start failed: {}", stderr));
    }

    Ok(())
}
