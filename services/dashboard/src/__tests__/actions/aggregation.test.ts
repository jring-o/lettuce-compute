import type { AggregationResult } from "@/types/infrastructure";

// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockClient = {
  getLeaf: jest.fn(),
  triggerAggregation: jest.fn(),
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

import { triggerLeafAggregation } from "@/lib/actions/aggregation";
import { InfrastructureApiError } from "@/lib/infrastructure-client";

// --- Fixtures ---

const authenticatedSession = {
  user: { id: "user-1", username: "alice", role: "USER" },
};

const adminSession = {
  user: { id: "admin-1", username: "admin", role: "ADMIN" },
};

// withOwnership reads creator_id off the leaf.
const ownedLeaf = { id: "leaf-1", creator_id: "user-1" };
const othersLeaf = { id: "leaf-1", creator_id: "someone-else" };

const mockAggregation: AggregationResult = {
  status: "complete",
  format: "json",
  result: { mean: 42 },
  work_units_aggregated: 10,
  work_units_total: 10,
  aggregated_at: "2026-01-01T00:00:00Z",
};

beforeEach(() => {
  jest.clearAllMocks();
  mockAuth.mockResolvedValue(authenticatedSession);
  mockClient.getLeaf.mockResolvedValue(ownedLeaf);
});

describe("Aggregation Server Actions", () => {
  describe("triggerLeafAggregation", () => {
    it("returns UNAUTHENTICATED when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await triggerLeafAggregation("leaf-1");
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
      expect(mockClient.triggerAggregation).not.toHaveBeenCalled();
    });

    it("returns FORBIDDEN and skips aggregation for a non-owner non-admin", async () => {
      mockClient.getLeaf.mockResolvedValue(othersLeaf);

      const result = await triggerLeafAggregation("leaf-1");
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockClient.triggerAggregation).not.toHaveBeenCalled();
    });

    it("aggregates for the owner and checks ownership first", async () => {
      mockClient.triggerAggregation.mockResolvedValue({ data: mockAggregation });

      const result = await triggerLeafAggregation("leaf-1");
      expect(result).toEqual({ data: mockAggregation });
      expect(mockClient.getLeaf).toHaveBeenCalledWith("leaf-1");
      expect(mockClient.triggerAggregation).toHaveBeenCalledWith("leaf-1");
    });

    it("allows an ADMIN to aggregate another user's leaf", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockClient.getLeaf.mockResolvedValue(othersLeaf);
      mockClient.triggerAggregation.mockResolvedValue({ data: mockAggregation });

      const result = await triggerLeafAggregation("leaf-1");
      expect(result).toEqual({ data: mockAggregation });
      expect(mockClient.triggerAggregation).toHaveBeenCalled();
    });

    it("maps InfrastructureApiError from the aggregation call", async () => {
      mockClient.triggerAggregation.mockRejectedValue(
        new InfrastructureApiError("CONFLICT", "no validated work units", 409),
      );

      const result = await triggerLeafAggregation("leaf-1");
      expect(result).toEqual({
        error: { code: "CONFLICT", message: "no validated work units" },
      });
    });

    it("maps generic errors to INTERNAL_ERROR", async () => {
      mockClient.triggerAggregation.mockRejectedValue(new Error("boom"));

      const result = await triggerLeafAggregation("leaf-1");
      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });
  });
});
