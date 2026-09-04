use crate::api::{DaemonInfo, ManagementClient};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::time::{Duration, Instant};

/// `GET /api/v1/container-runtime`. Every field defaults when absent; the
/// machine sizes are integers on the daemon side but are read as i64 so a
/// future change in width cannot fail deserialization.
#[derive(Debug, Serialize, Deserialize, Clone, Default)]
#[serde(default)]
pub struct ContainerRuntimeStatus {
    pub backend: String,
    pub status: String,
    pub version: String,
    pub socket_path: String,
    pub machine_required: bool,
    pub machine_name: String,
    pub machine_cpus: i64,
    pub machine_memory_mb: i64,
    pub machine_disk_gb: i64,
    pub error: Option<String>,
    /// The daemon keeps probing for an engine (no runtime yet, a head trusted
    /// for containers); false when absent, as on older daemons.
    pub redetecting: bool,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct SetupRequest {
    pub cpus: Option<i32>,
    pub memory_mb: Option<i32>,
    pub disk_gb: Option<i32>,
}

#[derive(Debug, Serialize, Deserialize, Clone, Default)]
#[serde(default)]
pub struct SetupResponse {
    pub status: String,
    pub message: String,
}

impl ManagementClient {
    pub async fn get_container_runtime_status(&self) -> Result<ContainerRuntimeStatus, String> {
        self.get_json("/api/v1/container-runtime").await
    }

    pub async fn setup_container_runtime(
        &self,
        req: Option<SetupRequest>,
    ) -> Result<SetupResponse, String> {
        self.post_with_body("/api/v1/container-runtime/setup", req)
            .await
    }

    pub async fn start_container_runtime(&self) -> Result<SetupResponse, String> {
        self.post_with_body::<(), SetupResponse>("/api/v1/container-runtime/start", None)
            .await
    }

    pub async fn stop_container_runtime(&self) -> Result<SetupResponse, String> {
        self.post_with_body::<(), SetupResponse>("/api/v1/container-runtime/stop", None)
            .await
    }

