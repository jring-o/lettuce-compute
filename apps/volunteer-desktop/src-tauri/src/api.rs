//! Transport to the volunteer daemon's local management API.
//!
//! The daemon (`lettuce-volunteer start`) serves a small HTTP API on a random
//! loopback port and advertises the port, a bearer token and its PID in
//! `~/.lettuce/daemon.json`. It accepts only requests whose Host header is the
//! loopback address it is bound to and deliberately sets no CORS headers, so the
//! app's web view cannot call it directly: every call — from the web view via the
//! `mgmt_request` command, and from the Rust host itself (tray, notifications,
//! container runtime) — goes through `ManagementClient` in this process.

use std::sync::OnceLock;
use std::time::Duration;

use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::sidecar;

/// Contents of `~/.lettuce/daemon.json`, written by the daemon when it starts
/// and removed when it shuts down cleanly.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DaemonInfo {
    pub port: u16,
    pub token: String,
    pub pid: u32,
    #[serde(default)]
    pub started_at: String,
}

/// The part of `GET /api/v1/status` the Rust host reads (tray icon and menu,
/// notification polling). The web view gets the full response as raw JSON.
/// Every field defaults when absent so a daemon that adds or drops a field never
/// breaks the tray.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct StatusResponse {
    pub state: String,
    pub uptime_seconds: i64,
    pub connected_servers: i64,
    pub active_tasks: Vec<ActiveTaskInfo>,
    pub paused_reason: Option<String>,
}

/// One in-progress work unit, as far as the Rust host needs to know.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(default)]
pub struct ActiveTaskInfo {
    pub work_unit_id: String,
    pub leaf_name: String,
    pub progress_pct: i64,
    pub elapsed_seconds: i64,
    pub work_dir: String,
}

/// A failed management-API call, in the shape the web view receives when
/// `mgmt_request` rejects.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MgmtError {
    /// The daemon's own error code (`VALIDATION_ERROR`, `NOT_FOUND`, `CONFLICT`,
    /// `INTERNAL_ERROR`, ...), `UNKNOWN` when a non-2xx response carried no
    /// parseable `{error:{code,message}}` envelope, `INVALID_RESPONSE` when a
    /// 2xx body was not JSON, or `DAEMON_UNREACHABLE` when no request reached the
    /// daemon at all (no daemon.json, connection refused, timeout).
    pub code: String,
    pub message: String,
    /// HTTP status of the daemon's response; 0 when there was no response.
    pub status: u16,
}

impl MgmtError {
    pub fn unreachable(message: impl Into<String>) -> Self {
        Self {
            code: "DAEMON_UNREACHABLE".into(),
            message: message.into(),
            status: 0,
        }
    }

    fn with_status(code: &str, message: impl Into<String>, status: u16) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            status,
        }
    }
}

impl std::fmt::Display for MgmtError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for MgmtError {}

/// The daemon's error envelope: `{"error":{"code":"...","message":"..."}}`.
#[derive(Deserialize)]
struct ErrorEnvelope {
    error: ErrorDetail,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct ErrorDetail {
    code: String,
    message: String,
}

/// Default per-request timeout.
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(15);

/// Timeout for `GET /api/v1/credit`, which asks every attached head in turn
/// (up to 5 s each) before answering.
pub const CREDIT_TIMEOUT: Duration = Duration::from_secs(30);

/// The timeout budget for a given API path.
pub fn timeout_for(path: &str) -> Duration {
    if path.starts_with("/api/v1/credit") {
        CREDIT_TIMEOUT
    } else {
        DEFAULT_TIMEOUT
    }
}

/// Longest slice of a non-JSON error body quoted back in an error message.
const MAX_QUOTED_BODY: usize = 200;

/// Map a non-2xx response to a structured error, preferring the daemon's own
/// code and message and falling back to `UNKNOWN` with the raw body.
fn parse_error(status: u16, body: &[u8]) -> MgmtError {
    if let Ok(env) = serde_json::from_slice::<ErrorEnvelope>(body) {
        if !env.error.code.is_empty() || !env.error.message.is_empty() {
            let code = if env.error.code.is_empty() {
                "UNKNOWN".to_string()
            } else {
                env.error.code
            };
            return MgmtError {
                code,
                message: env.error.message,
                status,
            };
        }
    }
    let text = String::from_utf8_lossy(body);
    let text = text.trim();
    let message = if text.is_empty() {
        format!("HTTP {status}")
    } else {
        let quoted: String = text.chars().take(MAX_QUOTED_BODY).collect();
        format!("HTTP {status}: {quoted}")
    };
    MgmtError::with_status("UNKNOWN", message, status)
}

fn shared_http() -> &'static reqwest::Client {
    static HTTP: OnceLock<reqwest::Client> = OnceLock::new();
    HTTP.get_or_init(reqwest::Client::new)
}

/// Client for one daemon instance (port + token from daemon.json).
pub struct ManagementClient {
    port: u16,
    token: String,
    http: &'static reqwest::Client,
}

impl ManagementClient {
    pub fn new(port: u16, token: String) -> Self {
        Self {
            port,
            token,
            http: shared_http(),
        }
    }

    pub fn from_daemon_info(info: &DaemonInfo) -> Self {
        Self::new(info.port, info.token.clone())
    }

    /// Locate the running daemon through `~/.lettuce/daemon.json`. The file is
    /// read on every call so a restarted daemon (new port, new token) is picked
    /// up without any client state.
    pub fn discover() -> Result<Self, MgmtError> {
        let info = sidecar::read_daemon_json().map_err(MgmtError::unreachable)?;
        Ok(Self::from_daemon_info(&info))
    }

