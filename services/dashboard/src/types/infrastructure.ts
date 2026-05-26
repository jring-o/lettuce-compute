// Infrastructure API type definitions
// Matches the Go infrastructure REST API response shapes

// --- Enums ---

export type LeafState =
  | "DRAFT"
  | "CONFIGURING"
  | "ACTIVE"
  | "PAUSED"
  | "COMPLETED"
  | "ARCHIVED";

export type TaskPattern =
  | "PARAMETER_SWEEP"
  | "MAP_REDUCE"
  | "MONTE_CARLO"
  | "CUSTOM";

export type LeafVisibility = "PUBLIC" | "UNLISTED" | "PRIVATE";

export type WorkUnitState =
  | "PENDING"
  | "ASSIGNED"
  | "RUNNING"
  | "COMPLETED"
  | "VALIDATED"
  | "FAILED"
  | "CANCELLED"
  | "EXPIRED";

export type WorkUnitPriority = "LOW" | "NORMAL" | "HIGH" | "CRITICAL";

// --- JSONB Config Types ---

export type RuntimeType = "NATIVE" | "CONTAINER" | "WASM" | "SCRIPT";

export interface ExecutionConfig {
  runtime: RuntimeType;
  binaries?: Record<string, string>;
  image?: string | null;
  dockerfile?: string | null;
  language?: string | null;
  entry_point?: string | null;
  dependencies?: string | null;
  gpu_required: boolean;
  gpu_type: "ANY" | "NVIDIA" | "AMD";
  min_vram_gb: number;
  network_access: boolean;
  max_memory_mb: number;
  max_disk_mb: number;
  max_cpu_seconds: number;
  env_vars?: Record<string, string>;
}

export interface ValidationConfig {
  strategy: "QUORUM" | "SPOT_CHECK" | "NONE";
  quorum_size?: number;
  quorum_threshold?: number;
  spot_check_percentage?: number;
  tolerance?: number;
}

export interface FaultToleranceConfig {
  max_retries: number;
  retry_delay_seconds: number;
  timeout_multiplier: number;
  max_timeout_seconds: number;
  reassign_on_timeout: boolean;
}

export interface DataConfig {
  input_format?: string;
  output_format?: string;
  checkpoint_interval_seconds?: number;
  max_input_size_bytes?: number;
  max_output_size_bytes?: number;
  transfer_strategy?: string;
  external_base_url?: string;
  aggregation_format?: string;
  aggregation_config?: Record<string, unknown>;
  splitting_strategy?: string;
  splitting_config?: Record<string, unknown>;
  generation_mode?: string;
  lazy_threshold?: number;
  lazy_batch_size?: number;
}

export interface AggregationResult {
  status: "complete" | "partial" | "no_aggregation";
  format: "json" | "csv";
  result?: Record<string, unknown>;
  result_csv?: string;
  message?: string;
  work_units_aggregated: number;
  work_units_total: number;
  aggregated_at: string;
}

export interface CreditConfig {
  base_credit: number;
  time_multiplier: number;
  difficulty_multiplier: number;
  bonus_for_early_return: number;
  penalty_for_error: number;
}

export interface ResourceRequirements {
  min_cpu_cores?: number;
  // Memory requirement = the container limit (execution_config.max_memory_mb),
  // surfaced here in list summaries. There is no separate min_memory_mb.
  max_memory_mb?: number;
  min_disk_mb?: number;
  gpu_required?: boolean;
  gpu_type?: string;
  gpu_min_vram_mb?: number;
  os?: string[];
  arch?: string[];
}

// --- Core Types ---

export interface Leaf {
  id: string;
  name: string;
  slug: string;
  description: string;
  state: LeafState;
  task_pattern: TaskPattern;
  research_area: string | null;
  creator_id: string | null;
  execution_config: ExecutionConfig | null;
  validation_config: ValidationConfig | null;
  fault_tolerance_config: FaultToleranceConfig | null;
  data_config: DataConfig | null;
  credit_config: CreditConfig | null;
  resource_requirements: ResourceRequirements | null;
  is_ongoing: boolean;
  visibility: LeafVisibility;
  stats_cache_seconds: number;
  created_at: string;
  updated_at: string;
}

export interface LeafSummary {
  id: string;
  name: string;
  slug: string;
  description: string;
  research_area: string | null;
  state: LeafState;
  task_pattern: TaskPattern;
  resource_requirements: ResourceRequirements | null;
  runtime: RuntimeType;
  is_ongoing: boolean;
  visibility: LeafVisibility;
  stats_cache_seconds: number;
  active_volunteers: number;
  progress_pct: number | null;
  created_at: string;
}

