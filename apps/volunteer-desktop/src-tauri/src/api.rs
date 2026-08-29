use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DaemonInfo {
    pub port: u16,
    pub token: String,
    pub pid: u32,
    pub started_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StatusResponse {
    pub state: String,
    pub uptime_seconds: u64,
    pub connected_servers: u32,
    pub active_tasks: Vec<ActiveTaskInfo>,
    pub paused_reason: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ActiveTaskInfo {
    pub work_unit_id: String,
    pub leaf_name: String,
    pub progress_pct: u32,
    pub elapsed_seconds: u64,
    #[serde(default)]
    pub work_dir: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MetricsResponse {
    pub cpu_usage_pct: f64,
    pub gpu_usage_pct: f64,
    pub memory_used_mb: u64,
    pub memory_total_mb: u64,
    pub disk_used_gb: f64,
    pub disk_total_gb: f64,
    pub cpu_temp_c: i32,
    pub gpu_temp_c: i32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ApiError {
    pub error: ApiErrorDetail,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ApiErrorDetail {
    pub code: String,
    pub message: String,
}

pub struct ManagementClient {
    port: u16,
    token: String,
    http: reqwest::Client,
}

impl ManagementClient {
    pub fn new(port: u16, token: String) -> Self {
        Self {
            port,
            token,
            http: reqwest::Client::new(),
        }
    }

    pub fn from_daemon_info(info: &DaemonInfo) -> Self {
        Self::new(info.port, info.token.clone())
    }

    fn url(&self, path: &str) -> String {
        format!("http://127.0.0.1:{}{}", self.port, path)
    }

    async fn check_error(&self, resp: reqwest::Response) -> Result<reqwest::Response, String> {
        if !resp.status().is_success() {
            let err: ApiError = resp.json().await.unwrap_or(ApiError {
                error: ApiErrorDetail {
                    code: "UNKNOWN".into(),
                    message: "Unknown error".into(),
                },
            });
            return Err(format!("{}: {}", err.error.code, err.error.message));
        }
        Ok(resp)
    }

    async fn get<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, String> {
        let resp = self
            .http
            .get(self.url(path))
            .bearer_auth(&self.token)
            .send()
            .await
            .map_err(|e| format!("HTTP request failed: {}", e))?;

        let resp = self.check_error(resp).await?;

        resp.json()
            .await
            .map_err(|e| format!("Failed to parse response: {}", e))
    }

    async fn post(&self, path: &str) -> Result<(), String> {
        let resp = self
            .http
            .post(self.url(path))
            .bearer_auth(&self.token)
            .send()
            .await
            .map_err(|e| format!("HTTP request failed: {}", e))?;

        self.check_error(resp).await?;
        Ok(())
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

    pub async fn metrics(&self) -> Result<MetricsResponse, String> {
        self.get("/api/v1/metrics").await
    }

    pub async fn get_json<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, String> {
        self.get(path).await
    }

    pub async fn post_with_body<B: serde::Serialize, T: serde::de::DeserializeOwned>(
        &self,
        path: &str,
        body: Option<B>,
    ) -> Result<T, String> {
        let mut req = self.http.post(self.url(path)).bearer_auth(&self.token);

        if let Some(b) = body {
            req = req.json(&b);
        }

        let resp = req
            .send()
            .await
            .map_err(|e| format!("HTTP request failed: {}", e))?;

        let resp = self.check_error(resp).await?;

        resp.json()
            .await
            .map_err(|e| format!("Failed to parse response: {}", e))
    }

    pub async fn regenerate_keypair(&self) -> Result<String, String> {
        let resp: serde_json::Value = self
            .post_with_body::<(), serde_json::Value>("/api/v1/identity/regenerate", None)
            .await?;
        resp.get("public_key")
            .and_then(|v| v.as_str())
            .map(|s| s.to_string())
            .ok_or_else(|| "Missing public_key in response".to_string())
    }
}
