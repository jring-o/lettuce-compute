use std::collections::VecDeque;
use std::ffi::{OsStr, OsString};
use std::fs;
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus, Output, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, Receiver};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde::Serialize;

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
    read_daemon_json_at(&daemon_json_path())
}

fn read_daemon_json_at(path: &Path) -> Result<DaemonInfo, String> {
    let contents =
        fs::read_to_string(path).map_err(|e| format!("Failed to read daemon.json: {}", e))?;
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
    #[allow(unused_mut)] // only Windows sets a creation flag
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

// ---------------------------------------------------------------------------
// The daemon this process spawned
// ---------------------------------------------------------------------------

/// How many of the daemon's most recent stderr lines are kept, so a start
/// failure can quote the daemon's own refusal.
const STDERR_TAIL_LINES: usize = 20;

/// Longest reason quoted back to the volunteer from the daemon's stderr.
const MAX_QUOTED_REASON: usize = 300;

/// How long to let the stderr reader catch up once the daemon has exited,
/// before the exit is reported. The refusal line is written a moment before
/// the process ends; the pipe stays open longer only when a grandchild
/// inherited it, and then the reader is simply not waited for.
const STDERR_DRAIN_GRACE: Duration = Duration::from_millis(500);

/// The daemon this process started, held for its whole life.
///
/// Two things depend on keeping the `Child` instead of dropping it. On macOS
/// and Linux a child that has exited stays a zombie until its parent waits on
/// it, and `kill(pid, 0)` succeeds for a zombie, so a host that dropped the
/// handle and judged liveness by signal 0 saw a cleanly stopped daemon as
/// alive for as long as it cared to wait — "did not exit after stop --force"
/// after 40 s, with the restart working on the second try because by then
/// there was no daemon.json to find (TB-53). Asking the child itself
/// (`try_wait`, which also reaps it) answers at once. And the daemon's stderr
/// is where its refusal to start is written (`Error: no servers configured`,
/// `could not connect to any configured server`); with stderr discarded every
/// early exit looked like a start that timed out (TB-52).
struct SpawnedDaemon {
    pid: u32,
    child: Child,
    /// The most recent stderr lines, newest last, trimmed.
    stderr_tail: Arc<Mutex<VecDeque<String>>>,
    /// Signalled when the stderr reader has seen the pipe close.
    stderr_done: Receiver<()>,
    /// The exit status once `try_wait` has reported it (the child is reaped
    /// at that point, so it is remembered rather than asked again).
    exit: Option<ExitStatus>,
}

impl SpawnedDaemon {
    /// The exit status if the daemon has exited, reaping it on first sight.
    fn poll(&mut self) -> Option<ExitStatus> {
        if self.exit.is_none() {
            if let Ok(Some(status)) = self.child.try_wait() {
                // Give the reader a moment to deliver the last lines, which
                // is where the daemon says why it stopped.
                let _ = self.stderr_done.recv_timeout(STDERR_DRAIN_GRACE);
                self.exit = Some(status);
            }
        }
        self.exit
    }

    fn tail(&self) -> Vec<String> {
        self.stderr_tail
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .iter()
            .cloned()
            .collect()
    }
}

/// The one daemon this process may have spawned. Replaced by the next spawn;
/// an adopted daemon (started by an earlier app session or by hand) is never
/// in here, and its liveness is judged by the operating system as before.
static SPAWNED: Mutex<Option<SpawnedDaemon>> = Mutex::new(None);

fn spawned() -> std::sync::MutexGuard<'static, Option<SpawnedDaemon>> {
    SPAWNED.lock().unwrap_or_else(|p| p.into_inner())
}

/// What this process knows about the daemon it spawned.
enum Spawned {
    /// Nothing spawned by this process.
    None,
    Running,
    Exited {
        status: ExitStatus,
        /// The daemon's own words, when its stderr had any (see `exit_reason`).
        reason: Option<String>,
    },
}

