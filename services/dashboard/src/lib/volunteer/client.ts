// REST client for the browser volunteer API with Ed25519 request signing.

import type { VolunteerIdentity } from "./identity";
import { base64urlEncode, bytesToHex } from "./identity";
import type {
  HardwareCapabilities,
  RegisterResponse,
  RegisterChallengeResponse,
  RegisterPow,
  RequestWorkOptions,
  WorkUnitResponse,
  SubmitResultRequest,
  SubmitResultResponse,
} from "./types";

// Default timeout for API requests (30 seconds).
const DEFAULT_TIMEOUT_MS = 30_000;
// Longer timeout for result submission (large payloads).
const SUBMIT_TIMEOUT_MS = 60_000;

// Error raised for non-ok responses from the head's REST API. The head wraps
// errors in an {"error":{"code","message"}} envelope; ApiError surfaces the
// parsed code so callers classify on it (e.g. POW_REQUIRED, POW_INVALID,
// REGISTRATION_CAP_EXCEEDED) instead of matching brittle message text. When the
// body is not that envelope, code and serverMessage are null and the raw body
// rides Error.message.
export class ApiError extends Error {
  readonly status: number;
  readonly code: string | null;
  readonly serverMessage: string | null;

  constructor(
    status: number,
    code: string | null,
    serverMessage: string | null,
    message: string
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.serverMessage = serverMessage;
  }
}

// Parse the head's {"error":{"code","message"}} envelope out of a body string.
// Returns nulls for both fields when the body is not that shape (non-JSON, or
// JSON without the envelope).
function parseErrorEnvelope(raw: string): {
  code: string | null;
  message: string | null;
} {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (
      parsed &&
      typeof parsed === "object" &&
      "error" in parsed &&
      parsed.error &&
      typeof parsed.error === "object"
    ) {
      const err = parsed.error as { code?: unknown; message?: unknown };
      return {
        code: typeof err.code === "string" ? err.code : null,
        message: typeof err.message === "string" ? err.message : null,
      };
    }
  } catch {
    // Not JSON — fall through to the null/raw-text fallback.
  }
  return { code: null, message: null };
}

// Build an ApiError from a non-ok Response with a SINGLE body read. Real
// browsers throw "body stream already read" if both .json() and .text() are
// called on one Response, so the body is read exactly once via .text() and
// JSON-parsed in a try/catch.
async function apiErrorFromResponse(
  resp: Response,
  context: string
): Promise<ApiError> {
  const raw = await resp.text();
  const { code, message: serverMessage } = parseErrorEnvelope(raw);
  const message = `${context} failed: ${resp.status} ${serverMessage ?? raw}`;
  return new ApiError(resp.status, code, serverMessage, message);
}

export interface VolunteerClient {
  register(
    hardware: HardwareCapabilities,
    pow?: RegisterPow
  ): Promise<RegisterResponse>;
  fetchRegistrationChallenge(): Promise<RegisterChallengeResponse>;
  requestWork(opts: RequestWorkOptions): Promise<WorkUnitResponse | null>;
  submitResult(result: SubmitResultRequest): Promise<SubmitResultResponse>;
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
    async register(
      hardware: HardwareCapabilities,
      pow?: RegisterPow
    ): Promise<RegisterResponse> {
      const body: {
        public_key: string;
        hardware: HardwareCapabilities;
        pow_challenge_id?: string;
        pow_nonce?: string;
      } = {
        public_key: identity.publicKeyBase64url,
        hardware,
      };
      if (pow) {
        body.pow_challenge_id = pow.challengeId;
        body.pow_nonce = pow.nonce;
      }
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
        throw await apiErrorFromResponse(resp, "Registration");
      }
      return resp.json();
    },

    async fetchRegistrationChallenge(): Promise<RegisterChallengeResponse> {
      const body = { public_key: identity.publicKeyBase64url };
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), DEFAULT_TIMEOUT_MS);
      let resp: Response;
      try {
        resp = await fetch(
          `${baseUrl}/api/v1/volunteers/register-challenge`,
          {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(body),
            signal: controller.signal,
          }
        );
      } finally {
        clearTimeout(timer);
      }
      if (!resp.ok) {
        throw await apiErrorFromResponse(resp, "Registration challenge");
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
  };
}
