import { invoke } from "@tauri-apps/api/core";

/**
 * Client for the volunteer daemon's local management API.
 *
 * Every call is relayed through the Rust host process (the `mgmt_request`
 * Tauri command). The daemon accepts only loopback Host headers and sets no
 * CORS headers, so a browser `fetch` from this web view is refused at the
 * preflight, while the Rust process reaches it directly. The Rust side reads
 * `~/.lettuce/daemon.json` (port and bearer token) on every call, so nothing
 * here needs to know either, and a restarted daemon is picked up automatically.
 *
 * The types below mirror the daemon's response structs in
 * `services/volunteer-cli/internal/management/daemon_bridge.go` (routes in
 * `handlers.go`). A Go `omitempty` field is optional here; a Go pointer field
 * without `omitempty` is `T | null`. Where the daemon may send `null` for a
 * list, the client normalises it to `[]` so callers never branch on null.
 *
 * "`~/.lettuce`" below means the data directory, which `LETTUCE_DATA_DIR`
 * can relocate; the Rust host resolves it (`getDataDir()`).
 */

// ---------------------------------------------------------------------------
// GET /api/v1/status
// ---------------------------------------------------------------------------

export type DaemonState = "active" | "paused" | "stopped";

/**
 * Why the daemon is paused: "user" (pause button or CLI), "thermal" (CPU or
 * GPU over the configured temperature), "scheduled" (outside the configured
 * computing hours).
 */
export type PausedReason = "user" | "thermal" | "scheduled";

/**
 * "suspended" (without a suffix) is a task frozen while the daemon is paused
 * for a reason it did not report; the suffixed values name the cause.
 */
export type TaskStatus =
  | "running"
  | "suspended"
  | "suspended_user"
  | "suspended_thermal"
  | "suspended_scheduled";

export type RuntimeType = "native" | "container" | "wasm";

export interface ActiveTaskInfo {
  work_unit_id: string;
  leaf_name: string;
  progress_pct: number;
  /** Seconds actually run under a live daemon (excludes time the daemon was stopped). */
  elapsed_seconds: number;
  estimated_remaining_seconds?: number;
  work_dir: string;
  viz_bundle_path: string | null;
  checkpoint_sequence?: number;
  last_checkpoint_at?: string;
  resumed_from_checkpoint?: boolean;
  cpu_seconds: number;
  task_status: TaskStatus;
  status_reason: string | null;
  /** Seconds left before the work unit's deadline; 0 when the unit has none. */
  deadline_seconds: number;
  head_name: string;
  runtime_type: RuntimeType;
  process_id: number | null;
}

export interface QueuedTaskInfo {
  work_unit_id: string;
  leaf_name: string;
  deadline_seconds: number;
  fetched_at: string;
  server_name: string;
}

/**
 * A leaf whose work units have failed on this machine since the daemon
 * started. Also carried per leaf on `HeadInfo.leafs[].failures`.
 */
export interface FailingLeaf {
  leaf_id: string;
  leaf_name: string;
  consecutive_failures: number;
  total_failures: number;
  last_reason?: string;
  last_failed_at?: string;
  /** The per-leaf breaker has stopped requesting this leaf until `paused_until`. */
  paused: boolean;
  paused_until?: string;
}

export interface StatusResponse {
  state: DaemonState;
  uptime_seconds: number;
  connected_servers: number;
  active_tasks: ActiveTaskInfo[];
  queued_tasks: QueuedTaskInfo[];
  paused_reason: PausedReason | null;
  /** Newest failure first. Empty when nothing has failed. */
  failing_leafs: FailingLeaf[];
  /** Version of the running daemon (being added by the CLI; absent on older builds). */
  client_version?: string;
}

// ---------------------------------------------------------------------------
// GET /api/v1/metrics
// ---------------------------------------------------------------------------

/**
 * CPU, GPU, memory and temperature figures are reported as 0 by the daemon
 * (it has no platform collector yet); use `getSystemMetrics()` for host CPU
 * and memory. The disk figures are the fetch gate's own: `disk_used_mb` is
 * Lettuce's measured footprint (data directory plus cached container images)
 * and `disk_allowance_mb` the configured `max_disk_gb` it is budgeted
 * against. When `disk_usage_known` is false, `disk_used_mb` is not a
 * measurement and must be shown as unknown, never as 0.
 */