fn spawned_state() -> Spawned {
    let mut slot = spawned();
    match slot.as_mut() {
        None => Spawned::None,
        Some(d) => match d.poll() {
            None => Spawned::Running,
            Some(status) => Spawned::Exited {
                status,
                reason: exit_reason(&d.tail()),
            },
        },
    }
}

/// Whether the spawned daemon with this PID is alive, or `None` when `pid`
/// is not the daemon this process spawned.
fn spawned_alive(pid: u32) -> Option<bool> {
    let mut slot = spawned();
    let d = slot.as_mut()?;
    if d.pid != pid {
        return None;
    }
    Some(d.poll().is_none())
}

/// The reason to show for a daemon that exited, from its last stderr lines.
///
/// The CLI prints a refused start as `Error: <message>` (sometimes followed
/// by remedy lines) after its structured log has closed, so the last `Error:`
/// line and what follows it is the message. A Go crash starts with `panic:`.
/// Anything else on stderr is the daemon's JSON log, which is not a reason
/// and is not quoted.
fn exit_reason(tail: &[String]) -> Option<String> {
    let reason = if let Some(at) = tail.iter().rposition(|l| l.starts_with("Error:")) {
        let mut parts: Vec<&str> = vec![tail[at]["Error:".len()..].trim()];
        parts.extend(tail[at + 1..].iter().map(|l| l.trim()).filter(|l| !l.is_empty()));
        parts.join(" ")
    } else if let Some(line) = tail.iter().find(|l| l.starts_with("panic:")) {
        line.trim().to_string()
    } else {
        return None;
    };
    if reason.is_empty() {
        return None;
    }
    Some(reason.chars().take(MAX_QUOTED_REASON).collect())
}

/// Spawn `cmd` as the daemon and remember it, or report that the daemon
/// spawned earlier is still running. The check and the spawn happen under
/// one lock so two callers that both saw "no daemon.json yet" — the wizard's
/// `run_init` and the launch-time services — start one daemon between them.
fn spawn_daemon(mut cmd: Command) -> Result<SpawnOutcome, String> {
    let mut slot = spawned();
    if let Some(prev) = slot.as_mut() {
        if prev.poll().is_none() {
            return Ok(SpawnOutcome::AlreadyStarting(prev.pid));
        }
    }

    cmd.stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::piped());
    let mut child = cmd
        .spawn()
        .map_err(|e| format!("Failed to start sidecar: {}", e))?;

    let stderr_tail = Arc::new(Mutex::new(VecDeque::with_capacity(STDERR_TAIL_LINES)));
    let (done_tx, stderr_done) = mpsc::channel();
    if let Some(stderr) = child.stderr.take() {
        let tail = Arc::clone(&stderr_tail);
        std::thread::spawn(move || {
            for line in BufReader::new(stderr).lines().map_while(Result::ok) {
                let mut tail = tail.lock().unwrap_or_else(|p| p.into_inner());
                if tail.len() == STDERR_TAIL_LINES {
                    tail.pop_front();
                }
                tail.push_back(line.trim_end().to_string());
            }
            let _ = done_tx.send(());
        });
    }

    let pid = child.id();
    *slot = Some(SpawnedDaemon {
        pid,
        child,
        stderr_tail,
        stderr_done,
        exit: None,
    });
    Ok(SpawnOutcome::Spawned(pid))
}

enum SpawnOutcome {
    Spawned(u32),
    AlreadyStarting(u32),
}

/// Launch the daemon (`lettuce-volunteer start`) against the app's data
/// directory and return its PID without waiting; `wait_for_daemon` watches
/// for its `daemon.json` or its exit. Also succeeds, with that daemon's PID,
/// when this process already has a daemon starting.
pub fn start_sidecar() -> Result<u32, String> {
    if is_daemon_running() {
        return Err("Daemon is already running".into());
    }

    let binary = find_sidecar_binary()?;
    let mut cmd = sidecar_command(&binary);
    cmd.args(profile_args()).arg("start");
    Ok(match spawn_daemon(cmd)? {
        SpawnOutcome::Spawned(pid) | SpawnOutcome::AlreadyStarting(pid) => pid,
    })
}

