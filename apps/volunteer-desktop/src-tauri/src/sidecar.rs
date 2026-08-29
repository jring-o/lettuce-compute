use std::ffi::{OsStr, OsString};
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Output, Stdio};
use std::time::{Duration, Instant};

use crate::api::DaemonInfo;

/// Environment variable that relocates the data directory. When set to a
/// non-empty path, the app reads and writes everything under that path
/// instead of `~/.lettuce` (`daemon.json`, `config.yaml`, its own
/// `milestones.json` and first-launch marker), and passes `--data-dir <path>`
/// to every `lettuce-volunteer` command it runs, so the bundled client keeps
/// the whole profile — config, identity keys, work directories, logs — in the
/// same place. Two copies of the app with different values run as two
/// independent volunteers on one machine; it is also how tests point a build
/// at a throwaway profile.
pub const DATA_DIR_ENV: &str = "LETTUCE_DATA_DIR";

/// The directory the app and its bundled client keep their state in: the
/// `LETTUCE_DATA_DIR` override when set, else `~/.lettuce`. Always absolute.
pub fn data_dir() -> PathBuf {
    resolve_data_dir(std::env::var_os(DATA_DIR_ENV), dirs::home_dir())
}

/// `data_dir()` for a given override value and home directory. A missing,
/// empty or whitespace-only override means the default. A relative override
/// is made absolute against the current directory once, here, so the app and
/// the client (which absolutizes `--data-dir` against its own working
/// directory) agree on the same folder.
fn resolve_data_dir(override_value: Option<OsString>, home: Option<PathBuf>) -> PathBuf {
    if let Some(dir) = override_path(override_value) {
        return std::path::absolute(&dir).unwrap_or(dir);
    }
    home.expect("Could not determine home directory")
        .join(".lettuce")
}

/// The override as a path, or `None` when unset, empty or whitespace-only.
fn override_path(override_value: Option<OsString>) -> Option<PathBuf> {
    let raw = override_value?;
    let value = match raw.to_str() {
        Some(s) => PathBuf::from(s.trim()),
        None => PathBuf::from(raw),
    };
    (!value.as_os_str().is_empty()).then_some(value)
}

/// Arguments that make a `lettuce-volunteer` command use the app's data
/// directory: `--data-dir <dir>` under the override, nothing otherwise (the
/// client's own default is the same `~/.lettuce`).
fn profile_args() -> Vec<OsString> {
    match override_path(std::env::var_os(DATA_DIR_ENV)) {
        Some(_) => vec![OsString::from("--data-dir"), data_dir().into_os_string()],
        None => Vec::new(),
    }
}

fn daemon_json_path() -> PathBuf {
    data_dir().join("daemon.json")
}

fn config_yaml_path() -> PathBuf {
    data_dir().join("config.yaml")
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

/// Run the sidecar with the given arguments against the app's data directory
/// and wait for it to exit, capturing its output. Used for the CLI's one-shot
/// subcommands (`init`, `schedule set`, `stop`); the long-running daemon is
/// launched with `start_sidecar`. Under `LETTUCE_DATA_DIR` the command gets
/// `--data-dir` first, before the subcommand, where the CLI accepts its
/// persistent flags.
pub fn run_sidecar<S: AsRef<OsStr>>(args: &[S]) -> Result<Output, String> {
    let mut full: Vec<OsString> = profile_args();
    full.extend(args.iter().map(|a| a.as_ref().to_os_string()));
    run_sidecar_bare(&full)
}

/// Run the sidecar exactly as given, with no data-directory argument. Only
/// for commands that do not touch the profile (`--version`).
fn run_sidecar_bare<S: AsRef<OsStr>>(args: &[S]) -> Result<Output, String> {
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

/// Launch the daemon (`lettuce-volunteer start`) against the app's data
/// directory and return without waiting; `wait_for_daemon` watches for its
/// `daemon.json`.
pub fn start_sidecar() -> Result<Child, String> {
    if is_daemon_running() {
        return Err("Daemon is already running".into());
    }

    let binary = find_sidecar_binary()?;

    let mut cmd = sidecar_command(&binary);
    cmd.args(profile_args())
        .arg("start")
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
/// Reads no profile, so no `--data-dir` is passed.
pub fn client_version() -> Result<String, String> {
    let out = run_sidecar_bare(&["--version"])?;
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
    wait_for_fresh_daemon(previous.map(|p| p.pid), DAEMON_START_TIMEOUT)
}

/// How long a fresh daemon may take to publish its management API before the
/// host gives up: starting a container engine and registering with each head
/// happen before the API listens, and a cold Podman machine alone can take a
/// minute on Windows.
pub const DAEMON_START_TIMEOUT: Duration = Duration::from_secs(180);

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

#[cfg(test)]
mod tests {
    use super::resolve_data_dir;
    use std::ffi::OsString;
    use std::path::PathBuf;

    fn home() -> Option<PathBuf> {
        Some(PathBuf::from(if cfg!(windows) { r"C:\Users\vol" } else { "/home/vol" }))
    }

    #[test]
    fn defaults_to_dot_lettuce_under_home() {
        assert_eq!(resolve_data_dir(None, home()), home().unwrap().join(".lettuce"));
    }

    #[test]
    fn empty_or_blank_override_means_the_default() {
        assert_eq!(
            resolve_data_dir(Some(OsString::from("")), home()),
            home().unwrap().join(".lettuce")
        );
        assert_eq!(
            resolve_data_dir(Some(OsString::from("  \t ")), home()),
            home().unwrap().join(".lettuce")
        );
    }

    #[test]
    fn absolute_override_is_used_as_given() {
        let dir = if cfg!(windows) { r"D:\profiles\second" } else { "/srv/profiles/second" };
        assert_eq!(
            resolve_data_dir(Some(OsString::from(format!("  {dir}  "))), home()),
            PathBuf::from(dir)
        );
    }

    #[test]
    fn relative_override_is_made_absolute() {
        let resolved = resolve_data_dir(Some(OsString::from("rel-profile")), home());
        assert!(resolved.is_absolute(), "{}", resolved.display());
        assert_eq!(
            resolved,
            std::env::current_dir().unwrap().join("rel-profile")
        );
    }

    #[test]
    fn override_wins_even_without_a_home_directory() {
        let dir = if cfg!(windows) { r"D:\p" } else { "/p" };
        assert_eq!(resolve_data_dir(Some(OsString::from(dir)), None), PathBuf::from(dir));
    }
}

#[cfg(windows)]
extern "system" {
    fn OpenProcess(access: u32, inherit: i32, pid: u32) -> *mut std::ffi::c_void;
    fn GetExitCodeProcess(handle: *mut std::ffi::c_void, exit_code: *mut u32) -> i32;
    fn CloseHandle(handle: *mut std::ffi::c_void) -> i32;
    fn TerminateProcess(handle: *mut std::ffi::c_void, exit_code: u32) -> i32;
}

/// Kill `pid` outright (SIGKILL). The last resort after `lettuce-volunteer
/// stop --force` and the management API's suspend-and-quit have both failed.
#[cfg(unix)]
fn force_kill(pid: u32) -> Result<(), String> {
    let result = unsafe { libc::kill(pid as i32, libc::SIGKILL) };
    if result != 0 {
        return Err(format!("Failed to send SIGKILL to PID {}", pid));
    }
    Ok(())
}

/// Kill `pid` outright. Windows has no graceful signal; `TerminateProcess`
/// is the only kill there is.
#[cfg(windows)]
fn force_kill(pid: u32) -> Result<(), String> {
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