export interface MetricsResponse {
  cpu_usage_pct: number;
  gpu_usage_pct: number;
  memory_used_mb: number;
  memory_total_mb: number;
  cpu_temp_c: number;
  gpu_temp_c: number;
  disk_used_mb: number;
  disk_allowance_mb: number;
  disk_usage_known: boolean;
}

// ---------------------------------------------------------------------------
// GET /api/v1/history
// ---------------------------------------------------------------------------

export interface HistoryEntry {
  work_unit_id: string;
  leaf_name: string;
  completed_at: string;
  duration_seconds: number;
  cpu_seconds: number;
  /** Always 0 today: per-unit credit is not tracked locally. */
  credit_earned: number;
  validation_status: "accepted" | "rejected";
  head_name: string;
}

export interface HistoryResponse {
  entries: HistoryEntry[];
  pagination: {
    next_cursor: string;
    has_more: boolean;
  };
}

export interface HistoryParams {
  cursor?: string;
  /** 1–200, default 50. */
  limit?: number;
  leaf_id?: string;
  /** RFC 3339. */
  from?: string;
  /** RFC 3339. */
  to?: string;
}

// ---------------------------------------------------------------------------
// GET /api/v1/tasks/{work_unit_id}/details
// ---------------------------------------------------------------------------

export interface TaskDetail extends ActiveTaskInfo {
  memory_rss_mb: number | null;
  virtual_memory_mb: number | null;
  cpu_usage_pct: number | null;
  disk_read_mb: number | null;
  disk_written_mb: number | null;
  time_since_checkpoint_seconds: number | null;
  estimated_completion_at: string | null;
  progress_rate_pct_per_hour: number | null;
  /** Same figure as `progress_pct` (0–100), not a 0–1 fraction. */
  fraction_done: number;
  container_image: string | null;
}

// ---------------------------------------------------------------------------
// GET /api/v1/config and PUT /api/v1/config
// ---------------------------------------------------------------------------

export interface ResourceLimits {
  max_cpu_cores: number;
  max_memory_mb: number;
  max_disk_gb: number;
  /** 0 = unlimited. */
  max_bandwidth_mbps: number;
  /** 0–100; 0 disables GPU tasks. */
  max_gpu_vram_pct: number;
  /** Max processes per container; <= 0 uses the built-in default. */
  max_pids: number;
}

export interface ScheduleRange {
  /** 0 = Monday … 6 = Sunday. */
  days: number[];
  /** 0–23. */
  start_hour: number;
  /** 0–23; may be less than `start_hour` for a window that wraps midnight. */
  end_hour: number;
}

export interface Scheduling {
  /** "ALWAYS", "WHEN_IDLE" or "SCHEDULED". */
  mode: string;
  idle_threshold_mins: number;
  cron_expression?: string;
  /** SCHEDULED mode: active windows. Take precedence over `cron_expression`. */
  schedule_ranges?: ScheduleRange[];
}

export interface LeafFilter {
  /** "ALL", "SPECIFIC" or "BLOCKLIST". */
  mode: string;
  leaf_ids?: string[];
  blocked_ids?: string[];
}

export interface ThermalConfig {
  enabled: boolean;
  cpu_pause_threshold: number;
  cpu_resume_threshold: number;
  gpu_pause_threshold: number;
  gpu_resume_threshold: number;
  poll_interval_seconds: number;
  /** Longest single throttle in minutes; 0 = default (30), negative = unbounded. */
  max_throttle_minutes: number;
}

export interface NotificationConfig {
  credit_milestones: boolean;
  credit_milestone_threshold: number;
  work_unit_completed: boolean;
  errors: boolean;
  updates: boolean;
}

