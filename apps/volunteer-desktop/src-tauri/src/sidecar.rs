use std::fs;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use crate::api::DaemonInfo;
use crate::container_runtime;

pub fn lettuce_dir() -> PathBuf {
    dirs::home_dir()
        .expect("Could not determine home directory")
        .join(".lettuce")
}

fn daemon_json_path() -> PathBuf {
    lettuce_dir().join("daemon.json")
}

fn config_yaml_path() -> PathBuf {
    lettuce_dir().join("config.yaml")
}

fn sidecar_binary_name() -> &'static str {
    if cfg!(target_os = "windows") {
        "lettuce-volunteer.exe"
    } else {
        "lettuce-volunteer"
    }
}

pub fn read_daemon_json() -> Result<DaemonInfo, String> {
    let path = daemon_json_path();
    let contents =
        fs::read_to_string(&path).map_err(|e| format!("Failed to read daemon.json: {}", e))?;
    serde_json::from_str(&contents).map_err(|e| format!("Failed to parse daemon.json: {}", e))
}

pub fn is_daemon_running() -> bool {
    match read_daemon_json() {
        Ok(info) => is_pid_alive(info.pid),
        Err(_) => false,
    }
}

pub fn ensure_initialized() -> Result<bool, String> {
    Ok(!config_yaml_path().exists())
}

pub fn start_sidecar() -> Result<Child, String> {
    if is_daemon_running() {
        return Err("Daemon is already running".into());
    }

    let binary = find_sidecar_binary()?;

    let mut cmd = Command::new(&binary);
    cmd.arg("start")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());

    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x08000000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }

    let child = cmd
        .spawn()
        .map_err(|e| format!("Failed to start sidecar: {}", e))?;

    Ok(child)
}

pub fn wait_for_daemon(timeout: Duration) -> Result<DaemonInfo, String> {
    let start = Instant::now();
    let poll_interval = Duration::from_millis(100);

    loop {
        if start.elapsed() > timeout {
            return Err("Timed out waiting for daemon to start".into());
        }

        if let Ok(info) = read_daemon_json() {
            if is_pid_alive(info.pid) {
                return Ok(info);
            }
        }

        std::thread::sleep(poll_interval);
    }
}

pub fn stop_sidecar(pid: u32) -> Result<(), String> {
    terminate_process(pid)?;

    let timeout = Duration::from_secs(30);
    let start = Instant::now();
    let poll_interval = Duration::from_millis(200);

    loop {
        if !is_pid_alive(pid) {
            let _ = fs::remove_file(daemon_json_path());
            return Ok(());
        }

        if start.elapsed() > timeout {
            force_kill(pid)?;
            let _ = fs::remove_file(daemon_json_path());
            return Ok(());
        }

        std::thread::sleep(poll_interval);
    }
}

pub fn shutdown(app: &tauri::AppHandle) {
    if let Ok(info) = read_daemon_json() {
        container_runtime::ensure_podman_state(&info, "stopped");
        let _ = stop_sidecar(info.pid);
    }
    app.exit(0);
}

/// Ask the daemon to suspend all compute, save PIDs, release Job Object, and exit.
/// Frozen processes survive as orphans for the next launch.
pub fn suspend_and_quit_sidecar() -> Result<(), String> {
    let info = read_daemon_json()?;
    let client = crate::api::ManagementClient::from_daemon_info(&info);

    // Tell daemon to suspend-and-quit via the management API.
    let rt = tokio::runtime::Runtime::new().map_err(|e| e.to_string())?;
    let _ = rt.block_on(async { client.suspend_and_quit().await });

    // Wait for daemon process to actually exit.
    let timeout = Duration::from_secs(30);
    let start = Instant::now();
    let poll_interval = Duration::from_millis(200);

    loop {
        if !is_pid_alive(info.pid) {
            let _ = fs::remove_file(daemon_json_path());
            return Ok(());
        }

        if start.elapsed() > timeout {
            // Daemon didn't exit in time — force kill as last resort.
            force_kill(info.pid)?;
            let _ = fs::remove_file(daemon_json_path());
            return Ok(());
        }

        std::thread::sleep(poll_interval);
    }
}

pub fn find_sidecar_binary() -> Result<PathBuf, String> {
    let binary_name = sidecar_binary_name();

    // Check next to the app binary first
    if let Ok(exe_path) = std::env::current_exe() {
        if let Some(exe_dir) = exe_path.parent() {
            let candidate = exe_dir.join(binary_name);
            if candidate.exists() {
                return Ok(candidate);
            }
        }
    }

    // Fall back to PATH
    Ok(PathBuf::from(binary_name))
}

#[cfg(unix)]
fn is_pid_alive(pid: u32) -> bool {
    unsafe { libc::kill(pid as i32, 0) == 0 }
}

#[cfg(windows)]
fn is_pid_alive(pid: u32) -> bool {
    const PROCESS_QUERY_LIMITED_INFORMATION: u32 = 0x1000;
    const STILL_ACTIVE: u32 = 259;

    unsafe {
        let handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
        if handle.is_null() {
            return false;
        }
        let mut exit_code: u32 = 0;
        let result = GetExitCodeProcess(handle, &mut exit_code);
        CloseHandle(handle);
        result != 0 && exit_code == STILL_ACTIVE
    }
}

#[cfg(windows)]
extern "system" {
    fn OpenProcess(access: u32, inherit: i32, pid: u32) -> *mut std::ffi::c_void;
    fn GetExitCodeProcess(handle: *mut std::ffi::c_void, exit_code: *mut u32) -> i32;
    fn CloseHandle(handle: *mut std::ffi::c_void) -> i32;
    fn TerminateProcess(handle: *mut std::ffi::c_void, exit_code: u32) -> i32;
}

#[cfg(unix)]
fn terminate_process(pid: u32) -> Result<(), String> {
    let result = unsafe { libc::kill(pid as i32, libc::SIGTERM) };
    if result != 0 {
        return Err(format!("Failed to send SIGTERM to PID {}", pid));
    }
    Ok(())
}

#[cfg(windows)]
fn terminate_process(pid: u32) -> Result<(), String> {
    const PROCESS_TERMINATE: u32 = 0x0001;

    unsafe {
        let handle = OpenProcess(PROCESS_TERMINATE, 0, pid);
        if handle.is_null() {
            return Err(format!("Failed to open process PID {}", pid));
        }
        let result = TerminateProcess(handle, 1);
        CloseHandle(handle);
        if result == 0 {
            return Err(format!("Failed to terminate PID {}", pid));
        }
    }
    Ok(())
}

#[cfg(unix)]
fn force_kill(pid: u32) -> Result<(), String> {
    let result = unsafe { libc::kill(pid as i32, libc::SIGKILL) };
    if result != 0 {
        return Err(format!("Failed to send SIGKILL to PID {}", pid));
    }
    Ok(())
}

#[cfg(windows)]
fn force_kill(pid: u32) -> Result<(), String> {
    // On Windows, TerminateProcess is already a force kill
    terminate_process(pid)
}
