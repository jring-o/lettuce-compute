import { invoke } from "@tauri-apps/api/core";

interface DaemonConnection {
  port: number;
  token: string;
}

export interface ActiveTaskInfo {
  work_unit_id: string;
  leaf_name: string;
  progress_pct: number;
  elapsed_seconds: number;
  estimated_remaining_seconds?: number | null;
  work_dir: string;
  viz_bundle_path?: string | null;
  checkpoint_sequence?: number;
  last_checkpoint_at?: string;
  resumed_from_checkpoint?: boolean;
  cpu_seconds: number;
  task_status: "running" | "suspended_user" | "suspended_thermal" | "suspended_scheduled" | "error";
  status_reason: string | null;
  deadline_seconds: number;
  head_name: string;
  runtime_type: "native" | "container" | "wasm";
  process_id: number | null;
}

export interface StatusResponse {
  state: "active" | "paused" | "stopped";
  uptime_seconds: number;
  connected_servers: number;
  active_tasks: ActiveTaskInfo[];
  queued_tasks: QueuedTaskInfo[];
  paused_reason: string | null;
}

export interface QueuedTaskInfo {
  work_unit_id: string;
  leaf_name: string;
  deadline_seconds: number;
  fetched_at: string;
  server_name: string;
}

export interface MetricsResponse {
  cpu_usage_pct: number;
  gpu_usage_pct: number;
  memory_used_mb: number;
  memory_total_mb: number;
  disk_used_gb: number;
  disk_total_gb: number;
  cpu_temp_c: number;
  gpu_temp_c: number;
}

export interface AvailableLeaf {
  server_name: string;
  leaf_id: string;
  leaf_name: string;
  description?: string;
  research_area?: string;
}

export interface HistoryEntry {
  work_unit_id: string;
  leaf_name: string;
  completed_at: string;
  duration_seconds: number;
  credit_earned: number;
  validation_status: "accepted" | "rejected" | "pending";
  cpu_seconds: number;
  head_name: string;
}

export interface TaskDetail extends ActiveTaskInfo {
  memory_rss_mb: number | null;
  virtual_memory_mb: number | null;
  cpu_usage_pct: number | null;
  disk_read_mb: number | null;
  disk_written_mb: number | null;
  time_since_checkpoint_seconds: number | null;
  estimated_completion_at: string | null;
  progress_rate_pct_per_hour: number | null;
  fraction_done: number;
  container_image: string | null;
}

export interface HistoryResponse {
  entries: HistoryEntry[];
  pagination: {
    next_cursor: string;
    has_more: boolean;
  };
}

export interface ConfigResponse {
  data_dir: string;
  public_key?: string;
  resource_limits: {
    max_cpu_cores: number;
    max_memory_mb: number;
    max_disk_gb: number;
    max_bandwidth_mbps: number;
    max_gpu_vram_pct: number;
  };
  scheduling: {
    mode: string;
    idle_threshold_mins: number;
    cron_expression: string;
    schedule_ranges?: ScheduleRange[];
  };
  leafs: {
    mode: string;
    leaf_ids: string[];
    blocked_ids: string[];
  };
  thermal: {
    enabled: boolean;
    cpu_pause_threshold: number;
    cpu_resume_threshold: number;
    gpu_pause_threshold: number;
    gpu_resume_threshold: number;
    poll_interval_seconds: number;
  };
  notifications: {
    credit_milestones: boolean;
    credit_milestone_threshold: number;
    work_unit_completed: boolean;
    errors: boolean;
    updates: boolean;
  };
  servers: Array<{
    grpc_address: string;
    http_address: string;
    leaf_id: string;
    name: string;
    insecure: boolean;
    weight: number;
    leaf_preferences: LeafPreferences;
  }>;
  log_level: string;
  max_concurrent_tasks: number;
  work_buffer_size: number;
  available_runtimes: string[];
}

export interface HeadInfo {
  name: string;
  description: string;
  url: string;
  grpc_address: string;
  status: "connected" | "disconnected";
  weight: number;
  volunteer_id?: string;
  leafs: LeafInfo[];
}

export interface ExecutionSpec {
  image?: string;
  binaries?: Record<string, string>;
  gpu_required?: boolean;
  gpu_type?: string;
  max_memory_mb?: number;
  max_disk_mb?: number;
  network_access?: boolean;
}