export interface LeafPreferences {
  /** "ALL" (head defaults), "SPECIFIC" (only `enabled`) or "BLOCKLIST" (all but `disabled`). */
  mode: "ALL" | "SPECIFIC" | "BLOCKLIST";
  /** Leaf slug -> weight override. */
  weights?: Record<string, number>;
  /** Leaf slugs, SPECIFIC mode. */
  enabled?: string[];
  /** Leaf slugs, BLOCKLIST mode. */
  disabled?: string[];
}

/** One configured head, as stored in config.yaml. */
export interface ServerConfig {
  grpc_address: string;
  http_address?: string;
  /** Leaf IDs explicitly attached on this head (fetched by ID even if unlisted). */
  pinned_leaf_ids?: string[];
  name: string;
  insecure?: boolean;
  ca_cert?: string;
  cert?: string;
  key?: string;
  /** Head-level share of this machine's work; absent = 100. */
  weight?: number;
  leaf_preferences: LeafPreferences;
  /**
   * Runtimes this head is trusted to run here, uppercase ("CONTAINER",
   * "NATIVE"). Always present; `[]` means WASM only (WASM is always allowed).
   */
  trusted_runtimes: string[];
}

export interface ConfigResponse {
  data_dir: string;
  public_key?: string;
  resource_limits: ResourceLimits;
  scheduling: Scheduling;
  leafs: LeafFilter;
  thermal: ThermalConfig;
  notifications: NotificationConfig;
  servers: ServerConfig[];
  log_level: string;
  max_concurrent_tasks: number;
  /**
   * Hours of work kept buffered per concurrent task (daemon default 2).
   * 0 means the daemon's small fixed unit-count fallback.
   */
  work_buffer_hours: number;
}

/** Per-head fields `PUT /api/v1/config` merges, matched by `name`. */
export interface ServerConfigUpdate {
  name: string;
  weight?: number;
  leaf_preferences?: LeafPreferences;
  /** Uppercase runtime names; being added by the CLI alongside the GET field. */
  trusted_runtimes?: string[];
}

/**
 * Body of `PUT /api/v1/config`: a partial update. Only the listed keys are
 * applied; anything else is ignored.
 */
export interface ConfigUpdate {
  resource_limits?: Partial<ResourceLimits>;
  scheduling?: Partial<Scheduling>;
  thermal?: Partial<ThermalConfig>;
  notifications?: Partial<NotificationConfig>;
  leafs?: Partial<LeafFilter>;
  log_level?: string;
  max_concurrent_tasks?: number;
  /** Hours of work to keep buffered per concurrent task (daemon default 2). */
  work_buffer_hours?: number;
  servers?: ServerConfigUpdate[];
}

/**
 * Response of `PUT /api/v1/config`. The daemon echoes the saved config and
 * adds `restart_required` so the app can tell when a change (for example
 * runtime trust) needs `restartDaemon()` to take effect.
 */
export interface ConfigUpdateResponse {
  status?: string;
  restart_required?: boolean;
}

// ---------------------------------------------------------------------------
// GET /api/v1/heads
// ---------------------------------------------------------------------------

export interface ExecutionSpec {
  binaries?: Record<string, string>;
  image?: string;
  gpu_required?: boolean;
  gpu_type?: string;
  max_memory_mb?: number;
  max_disk_mb?: number;
  network_access?: boolean;
}

/**
 * What the head requires of this machine before it dispatches this leaf's
 * work. Compare against `MachineCapabilities`. Absent from heads too old to
 * report it.
 */
export interface LeafResourceRequirements {
  min_disk_mb?: number;
  min_cpu_cores?: number;
  /** Compared against the machine's ALLOWED VRAM (`max_gpu_vram_mb`), not the card size. */
  min_gpu_vram_mb?: number;
  gpu_type?: string;
  gpu_compute_capability?: string;
  /** A GPU is required when this OR `execution_spec.gpu_required` is set. */
  gpu_required?: boolean;
}

/**
 * The running daemon's live verdict on whether its fetcher is skipping this
 * leaf for disk reasons, and the `max_disk_gb` that would clear it.
 */
export interface LeafDiskGate {
  blocked: boolean;
  reason?: string;
  raise_to_gb?: number;
}