    fn url(&self, path: &str) -> String {
        // reqwest sends Host as `127.0.0.1:<port>`, which is on the daemon's
        // Host allow-list; do not override it.
        format!("http://127.0.0.1:{}{}", self.port, path)
    }

    /// Send one request and return the parsed JSON body. A 204 or empty body
    /// yields `Value::Null`; any non-2xx status becomes an `MgmtError`.
    pub async fn request_value(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<&Value>,
        timeout: Duration,
    ) -> Result<Value, MgmtError> {
        let mut req = self
            .http
            .request(method, self.url(path))
            .bearer_auth(&self.token)
            .timeout(timeout);
        if let Some(b) = body {
            req = req.json(b);
        }

        let resp = req
            .send()
            .await
            .map_err(|e| MgmtError::unreachable(format!("management API request failed: {e}")))?;
        let status = resp.status();
        let bytes = resp.bytes().await.map_err(|e| {
            MgmtError::with_status(
                "UNKNOWN",
                format!("reading management API response: {e}"),
                status.as_u16(),
            )
        })?;

        if !status.is_success() {
            return Err(parse_error(status.as_u16(), &bytes));
        }
        if status == reqwest::StatusCode::NO_CONTENT || bytes.is_empty() {
            return Ok(Value::Null);
        }
        serde_json::from_slice(&bytes).map_err(|e| {
            MgmtError::with_status(
                "INVALID_RESPONSE",
                format!("management API returned malformed JSON: {e}"),
                status.as_u16(),
            )
        })
    }

    async fn get<T: DeserializeOwned>(&self, path: &str) -> Result<T, String> {
        let value = self
            .request_value(reqwest::Method::GET, path, None, timeout_for(path))
            .await
            .map_err(|e| e.to_string())?;
        serde_json::from_value(value).map_err(|e| format!("Failed to parse response: {}", e))
    }

    async fn post(&self, path: &str) -> Result<(), String> {
        self.request_value(reqwest::Method::POST, path, None, timeout_for(path))
            .await
            .map(|_| ())
            .map_err(|e| e.to_string())
    }

    pub async fn status(&self) -> Result<StatusResponse, String> {
        self.get("/api/v1/status").await
    }

    pub async fn pause(&self) -> Result<(), String> {
        self.post("/api/v1/daemon/pause").await
    }

    pub async fn resume(&self) -> Result<(), String> {
        self.post("/api/v1/daemon/resume").await
    }

    pub async fn suspend_and_quit(&self) -> Result<(), String> {
        self.post("/api/v1/daemon/suspend-and-quit").await
    }

    pub async fn get_json<T: DeserializeOwned>(&self, path: &str) -> Result<T, String> {
        self.get(path).await
    }

    pub async fn post_with_body<B: Serialize, T: DeserializeOwned>(
        &self,
        path: &str,
        body: Option<B>,
    ) -> Result<T, String> {
        let body = body
            .map(|b| serde_json::to_value(b))
            .transpose()
            .map_err(|e| format!("Failed to encode request body: {}", e))?;
        let value = self
            .request_value(reqwest::Method::POST, path, body.as_ref(), timeout_for(path))
            .await
            .map_err(|e| e.to_string())?;
        serde_json::from_value(value).map_err(|e| format!("Failed to parse response: {}", e))
    }

    pub async fn regenerate_keypair(&self) -> Result<String, String> {
        let resp: Value = self
            .post_with_body::<(), Value>("/api/v1/identity/regenerate", None)
            .await?;
        resp.get("public_key")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string())
            .ok_or_else(|| "Missing public_key in response".to_string())
    }
}

/// The web view's single entry point to the management API.
///
/// `method` is an HTTP method name, `path` an absolute API path such as
/// `/api/v1/status?x=1`, and `body` an optional JSON value sent as the request
/// body. The daemon is rediscovered from daemon.json on every call. Returns the
/// parsed JSON response (`null` for 204); rejects with an `MgmtError`.
#[tauri::command]
pub async fn mgmt_request(
    method: String,
    path: String,
    body: Option<Value>,
) -> Result<Value, MgmtError> {
    let method = reqwest::Method::from_bytes(method.trim().to_ascii_uppercase().as_bytes())
        .map_err(|_| {
            MgmtError::with_status("VALIDATION_ERROR", format!("unsupported HTTP method {method:?}"), 0)
        })?;
    if !path.starts_with("/api/") {
        return Err(MgmtError::with_status(
            "VALIDATION_ERROR",
            format!("management API path must start with /api/, got {path:?}"),
            0,
        ));
    }
    let client = ManagementClient::discover()?;
    client
        .request_value(method, &path, body.as_ref(), timeout_for(&path))
        .await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_error_uses_daemon_envelope() {
        let err = parse_error(404, br#"{"error":{"code":"NOT_FOUND","message":"Task not found"}}"#);
        assert_eq!(err.code, "NOT_FOUND");
        assert_eq!(err.message, "Task not found");
        assert_eq!(err.status, 404);
    }

    #[test]
    fn parse_error_falls_back_to_unknown() {
        let err = parse_error(500, b"<html>boom</html>");
        assert_eq!(err.code, "UNKNOWN");
        assert_eq!(err.message, "HTTP 500: <html>boom</html>");
        assert_eq!(err.status, 500);

        let err = parse_error(502, b"");
        assert_eq!(err.code, "UNKNOWN");
        assert_eq!(err.message, "HTTP 502");
    }

    #[test]
    fn credit_gets_longer_timeout() {
        assert_eq!(timeout_for("/api/v1/credit"), CREDIT_TIMEOUT);
        assert_eq!(timeout_for("/api/v1/status"), DEFAULT_TIMEOUT);
    }
}