export interface LeafInfo {
  id: string;
  slug: string;
  name: string;
  description: string;
  research_area: string;
  task_pattern: string;
  state: string;
  queued_work_units: number;
  active_volunteers: number;
  enabled: boolean;
  effective_weight: number;
  execution_spec?: ExecutionSpec;
}

export interface LeafPreferences {
  mode: "ALL" | "SPECIFIC" | "BLOCKLIST";
  weights?: Record<string, number>;
  enabled?: string[];
  disabled?: string[];
}

export interface HeadPreview {
  name: string;
  description: string;
  leafs: Array<{ slug: string; name: string; research_area: string }>;
}

export interface CreditSummary {
  total_credit: number;
  today: number;
  this_week: number;
  this_month: number;
  by_leaf: Array<{
    leaf_id: string;
    leaf_name: string;
    credit: number;
  }>;
  by_head?: Array<{
    head_name: string;
    credit: number;
    leafs: Array<{
      leaf_slug: string;
      leaf_name: string;
      credit: number;
    }>;
  }>;
}

export interface AttachRequest {
  server_address: string;
  leaf_id?: string;
  name?: string;
}

export interface DetachRequest {
  server_name?: string;
  server_address?: string;
}

export interface SearchParams {
  query?: string;
  server_address?: string;
}

export interface ScheduleRange {
  days: number[];
  start_hour: number;
  end_hour: number;
}

export interface HistoryParams {
  cursor?: string;
  limit?: number;
  leaf_id?: string;
  from?: string;
  to?: string;
}

export interface ResultEntry {
  work_unit_id: string;
  leaf_name: string;
  leaf_slug: string;
  head_name: string;
  completed_at: string;
  viz_bundle_path: string;
  size_bytes: number;
}

export interface ResultsResponse {
  results: ResultEntry[];
}

export interface SignChallengeResponse {
  public_key: string;
  signature: string;
}

export interface ContainerRuntimeStatus {
  backend: "podman" | "docker" | "none";
  status:
    | "running"
    | "stopped"
    | "not_initialized"
    | "not_installed"
    | "starting"
    | "error";
  version: string;
  socket_path: string;
  machine_required: boolean;
  machine_name: string;
  machine_cpus: number;
  machine_memory_mb: number;
  machine_disk_gb: number;
  error: string | null;
}

export interface ContainerRuntimeSetupResponse {
  status: string;
  message: string;
}

export async function getContainerRuntimeStatus(): Promise<ContainerRuntimeStatus> {
  return invoke("get_container_runtime_status");
}

export async function setupContainerRuntime(
  cpus?: number,
  memoryMb?: number,
  diskGb?: number
): Promise<ContainerRuntimeSetupResponse> {
  return invoke("setup_container_runtime", {
    cpus,
    memory_mb: memoryMb,
    disk_gb: diskGb,
  });
}

export async function startContainerRuntime(): Promise<ContainerRuntimeSetupResponse> {
  return invoke("start_container_runtime");
}

export async function stopContainerRuntime(): Promise<ContainerRuntimeSetupResponse> {
  return invoke("stop_container_runtime");
}

export interface PodmanPrerequisites {
  wsl_available: boolean;
  podman_installed: boolean;
  podman_path: string | null;
  needs_install: boolean;
}

export async function checkPodmanPrerequisites(): Promise<PodmanPrerequisites> {
  return invoke("check_podman_prerequisites");
}

export async function installPodman(
  cpus?: number,
  memoryMb?: number,
  diskGb?: number
): Promise<string> {
  return invoke("install_podman", {
    cpus,
    memory_mb: memoryMb,
    disk_gb: diskGb,
  });
}

/**
 * Total physical system memory in MB, read from the OS by the Rust backend.
 * Used by the setup wizard to size the memory slider to real hardware.
 * Returns 0 if detection fails (caller should fall back to a default).
 */
export async function getSystemMemoryMb(): Promise<number> {
  return invoke("get_system_memory_mb");
}

interface ApiErrorBody {
  error: {
    code: string;
    message: string;
  };
}

export class ApiError extends Error {
  code: string;

  constructor(code: string, message: string) {
    super(message);
    this.code = code;
    this.name = "ApiError";
  }
}

export class ManagementClient {
  private baseUrl: string;
  private token: string;