export interface LeafInfo {
  id: string;
  slug: string;
  name: string;
  description?: string;
  /** Normalised: `[]` when the head reports none. */
  research_area: string[];
  task_pattern: string;
  state: string;
  queued_work_units: number;
  active_volunteers: number;
  active_hosts: number;
  /** Whether this machine's leaf preferences allow the leaf. */
  enabled: boolean;
  effective_weight: number;
  execution_spec?: ExecutionSpec;
  resource_requirements?: LeafResourceRequirements;
  /** Present only when the leaf has failed on this machine. */
  failures?: FailingLeaf;
  disk_gate?: LeafDiskGate;
}

export interface HeadInfo {
  name: string;
  description?: string;
  url?: string;
  grpc_address: string;
  status: "connected" | "disconnected";
  weight: number;
  volunteer_id?: string;
  leafs: LeafInfo[];
  /**
   * When the leaf figures were last fetched (RFC 3339). Absent when nothing
   * has been cached yet — show as unknown, never as "now".
   */
  leafs_refreshed_at?: string;
  /** Head software version (being added by the CLI). */
  head_version?: string;
  /** The head requires a newer volunteer client (being added by the CLI). */
  update_required?: boolean;
}

/** This machine's capabilities as the RUNNING daemon sees them. Arrays are normalised to `[]`. */
export interface MachineCapabilities {
  /** Runtime kinds the daemon registered, lowercase (e.g. ["container","wasm"]). */
  runtimes: string[];
  has_gpu: boolean;
  max_memory_mb: number;
  /** `max_disk_gb` as advertised to heads, in MB. */
  max_disk_mb: number;
  max_cpu_cores: number;
  /** ALLOWED VRAM: card size × `max_gpu_vram_pct` / 100 — the figure dispatch compares against. */
  max_gpu_vram_mb: number;
  gpu_card_vram_mb: number;
  gpu_vram_pct: number;
  /** Uppercase, e.g. "NVIDIA". */
  gpu_vendors: string[];
  gpu_compute_capabilities: string[];
}

export interface HeadsResponse {
  heads: HeadInfo[];
  machine: MachineCapabilities;
}

/**
 * Shape of the head's public `GET /api/v1/head` as returned by the
 * `fetch_head_info` command. Heads send `research_area` as a single string or
 * as a list; callers must accept both.
 */
export interface HeadPreview {
  name: string;
  description: string;
  leafs: Array<{ slug: string; name: string; research_area: string | string[] }>;
}

/**
 * One leaf in `fetchHeadInfo()`'s result. The head sends `research_area` as a
 * list of strings; it is joined here so callers can render it directly.
 * `state` is the leaf lifecycle state (`ACTIVE`, `PAUSED`, ...).
 */
export interface HeadInfoLeaf {
  slug: string;
  name: string;
  research_area: string;
  state: string;
}

export interface HeadInfoResponse {
  name: string;
  description: string;
  leafs: HeadInfoLeaf[];
}

/**
 * Probe a head's `GET /api/v1/health` through the Rust host (the `test_server_connection`
 * command). Resolves to `{ status: "healthy" }` on success; rejects with the
 * command's message otherwise. `url` may omit the scheme (https is assumed).
 */
export async function testServerConnection(url: string): Promise<{ status: string }> {
  return invoke("test_server_connection", { url });
}

/** The head's public `GET /api/v1/head` (the `fetch_head_info` command), with fields normalised. */
export async function fetchHeadInfo(url: string): Promise<HeadInfoResponse> {
  const raw = await invoke<{
    name?: string;
    description?: string;
    leafs?: Array<{ slug?: string; name?: string; research_area?: unknown; state?: string }> | null;
  }>("fetch_head_info", { url });
  return {
    name: raw.name ?? "",
    description: raw.description ?? "",
    leafs: (raw.leafs ?? []).map((l) => ({
      slug: l.slug ?? "",
      name: l.name ?? "",
      research_area: Array.isArray(l.research_area)
        ? l.research_area.filter((s): s is string => typeof s === "string").join(", ")
        : typeof l.research_area === "string"
          ? l.research_area
          : "",
      state: l.state ?? "",
    })),
  };
}

