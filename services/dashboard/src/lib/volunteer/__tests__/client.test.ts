import { createVolunteerClient, ApiError } from "../client";
import type { VolunteerIdentity } from "../identity";
import type { HardwareCapabilities } from "../types";

// Mock crypto.subtle for signing.
const mockSignature = new Uint8Array(64);
for (let i = 0; i < 64; i++) mockSignature[i] = i;

const mockDigest = new Uint8Array(32);
for (let i = 0; i < 32; i++) mockDigest[i] = i * 3;

Object.defineProperty(global, "crypto", {
  value: {
    subtle: {
      sign: jest.fn().mockResolvedValue(mockSignature.buffer),
      digest: jest.fn().mockResolvedValue(mockDigest.buffer),
    },
  },
  writable: true,
});

const mockFetch = jest.fn();
global.fetch = mockFetch;

const mockPublicKey = new Uint8Array(32);
for (let i = 0; i < 32; i++) mockPublicKey[i] = i;

const mockIdentity: VolunteerIdentity = {
  publicKey: mockPublicKey,
  privateKey: { type: "private", algorithm: { name: "Ed25519" } } as CryptoKey,
  publicKeyBase64url: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
  fingerprint: "0123456789abcdef",
};

const mockHardware: HardwareCapabilities = {
  cpu_cores: 2,
  memory_mb: 4096,
  has_gpu: false,
  gpu_vendors: [],
  available_runtimes: ["WASM"],
};

// Single-read Response stub: the body may be read exactly once, via text() OR
// json(). A second read on the same Response rejects, mirroring real browsers
// ("body stream already read"), so a production double-read regression fails the
// suite (design §2.1 HIGH-1). A string body is served verbatim (for non-JSON
// error bodies); anything else is JSON-serialized.
function mockResponse(status: number, body: unknown) {
  const bodyStr = typeof body === "string" ? body : JSON.stringify(body);
  let consumed = false;
  const readOnce = (): string => {
    if (consumed) {
      throw new TypeError("Body is unusable: Body has already been read");
    }
    consumed = true;
    return bodyStr;
  };
  mockFetch.mockResolvedValueOnce({
    ok: status >= 200 && status < 300,
    status,
    text: () => Promise.resolve().then(readOnce),
    json: () => Promise.resolve().then(() => JSON.parse(readOnce())),
  });
}

// Resolve the ApiError a rejecting client call throws, with its type preserved.
async function expectApiError(promise: Promise<unknown>): Promise<ApiError> {
  try {
    await promise;
  } catch (e) {
    if (e instanceof ApiError) return e;
    throw e;
  }
  throw new Error("expected the promise to reject with an ApiError");
}

