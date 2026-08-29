use crate::api::{DaemonInfo, ManagementClient};
use serde::{Deserialize, Serialize};

#[derive(Debug, Serialize, Deserialize, Clone)]
pub struct ContainerRuntimeStatus {
    pub backend: String,
    pub status: String,
    pub version: String,
    pub socket_path: String,
    pub machine_required: bool,
    pub machine_name: String,
    pub machine_cpus: i32,
    pub machine_memory_mb: i32,
    pub machine_disk_gb: i32,
    pub error: Option<String>,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct SetupRequest {
    pub cpus: Option<i32>,
    pub memory_mb: Option<i32>,
    pub disk_gb: Option<i32>,
}

#[derive(Debug, Serialize, Deserialize, Clone)]
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