// ---------------------------------------------------------------------------
// GET /api/v1/credit
// ---------------------------------------------------------------------------

export interface LeafCredit {
  leaf_id: string;
  leaf_name: string;
  credit: number;
}

export interface HeadCredit {
  head_name: string;
  volunteer_id: string;
  total_credit: number;
  /** False when the head was unreachable or too old to report credit. */
  available: boolean;
}

/**
 * Credit for the volunteer ACCOUNT, summed over all of its machines. Numbers
 * are decimals. `source` is "head" when at least one head answered
 * (authoritative) or "local" when derived from local history because no head
 * could be reached (one accepted unit counted as one credit).
 */
export interface CreditSummary {
  total_credit: number;
  today: number;
  this_week: number;
  this_month: number;
  by_leaf: LeafCredit[];
  by_head: HeadCredit[];
  source: "head" | "local";
  /**
   * The calendar rule behind `today`, `this_week` and `this_month`. "utc" when
   * they come from the head's daily timeline (the head records credit by UTC
   * date, so the buckets cannot follow this machine's clock); "local" when they
   * were cut from the local history file by this machine's day, the same rule
   * the History page groups by.
   */
  day_boundary: "utc" | "local";
}

// ---------------------------------------------------------------------------
// POST /api/v1/leafs/attach and /detach
// ---------------------------------------------------------------------------

export interface AttachRequest {
  server_address: string;
  leaf_id?: string;
  name?: string;
  /** Uppercase runtime names to trust the head with (being added by the CLI; omitted = WASM only). */
  trusted_runtimes?: string[];
}

export interface DetachRequest {
  server_name?: string;
  server_address?: string;
}

// ---------------------------------------------------------------------------
// GET /api/v1/results
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// POST /api/v1/identity/sign
// ---------------------------------------------------------------------------

export interface SignChallengeResponse {
  public_key: string;
  signature: string;
}

// ---------------------------------------------------------------------------
// GET /api/v1/notices?since=<id> (being added by the CLI)
// ---------------------------------------------------------------------------

export type NoticeLevel = "info" | "warn" | "error";

export interface Notice {
  id: number;
  level: NoticeLevel;
  code: string;
  message: string;
  head?: string;
  leaf?: string;
  /** Times this notice repeated; `first_at` is the first occurrence, `at` the latest. */
  count: number;
  first_at: string;
  at: string;
  /**
   * When the daemon observed the condition end (work arrived after a no-work
   * streak, the disk gate cleared, a leaf recovered). Absent while it is live.
   */
  resolved_at?: string;
}

export interface NoticesResponse {
  notices: Notice[];
  latest_id: number;
}

// ---------------------------------------------------------------------------
// Container runtime (dedicated Rust commands, used before the daemon exists)
// ---------------------------------------------------------------------------

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
 * What the `detect_container_runtime` command found on this machine. Unlike
 * `getContainerRuntimeStatus` it needs no daemon, so the setup wizard can use
 * it before `init` has run. `responding` means the engine answers: a running
 * Podman machine (Windows/macOS), a Podman API socket (Linux), or a Docker
 * server that replied. `detail` says why not, in plain language.
 */
export interface ContainerRuntimeDetection {
  backend: "podman" | "docker" | "none";
  version: string;
  binary_path: string;
  responding: boolean;
  detail: string;
}

export async function detectContainerRuntime(): Promise<ContainerRuntimeDetection> {
  return invoke("detect_container_runtime");
}

// ---------------------------------------------------------------------------
// Host-side commands (Rust, no daemon involved)
// ---------------------------------------------------------------------------

/** Names accepted by `schedule set --days`. */
export type ScheduleWeekday = "mon" | "tue" | "wed" | "thu" | "fri" | "sat" | "sun";

/** A daily window for `run_init` (`from_hour`/`to_hour` are 0–23; `to <= from` wraps midnight). */
export interface InitScheduleWindow {
  from_hour: number;
  to_hour: number;
  days: ScheduleWeekday[];
}