describe("Volunteer REST Client", () => {
  const client = createVolunteerClient("http://localhost:8080", mockIdentity);

  beforeEach(() => {
    jest.clearAllMocks();
  });

  describe("register", () => {
    it("sends correct request format without auth", async () => {
      const hardware: HardwareCapabilities = {
        cpu_cores: 4,
        memory_mb: 8192,
        has_gpu: true,
        gpu_vendors: ["WEBGPU"],
        available_runtimes: ["WASM"],
      };

      mockResponse(201, {
        volunteer_id: "vol-1",
        registered_at: "2026-03-27T00:00:00Z",
      });

      const result = await client.register(hardware);

      expect(result.volunteer_id).toBe("vol-1");
      expect(mockFetch).toHaveBeenCalledTimes(1);

      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://localhost:8080/api/v1/volunteers/register");
      expect(opts.method).toBe("POST");
      expect(opts.headers["Content-Type"]).toBe("application/json");
      // Register does NOT have Authorization header.
      expect(opts.headers.Authorization).toBeUndefined();

      const body = JSON.parse(opts.body);
      expect(body.public_key).toBe(mockIdentity.publicKeyBase64url);
      expect(body.hardware.cpu_cores).toBe(4);
      expect(body.hardware.has_gpu).toBe(true);
    });

    it("handles 409 Conflict (already registered)", async () => {
      mockResponse(409, {
        volunteer_id: "existing-vol",
        registered_at: "2026-01-01T00:00:00Z",
      });

      const result = await client.register(mockHardware);

      expect(result.volunteer_id).toBe("existing-vol");
    });

    it("carries pow_challenge_id and pow_nonce when a solution is supplied", async () => {
      mockResponse(201, {
        volunteer_id: "vol-pow",
        registered_at: "2026-03-27T00:00:00Z",
      });

      await client.register(mockHardware, {
        challengeId: "chal-1",
        nonce: "1099511628211",
      });

      const [, opts] = mockFetch.mock.calls[0];
      const body = JSON.parse(opts.body);
      expect(body.pow_challenge_id).toBe("chal-1");
      expect(body.pow_nonce).toBe("1099511628211");
    });

    it("omits the pow fields when no solution is supplied", async () => {
      mockResponse(201, {
        volunteer_id: "vol-bare",
        registered_at: "2026-03-27T00:00:00Z",
      });

      await client.register(mockHardware);

      const [, opts] = mockFetch.mock.calls[0];
      const body = JSON.parse(opts.body);
      expect(body).not.toHaveProperty("pow_challenge_id");
      expect(body).not.toHaveProperty("pow_nonce");
    });

    it("throws a typed ApiError on 403 POW_REQUIRED", async () => {
      mockResponse(403, {
        error: {
          code: "POW_REQUIRED",
          message: "registration requires proof-of-work",
        },
      });

      const err = await expectApiError(client.register(mockHardware));
      expect(err.status).toBe(403);
      expect(err.code).toBe("POW_REQUIRED");
      expect(err.serverMessage).toBe("registration requires proof-of-work");
    });

    it("throws a typed ApiError on 400 POW_INVALID", async () => {
      mockResponse(400, {
        error: {
          code: "POW_INVALID",
          message: "proof-of-work solution is invalid or expired",
        },
      });

      const err = await expectApiError(
        client.register(mockHardware, { challengeId: "chal-1", nonce: "42" })
      );
      expect(err.status).toBe(400);
      expect(err.code).toBe("POW_INVALID");
      expect(err.serverMessage).toBe(
        "proof-of-work solution is invalid or expired"
      );
    });

    it("carries REGISTRATION_CAP_EXCEEDED on a 429 with its server message", async () => {
      mockResponse(429, {
        error: {
          code: "REGISTRATION_CAP_EXCEEDED",
          message: "registration capacity reached; try again later",
        },
      });

      const err = await expectApiError(client.register(mockHardware));
      expect(err.status).toBe(429);
      expect(err.code).toBe("REGISTRATION_CAP_EXCEEDED");
      expect(err.serverMessage).toBe(
        "registration capacity reached; try again later"
      );
    });

    it("carries RATE_LIMITED on a 429 with its server message", async () => {
      mockResponse(429, {
        error: {
          code: "RATE_LIMITED",
          message: "too many requests",
        },
      });

      const err = await expectApiError(client.register(mockHardware));
      expect(err.status).toBe(429);
      expect(err.code).toBe("RATE_LIMITED");
      expect(err.serverMessage).toBe("too many requests");
    });

    it("falls back to raw text when the error body is not the envelope", async () => {
      mockResponse(502, "Bad Gateway");

      const err = await expectApiError(client.register(mockHardware));
      expect(err.status).toBe(502);
      expect(err.code).toBeNull();
      expect(err.serverMessage).toBeNull();
      expect(err.message).toContain("Bad Gateway");
    });

    it("throws on non-409 error response", async () => {
      mockResponse(500, "internal server error");

      await expect(client.register(mockHardware)).rejects.toThrow(
        "Registration failed: 500"
      );
    });
  });

  describe("fetchRegistrationChallenge", () => {
    it("posts the public key unauthenticated and returns the parsed challenge", async () => {
      mockResponse(200, {
        challenge_id: "chal-9",
        challenge_hex: "00".repeat(32),
        difficulty_bits: 20,
        expires_at: "2026-07-09T00:10:00Z",
      });

      const challenge = await client.fetchRegistrationChallenge();

      expect(challenge.challenge_id).toBe("chal-9");
      expect(challenge.difficulty_bits).toBe(20);

      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe(
        "http://localhost:8080/api/v1/volunteers/register-challenge"
      );
      expect(opts.method).toBe("POST");
      expect(opts.headers["Content-Type"]).toBe("application/json");
      // Challenge issuance is unauthenticated — no signing header.
      expect(opts.headers.Authorization).toBeUndefined();

      const body = JSON.parse(opts.body);
      expect(body.public_key).toBe(mockIdentity.publicKeyBase64url);
    });

    it("throws a typed ApiError when issuance fails", async () => {
      mockResponse(503, {
        error: {
          code: "POW_UNAVAILABLE",
          message: "proof-of-work is not configured",
        },
      });

      const err = await expectApiError(client.fetchRegistrationChallenge());
      expect(err.status).toBe(503);
      expect(err.code).toBe("POW_UNAVAILABLE");
      expect(err.serverMessage).toBe("proof-of-work is not configured");
    });
  });

  describe("requestWork", () => {
    it("includes Ed25519 auth header", async () => {
      mockResponse(200, {
        work_unit_id: "wu-1",
        leaf_id: "leaf-1",
        runtime: "WASM",
        deadline_seconds: 3600,
        execution_spec: {
          binaries: {},
          gpu_required: false,
          max_memory_mb: 4096,
          max_disk_mb: 51200,
          network_access: false,
        },
      });

      await client.requestWork({ leaf_ids: ["leaf-1"] });

      const [, opts] = mockFetch.mock.calls[0];
      const authHeader = opts.headers.Authorization as string;
      expect(authHeader).toMatch(
        /^Ed25519 [A-Za-z0-9_-]+:[A-Za-z0-9_-]+:\d+$/
      );

      // Verify structure: pubkey:signature:timestamp.
      const parts = authHeader.replace("Ed25519 ", "").split(":");
      expect(parts).toHaveLength(3);
      expect(parts[0]).toBe(mockIdentity.publicKeyBase64url);
      expect(parseInt(parts[2])).toBeGreaterThan(0);
    });

    it("returns null when no work available (404)", async () => {
      mockResponse(404, { code: "NO_WORK_AVAILABLE" });

      const result = await client.requestWork({});
      expect(result).toBeNull();
    });

    it("throws on non-404 error", async () => {
      mockResponse(503, "service unavailable");

      await expect(client.requestWork({})).rejects.toThrow(
        "Request work failed: 503"
      );
    });

    it("sends correct default values for optional fields", async () => {
      mockResponse(200, {
        work_unit_id: "wu-2",
        leaf_id: "leaf-2",
        runtime: "WASM",
        deadline_seconds: 3600,
        execution_spec: {
          binaries: {},
          gpu_required: false,
          max_memory_mb: 4096,
          max_disk_mb: 51200,
          network_access: false,
        },
      });

      await client.requestWork({});

      const [, opts] = mockFetch.mock.calls[0];
      const body = JSON.parse(opts.body);
      expect(body.leaf_ids).toEqual([]);
      expect(body.max_memory_mb).toBe(4096);
      expect(body.max_disk_mb).toBe(51200);
      expect(body.has_gpu).toBe(false);
      expect(body.gpu_vendors).toEqual([]);
    });
  });

  describe("URL handling", () => {
    it("strips trailing slash from server URL", async () => {
      const trailingSlashClient = createVolunteerClient(
        "http://localhost:8080/",
        mockIdentity
      );

      mockResponse(201, {
        volunteer_id: "vol-2",
        registered_at: "2026-03-27T00:00:00Z",
      });

      await trailingSlashClient.register(mockHardware);

      const [url] = mockFetch.mock.calls[0];
      expect(url).toBe("http://localhost:8080/api/v1/volunteers/register");
      // Verify no double slash.
      expect(url).not.toContain("//api");
    });
  });

  describe("submitResult", () => {
    it("sends result with correctly formatted Ed25519 auth header", async () => {
      mockResponse(200, {
        accepted: true,
        validation_status: "VALIDATION_PENDING",
      });

      const result = await client.submitResult({
        work_unit_id: "wu-1",
        output_data: btoa("test output"),
        output_checksum: "a".repeat(64),
        exit_code: 0,
        metrics: {
          wall_clock_seconds: 120,
          cpu_seconds_user: 95.5,
          peak_memory_mb: 512,
        },
      });

      expect(result.accepted).toBe(true);

      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe(
        "http://localhost:8080/api/v1/volunteers/submit-result"
      );

      // Verify full Ed25519 auth header structure: pubkey:signature:timestamp.
      const authHeader = opts.headers.Authorization as string;
      expect(authHeader).toMatch(
        /^Ed25519 [A-Za-z0-9_-]+:[A-Za-z0-9_-]+:\d+$/
      );
      const parts = authHeader.replace("Ed25519 ", "").split(":");
      expect(parts).toHaveLength(3);
      expect(parts[0]).toBe(mockIdentity.publicKeyBase64url);
      expect(parseInt(parts[2])).toBeGreaterThan(0);

      const body = JSON.parse(opts.body);
      expect(body.work_unit_id).toBe("wu-1");
      expect(body.exit_code).toBe(0);
      expect(body.metrics.wall_clock_seconds).toBe(120);
    });

    it("throws on server error", async () => {
      mockResponse(500, "internal error");

      await expect(
        client.submitResult({
          work_unit_id: "wu-1",
          output_data: btoa("test"),
          output_checksum: "b".repeat(64),
          exit_code: 1,
          metrics: {
            wall_clock_seconds: 10,
            cpu_seconds_user: 8,
            peak_memory_mb: 64,
          },
        })
      ).rejects.toThrow("Submit result failed: 500");
    });
  });
});
