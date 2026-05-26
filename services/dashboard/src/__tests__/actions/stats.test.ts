import type { LeafStats } from "@/types/infrastructure";

// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockClient = {
  getLeaf: jest.fn(),
  getLeafStats: jest.fn(),
  getLeafStatsHistory: jest.fn(),
};

jest.mock("@/lib/infrastructure-client", () => ({
  infrastructureClient: mockClient,
  InfrastructureApiError: class extends Error {
    code: string;
    status: number;
    details?: Record<string, unknown>;
    constructor(
      code: string,
      message: string,
      status: number,
      details?: Record<string, unknown>,
    ) {
      super(message);
      this.code = code;
      this.status = status;
      this.details = details;
    }
  },
}));

import {
  getLeafStats,
  getLeafStatsHistory,
} from "@/lib/actions/stats";
import { InfrastructureApiError } from "@/lib/infrastructure-client";

// --- Fixtures ---

const mockStats: LeafStats = {
  id: "00000000-0000-0000-0000-000000000001",
  leaf_id: "leaf-1",
  snapshot_at: "2026-01-01T00:00:00Z",
  total_work_units: 1000,
  work_units_queued: 500,
  work_units_assigned: 100,
  work_units_running: 200,
  work_units_completed: 150,
  work_units_validated: 150,
  work_units_failed: 30,
  active_volunteers: 50,
  total_credit_granted: 5000,
  avg_completion_seconds: 120,
  agreement_rate: 0.97,
  throughput_per_hour: 75,
  created_at: "2026-01-01T00:00:00Z",
};

const authenticatedSession = {
  user: { id: "user-1", username: "alice", role: "USER" },
};

const adminSession = {
  user: { id: "admin-1", username: "admin", role: "ADMIN" },
};

// withOwnership/assertLeafOwnership reads creator_id off the leaf.
const ownedLeaf = { id: "leaf-1", creator_id: "user-1" };
const othersLeaf = { id: "leaf-1", creator_id: "someone-else" };

beforeEach(() => {
  jest.clearAllMocks();
  // Default: authenticated owner.
  mockAuth.mockResolvedValue(authenticatedSession);
  mockClient.getLeaf.mockResolvedValue(ownedLeaf);
});

describe("Stats Server Actions", () => {
  // --- Authorization ---
  // These actions are owner-gated: the dashboard does its own per-user check
  // because it talks to infrastructure with one shared service key.

  describe("authorization", () => {
    it("returns UNAUTHENTICATED for getLeafStats when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await getLeafStats("leaf-1");
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
      expect(mockClient.getLeafStats).not.toHaveBeenCalled();
    });

    it("returns UNAUTHENTICATED for getLeafStatsHistory when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await getLeafStatsHistory("leaf-1", { from: "2026-01-01" });
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
      expect(mockClient.getLeafStatsHistory).not.toHaveBeenCalled();
    });

    it("returns FORBIDDEN and skips getLeafStats for a non-owner non-admin", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(othersLeaf);

      const result = await getLeafStats("leaf-1");
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockClient.getLeafStats).not.toHaveBeenCalled();
    });

    it("returns FORBIDDEN and skips getLeafStatsHistory for a non-owner non-admin", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(othersLeaf);

      const result = await getLeafStatsHistory("leaf-1", { from: "2026-01-01" });
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockClient.getLeafStatsHistory).not.toHaveBeenCalled();
    });

    it("allows an ADMIN to read stats for another user's leaf", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockClient.getLeaf.mockResolvedValue(othersLeaf);
      mockClient.getLeafStats.mockResolvedValue(mockStats);

      const result = await getLeafStats("leaf-1");
      expect(result).toEqual({ data: mockStats });
    });
  });

  describe("getLeafStats", () => {
    it("returns stats for the owner and checks ownership first", async () => {
      mockClient.getLeafStats.mockResolvedValue(mockStats);

      const result = await getLeafStats("leaf-1");
      expect(result).toEqual({ data: mockStats });
      expect(mockClient.getLeaf).toHaveBeenCalledWith("leaf-1");
      expect(mockClient.getLeafStats).toHaveBeenCalledWith("leaf-1");
    });

    it("maps InfrastructureApiError", async () => {
      mockClient.getLeafStats.mockRejectedValue(
        new InfrastructureApiError("NOT_FOUND", "Leaf not found", 404),
      );

      const result = await getLeafStats("leaf-1");
      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: "Leaf not found" },
      });
    });

    it("maps generic errors to INTERNAL_ERROR", async () => {
      mockClient.getLeafStats.mockRejectedValue(new Error("Network failure"));

      const result = await getLeafStats("leaf-1");
      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });
  });

  describe("getLeafStatsHistory", () => {
    it("returns stats history for the owner", async () => {
      const historyResponse = { data: [mockStats] };
      mockClient.getLeafStatsHistory.mockResolvedValue(historyResponse);

      const params = {
        from: "2026-01-01T00:00:00Z",
        to: "2026-01-02T00:00:00Z",
        interval: "daily" as const,
      };
      const result = await getLeafStatsHistory("leaf-1", params);

      expect(result).toEqual({ data: historyResponse });
      expect(mockClient.getLeafStatsHistory).toHaveBeenCalledWith(
        "leaf-1",
        params,
      );
    });

    it("works with only required from parameter", async () => {
      const historyResponse = { data: [] };
      mockClient.getLeafStatsHistory.mockResolvedValue(historyResponse);

      const params = { from: "2026-01-01T00:00:00Z" };
      const result = await getLeafStatsHistory("leaf-1", params);

      expect(result).toEqual({ data: historyResponse });
      expect(mockClient.getLeafStatsHistory).toHaveBeenCalledWith(
        "leaf-1",
        params,
      );
    });

    it("maps InfrastructureApiError for history", async () => {
      mockClient.getLeafStatsHistory.mockRejectedValue(
        new InfrastructureApiError(
          "INVALID_PARAMETER",
          "Invalid date range",
          400,
        ),
      );

      const result = await getLeafStatsHistory("leaf-1", {
        from: "invalid",
      });
      expect(result).toEqual({
        error: { code: "INVALID_PARAMETER", message: "Invalid date range" },
      });
    });

    it("maps generic errors to INTERNAL_ERROR for history", async () => {
      mockClient.getLeafStatsHistory.mockRejectedValue(
        new Error("Connection refused"),
      );

      const result = await getLeafStatsHistory("leaf-1", {
        from: "2026-01-01T00:00:00Z",
      });
      expect(result).toEqual({
        error: {
          code: "INTERNAL_ERROR",
          message: "An unexpected error occurred.",
        },
      });
    });
  });
});