/**
 * The `run_init` payload. `schedule_mode` is the CLI's `init --schedule-mode`
 * value; `schedule_window` makes the Rust side run `schedule set` after
 * `init`. `trust` lists the runtimes the head at `server_url` may run beyond
 * the always-allowed WASM sandbox (`"container"`, `"native"`); it is sent as
 * `--trust` and an empty list records an explicit WASM-only decision.
 */
export interface InitRequest {
  cpu_cores: number | null;
  memory_mb: number | null;
  gpu_vram_pct: number | null;
  disk_gb: number | null;
  schedule_mode: "always" | "idle" | "scheduled" | null;
  idle_threshold_mins: number | null;
  schedule_window: InitScheduleWindow | null;
  server_url: string | null;
  trust: Array<"container" | "native">;
  enabled_leafs: string[] | null;
}

/**
 * Run `lettuce-volunteer init` with the wizard's choices, apply any schedule
 * window, then start the daemon and wait for it. Rejects with the CLI's
 * error text when any step fails.
 */
export async function runInit(config: InitRequest): Promise<void> {
  await invoke("run_init", { config });
}

/**
 * Total physical system memory in MB, read from the OS by the Rust backend.
 * Used by the setup wizard to size the memory slider to real hardware.
 * Returns 0 if detection fails (caller should fall back to a default).
 */
export async function getSystemMemoryMb(): Promise<number> {
  return invoke("get_system_memory_mb");
}

/**
 * Logical CPUs available on this machine, read from the OS by the Rust host.
 * The web view cannot be trusted for this: WebKit (Linux and macOS) caps
 * `navigator.hardwareConcurrency` at 8, and the number ends up as the daemon's
 * hard container CPU quota (TB-47). Sizes the CPU-cores sliders and the wizard
 * default. Returns 0 if detection fails (caller should fall back).
 */
export async function getSystemCpuCount(): Promise<number> {
  return invoke("get_system_cpu_count");
}

/** Host CPU and memory usage measured by the app itself (the daemon reports zeros). */
export interface SystemMetrics {
  cpu_usage_pct: number;
  memory_used_mb: number;
  memory_total_mb: number;
}

export async function getSystemMetrics(): Promise<SystemMetrics> {
  return invoke("system_metrics");
}

/**
 * Stop the daemon (gracefully, forced after 30 s) and start a fresh one.
 * Resolves once the new daemon has written daemon.json; may take up to about
 * a minute. Callers should re-run their queries afterwards.
 */
export async function restartDaemon(): Promise<void> {
  await invoke("restart_daemon");
}

/** Version of the bundled `lettuce-volunteer` CLI (`--version`). */
export async function getClientVersion(): Promise<string> {
  return invoke("get_client_version");
}

/**
 * The data directory the app and its bundled client use: `~/.lettuce`, or
 * the `LETTUCE_DATA_DIR` override for a second, isolated profile (see the
 * README). Absolute.
 */
