// Browser volunteer types matching the infrastructure REST API.

export interface HardwareCapabilities {
  cpu_cores: number;
  memory_mb: number;
  has_gpu: boolean;
  gpu_vendors: string[];
  available_runtimes: string[];
}

export interface RegisterResponse {
  volunteer_id: string;
  registered_at: string;
}

export interface RequestWorkOptions {
  leaf_ids?: string[];
  max_memory_mb?: number;
  max_disk_mb?: number;
  has_gpu?: boolean;
  gpu_vendors?: string[];
}

export interface ExecutionSpec {
  binaries: Record<string, string>;
  gpu_required: boolean;
  gpu_type?: string;
  max_memory_mb: number;
  max_disk_mb: number;
  network_access: boolean;
}

export interface WorkUnitResponse {
  work_unit_id: string;
  leaf_id: string;
  runtime: string;
  input_data?: string;
  input_data_url?: string;
  code_artifact_url?: string;
  parameters_json?: string;
  deadline_seconds: number;
  heartbeat_interval_seconds: number;
  env_vars?: Record<string, string>;
  execution_spec: ExecutionSpec;
  rsc_fpops_est?: number;
}

export interface SubmitResultRequest {
  work_unit_id: string;
  output_data: string;
  output_checksum: string;
  exit_code: number;
  metrics: ExecutionMetrics;
}

export interface SubmitResultResponse {
  accepted: boolean;
  validation_status: string;
}

export interface HeartbeatRequest {
  work_unit_id: string;
  progress_pct: number;
  metrics: Partial<ExecutionMetrics>;
}

export interface HeartbeatResponse {
  continue_execution: boolean;
}

export interface ExecutionMetrics {
  wall_clock_seconds: number;
  cpu_seconds_user: number;
  peak_memory_mb: number;
}

// Worker <-> Main thread message types (discriminated union).

export type MainToWorkerMessage =
  | { type: "execute"; workUnit: WorkUnitResponse; gpuEnabled: boolean }
  | { type: "abort" };

export type WorkerToMainMessage =
  | {
      type: "result";
      output: ArrayBuffer;
      checksum: string;
      exitCode: number;
      metrics: ExecutionMetrics;
    }
  | {
      type: "progress";
      progressPct: number;
      metrics: Partial<ExecutionMetrics>;
    }
  | { type: "error"; message: string; fatal: boolean }
  | { type: "ready" };
