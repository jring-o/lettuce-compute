import { createVolunteerClient } from "../client";
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

function mockResponse(status: number, body: unknown) {
  mockFetch.mockResolvedValueOnce({
    ok: status >= 200 && status < 300,
    status,
    text: () => Promise.resolve(JSON.stringify(body)),
    json: () => Promise.resolve(body),
  });
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

      const result = await client.register({
        cpu_cores: 2,
        memory_mb: 4096,
        has_gpu: false,
        gpu_vendors: [],
        available_runtimes: ["WASM"],
      });

      expect(result.volunteer_id).toBe("existing-vol");
    });

    it("throws on non-409 error response", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        text: () => Promise.resolve("internal server error"),
        json: () => Promise.reject(new Error("not json")),
      });

      await expect(
        client.register({
          cpu_cores: 2,
          memory_mb: 4096,
          has_gpu: false,
          gpu_vendors: [],
          available_runtimes: ["WASM"],
        })
      ).rejects.toThrow("Registration failed: 500");
    });
  });

  describe("requestWork", () => {
    it("includes Ed25519 auth header", async () => {
      mockResponse(200, {
        work_unit_id: "wu-1",
        leaf_id: "leaf-1",
        runtime: "WASM",
        deadline_seconds: 3600,
        heartbeat_interval_seconds: 30,
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
      mockFetch.mockResolvedValueOnce({
        ok: false,
        status: 503,
        text: () => Promise.resolve("service unavailable"),
        json: () => Promise.reject(new Error("not json")),
      });

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
        heartbeat_interval_seconds: 30,
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

      await trailingSlashClient.register({
        cpu_cores: 2,
        memory_mb: 4096,
        has_gpu: false,
        gpu_vendors: [],
        available_runtimes: ["WASM"],
      });

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
      mockFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        text: () => Promise.resolve("internal error"),
        json: () => Promise.reject(new Error("not json")),
      });

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

  describe("heartbeat", () => {
    it("sends progress with correctly formatted Ed25519 auth", async () => {
      mockResponse(200, { continue_execution: true });

      const result = await client.heartbeat({
        work_unit_id: "wu-1",
        progress_pct: 45,
        metrics: {
          wall_clock_seconds: 60,
          cpu_seconds_user: 48.5,
          peak_memory_mb: 256,
        },
      });

      expect(result.continue_execution).toBe(true);

      const [url, opts] = mockFetch.mock.calls[0];
      expect(url).toBe("http://localhost:8080/api/v1/volunteers/heartbeat");

      // Verify full Ed25519 auth header structure on heartbeat too.
      const authHeader = opts.headers.Authorization as string;
      expect(authHeader).toMatch(
        /^Ed25519 [A-Za-z0-9_-]+:[A-Za-z0-9_-]+:\d+$/
      );
      const parts = authHeader.replace("Ed25519 ", "").split(":");
      expect(parts).toHaveLength(3);
      expect(parts[0]).toBe(mockIdentity.publicKeyBase64url);

      const body = JSON.parse(opts.body);
      expect(body.work_unit_id).toBe("wu-1");
      expect(body.progress_pct).toBe(45);
    });

    it("throws on server error", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        text: () => Promise.resolve("internal error"),
        json: () => Promise.reject(new Error("not json")),
      });

      await expect(
        client.heartbeat({
          work_unit_id: "wu-1",
          progress_pct: 0,
          metrics: {},
        })
      ).rejects.toThrow("Heartbeat failed: 500");
    });
  });
});