export async function getDataDir(): Promise<string> {
  return invoke("get_data_dir");
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

/**
 * A failed management-API call. `code` is the daemon's own error code
 * (`VALIDATION_ERROR`, `NOT_FOUND`, `CONFLICT`, `INTERNAL_ERROR`, ...),
 * `UNKNOWN` when the response carried no parseable error envelope, or
 * `DAEMON_UNREACHABLE` when no request reached the daemon (not running,
 * still starting, or timed out). `status` is the HTTP status, 0 when there
 * was no response.
 */
export class ApiError extends Error {
  code: string;
  status: number;

  constructor(code: string, message: string, status: number = 0) {
    super(message);
    this.code = code;
    this.status = status;
    this.name = "ApiError";
  }
}

/** Shape of the rejection value the `mgmt_request` command produces. */
interface MgmtErrorShape {
  code: string;
  message: string;
  status?: number;
}

function isMgmtError(err: unknown): err is MgmtErrorShape {
  return (
    typeof err === "object" &&
    err !== null &&
    typeof (err as MgmtErrorShape).code === "string" &&
    typeof (err as MgmtErrorShape).message === "string"
  );
}

function toApiError(err: unknown): ApiError {
  if (err instanceof ApiError) return err;
  if (isMgmtError(err)) {
    return new ApiError(
      err.code,
      err.message,
      typeof err.status === "number" ? err.status : 0
    );
  }
  return new ApiError("UNKNOWN", typeof err === "string" ? err : String(err));
}

type HttpMethod = "GET" | "POST" | "PUT" | "DELETE";

function withQuery(path: string, query: URLSearchParams): string {
  const qs = query.toString();
  return qs ? `${path}?${qs}` : path;
}

function list<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

/** `LeafInfo` as the daemon sends it: `research_area` may be absent. */
type RawLeafInfo = Omit<LeafInfo, "research_area"> & {
  research_area?: string[] | null;
};

type RawHeadInfo = Omit<HeadInfo, "leafs"> & { leafs?: RawLeafInfo[] | null };

type RawMachineCapabilities = Omit<
  MachineCapabilities,
  "runtimes" | "gpu_vendors" | "gpu_compute_capabilities"
> & {
  runtimes?: string[] | null;
  gpu_vendors?: string[] | null;
  gpu_compute_capabilities?: string[] | null;
};

interface RawHeadsResponse {
  heads?: RawHeadInfo[] | null;
  machine?: RawMachineCapabilities | null;
}

function normaliseLeaf(leaf: RawLeafInfo): LeafInfo {
  return { ...leaf, research_area: list(leaf.research_area) };
}

function normaliseHead(head: RawHeadInfo): HeadInfo {
  return { ...head, leafs: list(head.leafs).map(normaliseLeaf) };
}

function normaliseMachine(
  machine: RawMachineCapabilities | null | undefined
): MachineCapabilities {
  const m = machine ?? ({} as RawMachineCapabilities);
  return {
    runtimes: list(m.runtimes),
    has_gpu: m.has_gpu ?? false,
    max_memory_mb: m.max_memory_mb ?? 0,
    max_disk_mb: m.max_disk_mb ?? 0,
    max_cpu_cores: m.max_cpu_cores ?? 0,
    max_gpu_vram_mb: m.max_gpu_vram_mb ?? 0,
    gpu_card_vram_mb: m.gpu_card_vram_mb ?? 0,
    gpu_vram_pct: m.gpu_vram_pct ?? 0,
    gpu_vendors: list(m.gpu_vendors),
    gpu_compute_capabilities: list(m.gpu_compute_capabilities),
  };
}

type RawServerConfig = Omit<ServerConfig, "trusted_runtimes"> & {
  trusted_runtimes?: string[] | null;
};

type RawConfigResponse = Omit<ConfigResponse, "servers"> & {
  servers?: RawServerConfig[] | null;
};

/**
 * The daemon reports `runtime_type` in upper case ("CONTAINER"); the app keys
 * its badges and filters on the lower-case form. Normalise once, at the edge.
 */
function normalizeTask<T extends { runtime_type: string }>(task: T): T {
  return { ...task, runtime_type: String(task.runtime_type ?? "").toLowerCase() };
}

export class ManagementClient {
  private constructor() {}

  /**
   * Resolves once the daemon answers. Rejects with `ApiError`
   * (`DAEMON_UNREACHABLE`) while the daemon is not running or still starting,
   * so `useClient` keeps retrying until it is up.
   */
  static async create(): Promise<ManagementClient> {
    const client = new ManagementClient();
    await client.request("GET", "/api/v1/status");
    return client;
  }

  /**
   * Relay one request through the Rust host. A 204 / empty response resolves
   * to `undefined`; any failure rejects with `ApiError`.
   */
  private async request<T>(
    method: HttpMethod,
    path: string,
    body?: unknown
  ): Promise<T> {
    let result: unknown;
    try {
      result = await invoke<unknown>("mgmt_request", {
        method,
        path,
        body: body ?? null,
      });
    } catch (err) {
      throw toApiError(err);
    }
    return (result === null ? undefined : result) as T;
  }

  // --- daemon ---

  async status(): Promise<StatusResponse> {
    const resp = await this.request<StatusResponse>("GET", "/api/v1/status");
    return {
      ...resp,
      active_tasks: list(resp.active_tasks).map(normalizeTask),
      queued_tasks: list(resp.queued_tasks),
      failing_leafs: list(resp.failing_leafs),
    };
  }

  async pause(): Promise<void> {
    await this.request("POST", "/api/v1/daemon/pause");
  }

  async resume(): Promise<void> {
    await this.request("POST", "/api/v1/daemon/resume");
  }

  async metrics(): Promise<MetricsResponse> {
    return this.request("GET", "/api/v1/metrics");
  }

  /** Notices newer than `since` (omit for all retained notices). */
  async notices(since?: number): Promise<NoticesResponse> {
    const query = new URLSearchParams();
    if (since !== undefined) query.set("since", String(since));
    const resp = await this.request<NoticesResponse>(
      "GET",
      withQuery("/api/v1/notices", query)
    );
    return { notices: list(resp.notices), latest_id: resp.latest_id ?? 0 };
  }

  // --- heads and leafs ---

  /** Configured heads with their leafs, plus this machine's capabilities. */
  async headsAndMachine(): Promise<HeadsResponse> {
    const resp = await this.request<RawHeadsResponse>("GET", "/api/v1/heads");
    return {
      heads: list(resp.heads).map(normaliseHead),
      machine: normaliseMachine(resp.machine),
    };
  }

  async attachHead(req: AttachRequest): Promise<void> {
    await this.request("POST", "/api/v1/leafs/attach", req);
  }

  async detachHead(req: DetachRequest): Promise<void> {
    await this.request("POST", "/api/v1/leafs/detach", req);
  }

  // --- history, credit, results ---

  async history(params?: HistoryParams): Promise<HistoryResponse> {
    const query = new URLSearchParams();
    if (params?.cursor) query.set("cursor", params.cursor);
    if (params?.limit) query.set("limit", params.limit.toString());
    if (params?.leaf_id) query.set("leaf_id", params.leaf_id);
    if (params?.from) query.set("from", params.from);
    if (params?.to) query.set("to", params.to);
    const resp = await this.request<HistoryResponse>(
      "GET",
      withQuery("/api/v1/history", query)
    );
    return { ...resp, entries: list(resp.entries) };
  }

  async credit(): Promise<CreditSummary> {
    const resp = await this.request<CreditSummary>("GET", "/api/v1/credit");
    return { ...resp, by_leaf: list(resp.by_leaf), by_head: list(resp.by_head) };
  }

  async results(): Promise<ResultsResponse> {
    const resp = await this.request<ResultsResponse>("GET", "/api/v1/results");
    return { results: list(resp.results) };
  }

  async resultData(workUnitId: string): Promise<Record<string, unknown>> {
    return this.request("GET", `/api/v1/results/${workUnitId}`);
  }

  // --- config ---

  async config(): Promise<ConfigResponse> {
    const resp = await this.request<RawConfigResponse>("GET", "/api/v1/config");
    return {
      ...resp,
      servers: list(resp.servers).map((s) => ({
        ...s,
        trusted_runtimes: list(s.trusted_runtimes),
      })),
    };
  }

  async updateConfig(partial: ConfigUpdate): Promise<ConfigUpdateResponse> {
    return this.request("PUT", "/api/v1/config", partial);
  }

  // --- identity ---

  async signChallenge(challengeHex: string): Promise<SignChallengeResponse> {
    return this.request("POST", "/api/v1/identity/sign", {
      challenge_hex: challengeHex,
    });
  }

  // --- tasks ---

  async suspendTask(workUnitId: string): Promise<void> {
    await this.request("POST", `/api/v1/tasks/${workUnitId}/suspend`);
  }

  async resumeTask(workUnitId: string): Promise<void> {
    await this.request("POST", `/api/v1/tasks/${workUnitId}/resume`);
  }

  async abortTask(workUnitId: string): Promise<void> {
    await this.request("POST", `/api/v1/tasks/${workUnitId}/abort`);
  }

  async taskDetails(workUnitId: string): Promise<TaskDetail> {
    const detail = await this.request<TaskDetail>("GET", `/api/v1/tasks/${workUnitId}/details`);
    return normalizeTask(detail);
  }
}
