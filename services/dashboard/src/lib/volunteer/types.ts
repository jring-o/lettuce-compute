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

export interface ExecutionMetrics {
  wall_clock_seconds: number;
  cpu_seconds_user: number;
  peak_memory_mb: number;
}

// Registration proof-of-work (the head's REST contract: POST
// /api/v1/volunteers/register-challenge issues a challenge; the register body then
// carries pow_challenge_id + pow_nonce). The solution rule is pinned by the golden
// byte vector shared with the head's pow package: the digest of
// SHA-256(challenge || publicKey || nonce as 8 big-endian bytes) must have at least
// difficulty_bits leading zero bits.

export interface RegisterChallengeResponse {
  challenge_id: string;
  challenge_hex: string; // 32 bytes, hex-encoded — part of the hash preimage
  difficulty_bits: number;
  expires_at: string; // RFC 3339
}

// A solved challenge, ready to ride a register request.
export interface RegisterPow {
  challengeId: string;
  // Decimal uint64 string — the wire encoding (JSON numbers cannot carry uint64).
  nonce: string;
}

export interface PowSolution {
  nonce: string; // decimal uint64 string, same encoding as RegisterPow.nonce
  attempts: number;
}

// Solver worker <-> main thread messages (one-shot pow-worker). The solve loop is
// synchronous, so the worker never processes incoming messages mid-solve —
// cancellation is exclusively worker.terminate() from the main thread.

export type SolverRequestMessage = {
  type: "solve";
  challengeHex: string;
  publicKeyBase64url: string;
  difficultyBits: number;
};

export type SolverResponseMessage =
  | { type: "solved"; nonce: string; attempts: number }
  | { type: "pow-progress"; attempts: number }
  | { type: "error"; message: string };

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