    /// `POST /api/v1/container-runtime/redetect`: probe for an engine now.
    pub async fn redetect_container_runtime(&self) -> Result<SetupResponse, String> {
        self.post_with_body::<(), SetupResponse>("/api/v1/container-runtime/redetect", None)
            .await
    }
}

pub fn ensure_podman_state(info: &DaemonInfo, desired_status: &str) {
    let action_status = if desired_status == "running" { "stopped" } else { "running" };
    let client = ManagementClient::from_daemon_info(info);
    let rt = match tokio::runtime::Runtime::new() {
        Ok(rt) => rt,
        Err(_) => return,
    };
    rt.block_on(async {
        let status = match client.get_container_runtime_status().await {
            Ok(s) => s,
            Err(_) => return,
        };
        if status.backend == "podman" && status.status == action_status {
            if desired_status == "running" {
                let _ = client.start_container_runtime().await;
            } else {
                let _ = client.stop_container_runtime().await;
            }
        }
    });
}

// ---------------------------------------------------------------------------
// Host-side detection (no daemon involved)
// ---------------------------------------------------------------------------
//
// The setup wizard runs before the daemon exists, so it cannot ask
// `GET /api/v1/container-runtime`. This probe answers the two questions the
// wizard needs on every platform: is a container engine installed here, and
// does it currently answer. It mirrors the CLI's own detection order
// (`internal/runtime/backend.go`: Podman first, then Docker) and the socket
// locations its Linux resolver probes (`backend_linux.go`).

/// Result of `detect_container_runtime`.
#[derive(Debug, Serialize, Deserialize, Clone, Default, PartialEq)]
#[serde(default)]
pub struct ContainerRuntimeDetection {
    /// `podman`, `docker` or `none`.
    pub backend: String,
    /// Engine version when known, e.g. `5.3.1`; empty otherwise.
    pub version: String,
    /// Where the engine binary was found; empty when `backend` is `none`.
    pub binary_path: String,
    /// The engine answers: a Podman machine is running (Windows/macOS), a
    /// Podman API socket is present (Linux), or the Docker server replied.
    pub responding: bool,
    /// Plain-language reason when `responding` is false; empty otherwise.
    pub detail: String,
}

impl ContainerRuntimeDetection {
    fn none() -> Self {
        Self {
            backend: "none".into(),
            ..Default::default()
        }
    }
}

/// How long one probe subprocess may run before it is killed. `podman
/// machine inspect` can stall for a long time when the WSL/VM layer is
/// unhealthy, and the wizard must not hang on it.
const PROBE_TIMEOUT: Duration = Duration::from_secs(15);

/// A subprocess `Command` that never opens a console window.
fn quiet_command(program: &Path) -> Command {
    let mut cmd = Command::new(program);
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x08000000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    cmd.stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    cmd
}

/// Run `cmd`, killing it after `timeout`. `None` when it could not be
/// spawned or did not finish in time. Output from these probes is a single
/// line, so the piped stdout cannot fill up before the process exits.
fn run_with_timeout(mut cmd: Command, timeout: Duration) -> Option<Output> {
    let mut child = cmd.spawn().ok()?;
    let start = Instant::now();
    loop {
        match child.try_wait() {
            Ok(Some(_)) => return child.wait_with_output().ok(),
            Ok(None) => {
                if start.elapsed() > timeout {
                    let _ = child.kill();
                    let _ = child.wait();
                    return None;
                }
                std::thread::sleep(Duration::from_millis(50));
            }
            Err(_) => return None,
        }
    }
}

/// Trimmed stdout of a successful run; `None` on failure, timeout or a
/// non-zero exit.
fn capture(program: &Path, args: &[&str]) -> Option<String> {
    let mut cmd = quiet_command(program);
    cmd.args(args);
    let out = run_with_timeout(cmd, PROBE_TIMEOUT)?;
    if !out.status.success() {
        return None;
    }
    Some(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

/// The version token out of `podman version 5.3.1` / `Docker version 24.0.7,
/// build afdd53b`: the first whitespace-separated token that starts with a
/// digit, with any trailing comma removed.
fn parse_version_line(line: &str) -> String {
    line.split_whitespace()
        .find(|tok| tok.chars().next().is_some_and(|c| c.is_ascii_digit()))
        .map(|tok| tok.trim_end_matches(',').to_string())
        .unwrap_or_default()
}

fn exe_name(base: &str) -> String {
    if cfg!(windows) {
        format!("{base}.exe")
    } else {
        base.to_string()
    }
}

/// Find `name` on PATH, then in `extra` install locations. GUI apps on macOS
/// launch with a minimal PATH that omits Homebrew and Podman Desktop, and on
/// Windows a per-user MSI install is not on the PATH of an already-running
/// process, so the fixed locations matter.
fn find_binary(name: &str, extra: &[PathBuf]) -> Option<PathBuf> {
    let file = exe_name(name);
    if let Some(path_var) = std::env::var_os("PATH") {
        for dir in std::env::split_paths(&path_var) {
            let candidate = dir.join(&file);
            if candidate.is_file() {
                return Some(candidate);
            }
        }
    }
    extra.iter().find(|p| p.is_file()).cloned()
}

fn podman_candidates() -> Vec<PathBuf> {
    let mut v = Vec::new();
    if cfg!(target_os = "windows") {
        if let Some(home) = dirs::home_dir() {
            v.push(
                home.join("AppData")
                    .join("Local")
                    .join("Programs")
                    .join("Podman")
                    .join("podman.exe"),
            );
        }
        v.push(PathBuf::from(r"C:\Program Files\RedHat\Podman\podman.exe"));
    } else if cfg!(target_os = "macos") {
        v.push(PathBuf::from("/opt/podman/bin/podman"));
        v.push(PathBuf::from("/opt/homebrew/bin/podman"));
        v.push(PathBuf::from("/usr/local/bin/podman"));
    } else {
        v.push(PathBuf::from("/usr/bin/podman"));
        v.push(PathBuf::from("/usr/local/bin/podman"));
        v.push(PathBuf::from("/bin/podman"));
    }
    v
}

fn docker_candidates() -> Vec<PathBuf> {
    let mut v = Vec::new();
    if cfg!(target_os = "windows") {
        v.push(PathBuf::from(
            r"C:\Program Files\Docker\Docker\resources\bin\docker.exe",
        ));
    } else if cfg!(target_os = "macos") {
        v.push(PathBuf::from("/usr/local/bin/docker"));
        v.push(PathBuf::from("/opt/homebrew/bin/docker"));
        v.push(PathBuf::from(
            "/Applications/Docker.app/Contents/Resources/bin/docker",
        ));
    } else {
        v.push(PathBuf::from("/usr/bin/docker"));
        v.push(PathBuf::from("/usr/local/bin/docker"));
        v.push(PathBuf::from("/bin/docker"));
        v.push(PathBuf::from("/snap/bin/docker"));
    }
    v
}

/// A Unix socket path from a `CONTAINER_HOST` / `DOCKER_HOST` value: accepts
/// `unix:///path` and bare absolute paths, like the CLI's `unixSocketPath`.
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
fn socket_from_env_value(value: &str) -> Option<PathBuf> {
    let v = value.trim();
    if let Some(rest) = v.strip_prefix("unix://") {
        return (!rest.is_empty()).then(|| PathBuf::from(rest));
    }
    v.starts_with('/').then(|| PathBuf::from(v))
}

/// The Podman API sockets the CLI probes on Linux, in its order: an explicit
/// `CONTAINER_HOST`/`DOCKER_HOST` override, the rootless user socket, then
/// the rootful system socket.
#[cfg(target_os = "linux")]
fn linux_podman_sockets() -> Vec<PathBuf> {
    let mut v = Vec::new();
    for key in ["CONTAINER_HOST", "DOCKER_HOST"] {
        if let Some(p) = std::env::var(key)
            .ok()
            .and_then(|s| socket_from_env_value(&s))
        {
            v.push(p);
        }
    }
    if let Ok(xdg) = std::env::var("XDG_RUNTIME_DIR") {
        if !xdg.is_empty() {
            v.push(PathBuf::from(xdg).join("podman").join("podman.sock"));
        }
    }
    // The running user's id, read from procfs so no C binding is needed.
    let uid = {
        use std::os::unix::fs::MetadataExt;
        std::fs::metadata("/proc/self").map(|m| m.uid()).unwrap_or(1000)
    };
    v.push(PathBuf::from(format!("/run/user/{uid}/podman/podman.sock")));
    v.push(PathBuf::from("/run/podman/podman.sock"));
    v
}

fn probe_podman(binary: &Path) -> ContainerRuntimeDetection {
    let version = capture(binary, &["--version"])
        .map(|line| parse_version_line(&line))
        .unwrap_or_default();
    let mut det = ContainerRuntimeDetection {
        backend: "podman".into(),
        version,
        binary_path: binary.to_string_lossy().into_owned(),
        responding: false,
        detail: String::new(),
    };

    #[cfg(target_os = "linux")]
    {
        // Rootless Podman needs no VM, only its API socket service; that is
        // what the daemon connects to.
        if linux_podman_sockets().iter().any(|p| p.exists()) {
            det.responding = true;
        } else {
            det.detail = "Podman is installed but its API socket is not running.".into();
        }
    }

    #[cfg(not(target_os = "linux"))]
    {
        // Windows and macOS run containers inside a Podman machine (a small
        // Linux VM); nothing works until it exists and is running.
        match capture(binary, &["machine", "inspect", "--format", "{{.State}}"]) {
            Some(state) if state.eq_ignore_ascii_case("running") => det.responding = true,
            Some(state) => {
                let shown = if state.is_empty() {
                    "not running".to_string()
                } else {
                    state.to_lowercase()
                };
                det.detail = format!("Podman is installed but its machine is {shown}.");
            }
            None => {
                det.detail =
                    "Podman is installed but no Podman machine has been created.".into();
            }
        }
    }

    det
}

fn probe_docker(binary: &Path) -> ContainerRuntimeDetection {
    let mut det = ContainerRuntimeDetection {
        backend: "docker".into(),
        version: String::new(),
        binary_path: binary.to_string_lossy().into_owned(),
        responding: false,
        detail: String::new(),
    };
    // `docker version` talks to the server; it fails when the engine is down.
    match capture(binary, &["version", "--format", "{{.Server.Version}}"]) {
        Some(server) if !server.is_empty() => {
            det.responding = true;
            det.version = server;
        }
        _ => {
            det.version = capture(binary, &["--version"])
                .map(|line| parse_version_line(&line))
                .unwrap_or_default();
            det.detail = "Docker is installed but its engine is not running.".into();
        }
    }
    det
}

/// Probe this machine for a container engine. Prefers an engine that
/// answers; between two that answer, or two that do not, Podman comes first
/// (the CLI's order).
pub fn detect() -> ContainerRuntimeDetection {
    let podman = find_binary("podman", &podman_candidates()).map(|p| probe_podman(&p));
    let docker = find_binary("docker", &docker_candidates()).map(|p| probe_docker(&p));
    pick_detection(podman, docker)
}

fn pick_detection(
    podman: Option<ContainerRuntimeDetection>,
    docker: Option<ContainerRuntimeDetection>,
) -> ContainerRuntimeDetection {
    match (podman, docker) {
        (Some(p), _) if p.responding => p,
        (_, Some(d)) if d.responding => d,
        (Some(p), _) => p,
        (None, Some(d)) => d,
        (None, None) => ContainerRuntimeDetection::none(),
    }
}

#[cfg(test)]
mod detection_tests {
    use super::*;

    fn det(backend: &str, responding: bool) -> ContainerRuntimeDetection {
        ContainerRuntimeDetection {
            backend: backend.into(),
            responding,
            ..Default::default()
        }
    }

    #[test]
    fn parses_version_lines() {
        assert_eq!(parse_version_line("podman version 5.3.1"), "5.3.1");
        assert_eq!(
            parse_version_line("Docker version 24.0.7, build afdd53b"),
            "24.0.7"
        );
        assert_eq!(parse_version_line("nonsense"), "");
    }

    #[test]
    fn socket_env_values() {
        assert_eq!(
            socket_from_env_value("unix:///run/podman/podman.sock"),
            Some(PathBuf::from("/run/podman/podman.sock"))
        );
        assert_eq!(
            socket_from_env_value("/var/run/docker.sock"),
            Some(PathBuf::from("/var/run/docker.sock"))
        );
        assert_eq!(socket_from_env_value("tcp://127.0.0.1:2375"), None);
        assert_eq!(socket_from_env_value("unix://"), None);
    }

    #[test]
    fn prefers_a_responding_engine_then_podman() {
        assert_eq!(
            pick_detection(Some(det("podman", false)), Some(det("docker", true))).backend,
            "docker"
        );
        assert_eq!(
            pick_detection(Some(det("podman", true)), Some(det("docker", true))).backend,
            "podman"
        );
        assert_eq!(
            pick_detection(Some(det("podman", false)), Some(det("docker", false))).backend,
            "podman"
        );
        assert_eq!(
            pick_detection(None, Some(det("docker", false))).backend,
            "docker"
        );
        let none = pick_detection(None, None);
        assert_eq!(none.backend, "none");
        assert!(!none.responding);
    }
}