/// Start the daemon unless one is running (its own or adopted) or this
/// process is already starting one.
pub fn ensure_daemon_started() -> Result<(), String> {
    if is_daemon_running() || matches!(spawned_state(), Spawned::Running) {
        return Ok(());
    }
    start_sidecar().map(|_| ())
}

/// The daemon process as the host sees it, for the status bar: `daemon.json`
/// is the daemon's own announcement, and the spawned child fills in the two
/// states that file cannot show — still starting, or already gone and why.
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "state", rename_all = "lowercase")]
pub enum DaemonProcessState {
    /// `daemon.json` names a live process.
    Running,
    /// The daemon this app started is alive but not yet listening.
    Starting,
    /// The daemon this app started has exited and nothing replaced it.
    Exited {
        /// Its own words, when it left any (`exit_reason`).
        reason: Option<String>,
        code: Option<i32>,
    },
    /// No daemon.json and nothing started by this app.
    Stopped,
}

pub fn daemon_process_state() -> DaemonProcessState {
    if is_daemon_running() {
        return DaemonProcessState::Running;
    }
    match spawned_state() {
        Spawned::Running => DaemonProcessState::Starting,
        Spawned::Exited { status, reason } => DaemonProcessState::Exited {
            reason,
            code: status.code(),
        },
        Spawned::None => DaemonProcessState::Stopped,
    }
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

/// What waiting for the daemon's management API ended with.
#[derive(Debug, Clone, Serialize)]
#[serde(tag = "state", rename_all = "lowercase")]
pub enum DaemonStart {
    /// `daemon.json` describes a live daemon.
    Ready(DaemonInfo),
    /// The deadline passed with the daemon this process spawned still
    /// running: it registers with every head and brings up the container
    /// engine before it listens, and a cold Podman machine can take minutes.
    /// The app connects once it is up.
    Starting,
}

/// Wait for a daemon.json that describes a live daemon other than `old_pid`,
/// or for the daemon this process spawned to give up. An exit is reported at
/// once, with the daemon's own reason when it printed one; the deadline only
/// ever ends the wait for a daemon that is still working on it.
fn wait_for_daemon_in(
    daemon_json: &Path,
    old_pid: Option<u32>,
    timeout: Duration,
) -> Result<DaemonStart, String> {
    let start = Instant::now();
    loop {
        if let Ok(info) = read_daemon_json_at(daemon_json) {
            if Some(info.pid) != old_pid && is_pid_alive(info.pid) {
                return Ok(DaemonStart::Ready(info));
            }
        }
        let expired = start.elapsed() > timeout;
        match spawned_state() {
            Spawned::Exited { status, reason } => return Err(start_failure(status, reason)),
            Spawned::Running if expired => return Ok(DaemonStart::Starting),
            Spawned::None if expired => {
                return Err("Timed out waiting for daemon to start".into());
            }
            _ => {}
        }
        std::thread::sleep(Duration::from_millis(100));
    }
}

/// The message for a daemon that exited instead of listening.
fn start_failure(status: ExitStatus, reason: Option<String>) -> String {
    match reason {
        Some(reason) => format!("Lettuce could not start: {reason}"),
        None => {
            let how = match status.code() {
                Some(code) => format!("exit code {code}"),
                None => "stopped by a signal".to_string(),
            };
            format!(
                "Lettuce exited while starting ({how}) without saying why; its log is under {}",
                data_dir().join("logs").display()
            )
        }
    }
}

/// Restart the daemon: `lettuce-volunteer stop`, up to 30 s for the old process
/// to exit (then `stop --force`, which loses in-flight work), then `start` and
/// up to `DAEMON_START_TIMEOUT` for a fresh daemon.json with a live PID. Needed
/// after a config change the running daemon cannot apply in place (for
/// example runtime trust).
pub fn restart_daemon() -> Result<DaemonStart, String> {
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
    wait_for_daemon_in(
        &daemon_json_path(),
        previous.map(|p| p.pid),
        DAEMON_START_TIMEOUT,
    )
}

/// How long a fresh daemon may take to publish its management API before the
/// host stops waiting: starting a container engine and registering with each
/// head happen before the API listens, and a cold Podman machine alone can
/// take a minute on Windows and longer on an Intel Mac. A daemon still alive
/// at the deadline is reported as starting, not failed.
pub const DAEMON_START_TIMEOUT: Duration = Duration::from_secs(180);

pub fn wait_for_daemon(timeout: Duration) -> Result<DaemonStart, String> {
    wait_for_daemon_in(&daemon_json_path(), None, timeout)
}

/// Set once suspend-and-quit has been started, so the tray's Quit (which
/// runs it and then asks the app to exit) and the exit handler that catches
/// every other way out do not both do it.
static QUIT_STARTED: AtomicBool = AtomicBool::new(false);

/// Ask the daemon to suspend all compute, save PIDs, release Job Object, and exit.
/// Frozen processes survive as orphans for the next launch. Every way of
/// leaving the app must end here (TB-56); a second call is a no-op.
pub fn suspend_and_quit_sidecar() -> Result<(), String> {
    if QUIT_STARTED.swap(true, Ordering::SeqCst) {
        return Ok(());
    }

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

/// Whether `pid` is a live process. The daemon this process spawned is asked
/// directly (see `SpawnedDaemon`); any other PID is asked of the operating
/// system.
pub fn is_pid_alive(pid: u32) -> bool {
    match spawned_alive(pid) {
        Some(alive) => alive,
        None => pid_alive_in_os(pid),
    }
}

#[cfg(unix)]
fn pid_alive_in_os(pid: u32) -> bool {
    unsafe { libc::kill(pid as i32, 0) == 0 }
}

#[cfg(windows)]
fn pid_alive_in_os(pid: u32) -> bool {
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
    use super::*;
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

    fn lines(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn exit_reason_quotes_the_cli_error_and_its_remedy_lines() {
        let tail = lines(&[
            r#"{"level":"INFO","msg":"volunteer starting"}"#,
            "Error: loading identity: permission denied",
            "  Fix the ownership of /home/vol/.lettuce/volunteer.key",
            "",
        ]);
        assert_eq!(
            exit_reason(&tail).as_deref(),
            Some("loading identity: permission denied Fix the ownership of /home/vol/.lettuce/volunteer.key")
        );
    }

    #[test]
    fn exit_reason_prefers_a_crash_line_and_ignores_plain_log_lines() {
        let tail = lines(&[
            "panic: runtime error: index out of range",
            "goroutine 1 [running]:",
        ]);
        assert_eq!(
            exit_reason(&tail).as_deref(),
            Some("panic: runtime error: index out of range")
        );
        assert_eq!(exit_reason(&lines(&[r#"{"level":"INFO","msg":"volunteer stopped"}"#])), None);
        assert_eq!(exit_reason(&[]), None);
        assert_eq!(exit_reason(&lines(&["Error:"])), None);
    }

    /// Tests below share the process-wide `SPAWNED` slot, so they run one at
    /// a time.
    static SPAWN_TESTS: Mutex<()> = Mutex::new(());

    /// A stand-in for `lettuce-volunteer start`: a shell running `script`.
    fn fake_sidecar(script: &str) -> Command {
        let mut cmd = if cfg!(windows) {
            let mut c = Command::new("cmd");
            c.args(["/C", script]);
            c
        } else {
            let mut c = Command::new("sh");
            c.args(["-c", script]);
            c
        };
        cmd.stdin(Stdio::null());
        cmd
    }

    /// A script that sleeps for about five seconds.
    fn sleeping_script() -> &'static str {
        if cfg!(windows) {
            "ping -n 6 127.0.0.1 > nul"
        } else {
            "sleep 5"
        }
    }

    fn kill_spawned() {
        if let Some(d) = spawned().as_mut() {
            let _ = d.child.kill();
            let _ = d.child.wait();
            d.exit = None;
        }
        *spawned() = None;
    }

    /// A daemon.json path that does not exist.
    fn no_daemon_json() -> PathBuf {
        std::env::temp_dir().join(format!("lettuce-sidecar-test-{}-daemon.json", std::process::id()))
    }

    // TB-53: a daemon this process spawned and that has since exited must be
    // judged dead at once. Before the fix the host dropped the Child, so on
    // macOS and Linux the exited daemon was a zombie and `kill(pid, 0)` kept
    // calling it alive; Windows opens a fresh handle and was never affected.
    #[test]
    fn spawned_daemon_that_exited_is_judged_dead() {
        let _guard = SPAWN_TESTS.lock().unwrap_or_else(|p| p.into_inner());
        kill_spawned();
        let pid = match spawn_daemon(fake_sidecar("exit 0")).unwrap() {
            SpawnOutcome::Spawned(pid) => pid,
            SpawnOutcome::AlreadyStarting(_) => panic!("nothing should be running"),
        };
        assert!(
            wait_for_exit(pid, Duration::from_secs(5)),
            "PID {pid} still judged alive 5 s after it exited"
        );
        kill_spawned();
    }

    // TB-52: a daemon that refuses to start says so on stderr and exits; the
    // wait must report those words, not a timeout minutes later.
    #[test]
    fn wait_for_daemon_reports_the_spawned_daemons_refusal() {
        let _guard = SPAWN_TESTS.lock().unwrap_or_else(|p| p.into_inner());
        kill_spawned();
        let script = if cfg!(windows) {
            "echo Error: no servers configured. Run attach first 1>&2 & exit 1"
        } else {
            "echo 'Error: no servers configured. Run attach first' >&2; exit 1"
        };
        spawn_daemon(fake_sidecar(script)).unwrap();
        let err = wait_for_daemon_in(&no_daemon_json(), None, Duration::from_secs(5)).unwrap_err();
        assert_eq!(
            err,
            "Lettuce could not start: no servers configured. Run attach first"
        );
        kill_spawned();
    }

    // TB-52: a daemon still alive at the deadline is starting, not failed —
    // a Podman machine on its first boot takes longer than any fixed budget.
    #[test]
    fn wait_for_daemon_reports_a_still_starting_daemon_at_the_deadline() {
        let _guard = SPAWN_TESTS.lock().unwrap_or_else(|p| p.into_inner());
        kill_spawned();
        spawn_daemon(fake_sidecar(sleeping_script())).unwrap();
        let outcome = wait_for_daemon_in(&no_daemon_json(), None, Duration::from_millis(300));
        assert!(matches!(outcome, Ok(DaemonStart::Starting)), "{outcome:?}");
        assert!(matches!(daemon_process_state(), DaemonProcessState::Starting));
        kill_spawned();
    }

    // Two callers that both saw no daemon.json must not start two daemons.
    #[test]
    fn a_second_spawn_while_the_first_runs_is_refused() {
        let _guard = SPAWN_TESTS.lock().unwrap_or_else(|p| p.into_inner());
        kill_spawned();
        let first = match spawn_daemon(fake_sidecar(sleeping_script())).unwrap() {
            SpawnOutcome::Spawned(pid) => pid,
            SpawnOutcome::AlreadyStarting(_) => panic!("nothing should be running"),
        };
        match spawn_daemon(fake_sidecar(sleeping_script())).unwrap() {
            SpawnOutcome::AlreadyStarting(pid) => assert_eq!(pid, first),
            SpawnOutcome::Spawned(_) => panic!("a second daemon was started"),
        }
        kill_spawned();
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