export interface WorkUnit {
  id: string;
  leaf_id: string;
  batch_id: string | null;
  state: WorkUnitState;
  priority: WorkUnitPriority;
  parameters: Record<string, unknown> | null;
  input_data_url: string | null;
  result_data_url: string | null;
  assigned_to: string | null;
  assigned_at: string | null;
  started_at: string | null;
  completed_at: string | null;
  deadline: string | null;
  attempts: number;
  max_attempts: number;
  error_message: string | null;
  flagged_for_review: boolean;
  credit_awarded: number | null;
  created_at: string;
  updated_at: string;
}

export interface WorkUnitSummary {
  id: string;
  leaf_id: string;
  batch_id: string | null;
  state: WorkUnitState;
  priority: WorkUnitPriority;
  assigned_to: string | null;
  attempts: number;
  flagged_for_review: boolean;
  created_at: string;
  updated_at: string;
}

export interface LeafStats {
  id: string;
  leaf_id: string;
  snapshot_at: string;
  total_work_units: number;
  work_units_queued: number;
  work_units_assigned: number;
  work_units_running: number;
  work_units_completed: number;
  work_units_validated: number;
  work_units_failed: number;
  active_volunteers: number;
  total_credit_granted: number;
  avg_completion_seconds: number | null;
  agreement_rate: number | null;
  throughput_per_hour: number | null;
  created_at: string;
}

// --- API Request/Response Types ---

export interface Pagination {
  next_cursor: string | null;
  has_more: boolean;
}

export interface ApiError {
  error: {
    code: string;
    message: string;
    details?: Record<string, unknown>;
  };
}

export interface PaginatedResponse<T> {
  data: T[];
  pagination: Pagination;
}

export interface CreateLeafRequest {
  name: string;
  description: string;
  task_pattern: TaskPattern;
  research_area?: string;
  is_ongoing?: boolean;
  visibility?: LeafVisibility;
  creator_id?: string;
}

export interface UpdateLeafRequest {
  name?: string;
  description?: string;
  research_area?: string;
  execution_config?: ExecutionConfig;
  validation_config?: ValidationConfig;
  fault_tolerance_config?: FaultToleranceConfig;
  data_config?: DataConfig;
  credit_config?: CreditConfig;
  resource_requirements?: ResourceRequirements;
  is_ongoing?: boolean;
  visibility?: LeafVisibility;
  stats_cache_seconds?: number;
}

export interface ListLeafsParams {
  cursor?: string;
  limit?: number;
  state?: LeafState;
  creator_id?: string;
  research_area?: string;
  search?: string;
  sort?: "created_at" | "updated_at" | "name";
  order?: "asc" | "desc";
}

export interface ListWorkUnitsParams {
  cursor?: string;
  limit?: number;
  state?: WorkUnitState;
  batch_id?: string;
  priority?: WorkUnitPriority;
  flagged_for_review?: boolean;
}

export interface GenerateWorkUnitsRequest {
  batch_size?: number;
  parameter_space?: Record<string, unknown>;
}

export interface GenerateWorkUnitsResponse {
  batch_id: string;
  work_units_created: number;
  status: "complete" | "generating";
}

export interface StatsHistoryParams {
  from: string;
  to?: string;
  interval?: "raw" | "hourly" | "daily";
}

export interface HealthResponse {
  status: string;
  uptime_seconds: number;
  database: string;
}

export type ResultValidationStatus = "PENDING" | "AGREED" | "DISAGREED";

export interface ResultExecutionMetadata {
  wall_clock_seconds: number;
  cpu_seconds_user: number;
  cpu_seconds_system: number;
  cpu_cores_used: number;
  gpu_seconds: number;
  gpu_model?: string;
  gpu_vram_used_mb: number;
  peak_memory_mb: number;
  disk_read_mb: number;
  disk_write_mb: number;
  network_rx_mb: number;
  network_tx_mb: number;
}

export interface Result {
  id: string;
  work_unit_id: string;
  volunteer_id: string;
  output_data?: Record<string, unknown>;
  output_data_ref?: string;
  output_checksum: string;
  execution_metadata: ResultExecutionMetadata;
  validation_status: ResultValidationStatus;
  submitted_at: string;
  validated_at?: string;
  created_at: string;
  updated_at: string;
}

export interface ListResultsParams {
  cursor?: string;
  limit?: number;
  work_unit_id?: string;
  validation_status?: ResultValidationStatus;
  volunteer_id?: string;
}

