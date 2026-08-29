use std::ffi::OsStr;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Output, Stdio};
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

/// A `Command` for the sidecar binary that never opens a console window.
fn sidecar_command(binary: &Path) -> Command {
    let mut cmd = Command::new(binary);
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x08000000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    cmd
}

/// Run the sidecar with the given arguments and wait for it to exit, capturing
/// its output. Used for the CLI's one-shot subcommands (`init`, `stop`,
/// `--version`); the long-running daemon is launched with `start_sidecar`.
pub fn run_sidecar<S: AsRef<OsStr>>(args: &[S]) -> Result<Output, String> {
    let binary = find_sidecar_binary()?;
    let mut cmd = sidecar_command(&binary);
    cmd.args(args).stdin(Stdio::null());
    cmd.output().map_err(|e| {
        let shown: Vec<String> = args
            .iter()
            .map(|a| a.as_ref().to_string_lossy().into_owned())
            .collect();
        format!("Failed to run {} {}: {}", binary.display(), shown.join(" "), e)
    })
}

pub fn start_sidecar() -> Result<Child, String> {
    if is_daemon_running() {
        return Err("Daemon is already running".into());
    }

    let binary = find_sidecar_binary()?;

    let mut cmd = sidecar_command(&binary);
    cmd.arg("start")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());

    let child = cmd
        .spawn()
        .map_err(|e| format!("Failed to start sidecar: {}", e))?;

    Ok(child)
}

/// The sidecar's version string, from `lettuce-volunteer --version`. The CLI
/// prints `lettuce-volunteer version <v>`; only the version token is returned.
pub fn client_version() -> Result<String, String> {
    let out = run_sidecar(&["--version"])?;
    if !out.status.success() {
        let stderr = String::from_utf8_lossy(&out.stderr);
        return Err(format!(
            "lettuce-volunteer --version failed: {}",
            stderr.trim()
        ));
    }
    let stdout = String::from_utf8_lossy(&out.stdout);
    let first_line = stdout.lines().next().unwrap_or("").trim();
    let version = first_line
        .rsplit(char::is_whitespace)
        .next()
        .unwrap_or(first_line)
        .trim()
        .to_string();
    if version.is_empty() {
        return Err("lettuce-volunteer --version printed nothing".into());
    }
    Ok(version)
}

/// Poll until `pid` has exited or `timeout` elapses; true when it exited.
fn wait_for_exit(pid: u32, timeout: Duration) -> bool {
    let start = Instant::now();
    while is_pid_alive(pid) {
        if start.elapsed() > timeout {
            return false;
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    true
}

/// Wait for a daemon.json that describes a live daemon other than `old_pid`.
fn wait_for_fresh_daemon(old_pid: Option<u32>, timeout: Duration) -> Result<DaemonInfo, String> {
    let start = Instant::now();
    loop {
        if let Ok(info) = read_daemon_json() {
            if Some(info.pid) != old_pid && is_pid_alive(info.pid) {
                return Ok(info);
            }
        }
        if start.elapsed() > timeout {
            return Err("Timed out waiting for the restarted daemon to start".into());
        }
        std::thread::sleep(Duration::from_millis(100));
    }
}

/// Restart the daemon: `lettuce-volunteer stop`, up to 30 s for the old process
/// to exit (then `stop --force`, which loses in-flight work), then `start` and
/// up to 30 s for a fresh daemon.json with a live PID. Needed after a config
/// change the running daemon cannot apply in place (for example runtime trust).
pub fn restart_daemon() -> Result<DaemonInfo, String> {
    let previous = read_daemon_json()
        .ok()
        .filter(|info| is_pid_alive(info.pid));

    if let Some(prev) = &previous {
        // `stop` locates the daemon through its own PID file; if that file is
        // missing it exits non-zero even though the process is alive. Do not
        // fail here — the wait below decides, and the forced path still works.
        if let Ok(out) = run_sidecar(&["stop"]) {
            if !out.status.success() {
                eprintln!(
                    "lettuce-volunteer stop: {}",
                    String::from_utf8_lossy(&out.stderr).trim()
                );
            }
        }

        if !wait_for_exit(prev.pid, Duration::from_secs(30)) {
            let forced = run_sidecar(&["stop", "--force"]);
            if !matches!(&forced, Ok(out) if out.status.success()) {
                force_kill(prev.pid)?;
            }
            if !wait_for_exit(prev.pid, Duration::from_secs(10)) {
                return Err(format!(
                    "Daemon (PID {}) did not exit after stop --force",
                    prev.pid
                ));
            }
        }

        // A daemon that was killed never removes its own daemon.json; drop the
        // stale file so only the new daemon's file can satisfy the wait below.
        if let Ok(stale) = read_daemon_json() {
            if stale.pid == prev.pid {
                let _ = fs::remove_file(daemon_json_path());
            }
        }
    }

    start_sidecar()?;
    wait_for_fresh_daemon(previous.map(|p| p.pid), Duration::from_secs(30))
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
pub fn is_pid_alive(pid: u32) -> bool {
    unsafe { libc::kill(pid as i32, 0) == 0 }
}

#[cfg(windows)]
pub fn is_pid_alive(pid: u32) -> bool {
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