  private constructor(port: number, token: string) {
    this.baseUrl = `http://127.0.0.1:${port}`;
    this.token = token;
  }

  static async create(): Promise<ManagementClient> {
    const conn = await invoke<DaemonConnection>("get_daemon_info");
    return new ManagementClient(conn.port, conn.token);
  }

  private async request<T>(
    method: string,
    path: string,
    body?: unknown
  ): Promise<T> {
    const resp = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers: {
        Authorization: `Bearer ${this.token}`,
        "Content-Type": "application/json",
      },
      body: body ? JSON.stringify(body) : undefined,
    });

    if (!resp.ok) {
      let errBody: ApiErrorBody;
      try {
        errBody = await resp.json();
      } catch {
        throw new ApiError("UNKNOWN", `HTTP ${resp.status}: ${resp.statusText}`);
      }
      throw new ApiError(errBody.error.code, errBody.error.message);
    }

    if (resp.status === 204) {
      return undefined as T;
    }

    return resp.json();
  }

  async status(): Promise<StatusResponse> {
    return this.request("GET", "/api/v1/status");
  }

  async pause(): Promise<void> {
    return this.request("POST", "/api/v1/daemon/pause");
  }

  async resume(): Promise<void> {
    return this.request("POST", "/api/v1/daemon/resume");
  }

  async metrics(): Promise<MetricsResponse> {
    return this.request("GET", "/api/v1/metrics");
  }

  async heads(): Promise<HeadInfo[]> {
    const resp = await this.request<{ heads: HeadInfo[] }>("GET", "/api/v1/heads");
    return resp.heads;
  }

  async attachHead(req: AttachRequest): Promise<void> {
    return this.request("POST", "/api/v1/leafs/attach", req);
  }

  async detachHead(req: DetachRequest): Promise<void> {
    return this.request("POST", "/api/v1/leafs/detach", req);
  }

  async availableLeafs(params?: SearchParams): Promise<AvailableLeaf[]> {
    const query = new URLSearchParams();
    if (params?.query) query.set("search", params.query);
    if (params?.server_address)
      query.set("server_address", params.server_address);
    const qs = query.toString();
    const resp = await this.request<{ leafs: AvailableLeaf[] }>(
      "GET",
      `/api/v1/leafs/available${qs ? `?${qs}` : ""}`
    );
    return resp.leafs;
  }

  async history(params?: HistoryParams): Promise<HistoryResponse> {
    const query = new URLSearchParams();
    if (params?.cursor) query.set("cursor", params.cursor);
    if (params?.limit) query.set("limit", params.limit.toString());
    if (params?.leaf_id) query.set("leaf_id", params.leaf_id);
    if (params?.from) query.set("from", params.from);
    if (params?.to) query.set("to", params.to);
    const qs = query.toString();
    return this.request("GET", `/api/v1/history${qs ? `?${qs}` : ""}`);
  }

  async config(): Promise<ConfigResponse> {
    return this.request("GET", "/api/v1/config");
  }

  async updateConfig(partial: Partial<ConfigResponse>): Promise<ConfigResponse> {
    return this.request("PUT", "/api/v1/config", partial);
  }

  async credit(): Promise<CreditSummary> {
    return this.request("GET", "/api/v1/credit");
  }

  async signChallenge(challengeHex: string): Promise<SignChallengeResponse> {
    return this.request("POST", "/api/v1/identity/sign", {
      challenge_hex: challengeHex,
    });
  }

  async suspendTask(workUnitId: string): Promise<void> {
    return this.request("POST", `/api/v1/tasks/${workUnitId}/suspend`);
  }

  async resumeTask(workUnitId: string): Promise<void> {
    return this.request("POST", `/api/v1/tasks/${workUnitId}/resume`);
  }

  async abortTask(workUnitId: string): Promise<void> {
    return this.request("POST", `/api/v1/tasks/${workUnitId}/abort`);
  }

  async taskDetails(workUnitId: string): Promise<TaskDetail> {
    return this.request("GET", `/api/v1/tasks/${workUnitId}/details`);
  }

  async results(): Promise<ResultsResponse> {
    return this.request("GET", "/api/v1/results");
  }

  async resultData(workUnitId: string): Promise<Record<string, unknown>> {
    return this.request("GET", `/api/v1/results/${workUnitId}`);
  }
}
