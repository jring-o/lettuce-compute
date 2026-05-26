// REST client for the browser volunteer API with Ed25519 request signing.

import type { VolunteerIdentity } from "./identity";
import { base64urlEncode, bytesToHex } from "./identity";
import type {
  HardwareCapabilities,
  RegisterResponse,
  RequestWorkOptions,
  WorkUnitResponse,
  SubmitResultRequest,
  SubmitResultResponse,
  HeartbeatRequest,
  HeartbeatResponse,
} from "./types";

// Default timeout for API requests (30 seconds).
const DEFAULT_TIMEOUT_MS = 30_000;
// Longer timeout for result submission (large payloads).
const SUBMIT_TIMEOUT_MS = 60_000;

export interface VolunteerClient {
  register(hardware: HardwareCapabilities): Promise<RegisterResponse>;
  requestWork(opts: RequestWorkOptions): Promise<WorkUnitResponse | null>;
  submitResult(result: SubmitResultRequest): Promise<SubmitResultResponse>;
  heartbeat(req: HeartbeatRequest): Promise<HeartbeatResponse>;
}

async function sha256Hex(data: string): Promise<string> {
  const encoded = new TextEncoder().encode(data);
  const hash = await crypto.subtle.digest("SHA-256", encoded.buffer as ArrayBuffer);
  return bytesToHex(new Uint8Array(hash));
}

async function signRequest(
  identity: VolunteerIdentity,
  method: string,
  path: string,
  body: string
): Promise<string> {
  const timestamp = Math.floor(Date.now() / 1000);
  const bodyHash = await sha256Hex(body);
  const message = `${timestamp}:${method}:${path}:${bodyHash}`;
  const messageBytes = new TextEncoder().encode(message);
  const signature = await crypto.subtle.sign(
    "Ed25519",
    identity.privateKey,
    messageBytes
  );
  const sigBase64url = base64urlEncode(new Uint8Array(signature));
  return `Ed25519 ${identity.publicKeyBase64url}:${sigBase64url}:${timestamp}`;
}

export function createVolunteerClient(
  serverUrl: string,
  identity: VolunteerIdentity
): VolunteerClient {
  const baseUrl = serverUrl.replace(/\/$/, "");

  async function authenticatedFetch(
    path: string,
    body: object,
    timeoutMs: number = DEFAULT_TIMEOUT_MS
  ): Promise<Response> {
    const bodyStr = JSON.stringify(body);
    const authHeader = await signRequest(identity, "POST", path, bodyStr);
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
      return await fetch(`${baseUrl}${path}`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: authHeader,
        },
        body: bodyStr,
        signal: controller.signal,
      });
    } finally {
      clearTimeout(timer);
    }
  }

  return {
    async register(hardware: HardwareCapabilities): Promise<RegisterResponse> {
      const body = {
        public_key: identity.publicKeyBase64url,
        hardware,
      };
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), DEFAULT_TIMEOUT_MS);
      let resp: Response;
      try {
        resp = await fetch(`${baseUrl}/api/v1/volunteers/register`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
          signal: controller.signal,
        });
      } finally {
        clearTimeout(timer);
      }
      if (!resp.ok && resp.status !== 409) {
        throw new Error(
          `Registration failed: ${resp.status} ${await resp.text()}`
        );
      }
      return resp.json();
    },

    async requestWork(
      opts: RequestWorkOptions
    ): Promise<WorkUnitResponse | null> {
      const path = "/api/v1/volunteers/request-work";
      const resp = await authenticatedFetch(path, {
        leaf_ids: opts.leaf_ids ?? [],
        max_memory_mb: opts.max_memory_mb ?? 4096,
        max_disk_mb: opts.max_disk_mb ?? 51200,
        has_gpu: opts.has_gpu ?? false,
        gpu_vendors: opts.gpu_vendors ?? [],
      });
      if (resp.status === 404) return null;
      if (!resp.ok) {
        throw new Error(
          `Request work failed: ${resp.status} ${await resp.text()}`
        );
      }
      return resp.json();
    },

    async submitResult(
      result: SubmitResultRequest
    ): Promise<SubmitResultResponse> {
      const path = "/api/v1/volunteers/submit-result";
      const resp = await authenticatedFetch(path, result, SUBMIT_TIMEOUT_MS);
      if (!resp.ok) {
        throw new Error(
          `Submit result failed: ${resp.status} ${await resp.text()}`
        );
      }
      return resp.json();
    },

    async heartbeat(req: HeartbeatRequest): Promise<HeartbeatResponse> {
      const path = "/api/v1/volunteers/heartbeat";
      const resp = await authenticatedFetch(path, req);
      if (!resp.ok) {
        throw new Error(
          `Heartbeat failed: ${resp.status} ${await resp.text()}`
        );
      }
      return resp.json();
    },
  };
}
