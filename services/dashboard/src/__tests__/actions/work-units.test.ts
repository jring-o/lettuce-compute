import type {
  GenerateWorkUnitsResponse,
  PaginatedResponse,
  WorkUnit,
  WorkUnitSummary,
} from "@/types/infrastructure";

// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockClient = {
  getLeaf: jest.fn(),
  listWorkUnits: jest.fn(),
  getWorkUnit: jest.fn(),
  generateWorkUnits: jest.fn(),
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
  listWorkUnits,
  getWorkUnit,
  generateWorkUnits,
} from "@/lib/actions/work-units";
import { InfrastructureApiError } from "@/lib/infrastructure-client";

// --- Fixtures ---

const authenticatedSession = {
  user: { id: "user-1", username: "alice", role: "USER" },
};

const adminSession = {
  user: { id: "admin-1", username: "admin", role: "ADMIN" },
};

// withOwnership reads creator_id off the leaf; user-1 owns this one.
const ownedLeaf = { id: "leaf-1", creator_id: "user-1" };
const othersLeaf = { id: "leaf-1", creator_id: "someone-else" };

const mockWorkUnitSummary: WorkUnitSummary = {
  id: "wu-1",
  leaf_id: "leaf-1",
  batch_id: "batch-1",
  state: "PENDING",
  priority: "NORMAL",
  assigned_to: null,
  attempts: 0,
  flagged_for_review: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

const mockWorkUnit: WorkUnit = {
  id: "wu-1",
  leaf_id: "leaf-1",
  batch_id: "batch-1",
  state: "COMPLETED",
  priority: "NORMAL",
  parameters: { x: 1, y: 2 },
  input_data_url: null,
  result_data_url: null,
  assigned_to: "volunteer-1",
  assigned_at: "2026-01-01T00:00:00Z",
  started_at: "2026-01-01T00:00:30Z",
  completed_at: "2026-01-01T00:01:00Z",
  deadline: null,
  attempts: 1,
  max_attempts: 3,
  error_message: null,
  flagged_for_review: false,
  credit_awarded: 10,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:01:00Z",
};

beforeEach(() => {
  jest.clearAllMocks();
  // Default: ownership lookups resolve to a leaf owned by the test user.
  mockClient.getLeaf.mockResolvedValue(ownedLeaf);
});

describe("Work Unit Server Actions", () => {
  // --- Authentication ---

  describe("authentication", () => {
    it("returns UNAUTHENTICATED for listWorkUnits when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await listWorkUnits("leaf-1");
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: "You must be signed in." },
      });
      expect(mockClient.listWorkUnits).not.toHaveBeenCalled();
    });

    it("returns UNAUTHENTICATED for getWorkUnit when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await getWorkUnit("leaf-1", "wu-1");
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: "You must be signed in." },
      });
      expect(mockClient.getWorkUnit).not.toHaveBeenCalled();
    });

    it("returns UNAUTHENTICATED for generateWorkUnits when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await generateWorkUnits("leaf-1");
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: "You must be signed in." },
      });
      expect(mockClient.generateWorkUnits).not.toHaveBeenCalled();
    });

    it("returns UNAUTHENTICATED when session has no user id", async () => {
      mockAuth.mockResolvedValue({ user: {} });

      const result = await listWorkUnits("leaf-1");
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: "You must be signed in." },
      });
    });
  });

  // --- Authorization ---
  // The dashboard enforces per-user ownership itself (shared service key):
  // it fetches the leaf and allows owner-or-ADMIN only.

  describe("authorization", () => {
    it("returns FORBIDDEN and skips listWorkUnits for a non-owner non-admin", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(othersLeaf);

      const result = await listWorkUnits("leaf-1");
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockClient.listWorkUnits).not.toHaveBeenCalled();
    });

    it("returns FORBIDDEN and skips getWorkUnit for a non-owner non-admin", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(othersLeaf);

      const result = await getWorkUnit("leaf-1", "wu-1");
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockClient.getWorkUnit).not.toHaveBeenCalled();
    });

    it("returns FORBIDDEN and skips generateWorkUnits for a non-owner non-admin", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(othersLeaf);

      const result = await generateWorkUnits("leaf-1");
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockClient.generateWorkUnits).not.toHaveBeenCalled();
    });

    it("allows the owner to list work units (and fetches the leaf to check)", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(ownedLeaf);
      mockClient.listWorkUnits.mockResolvedValue({
        data: [mockWorkUnitSummary],
        pagination: { next_cursor: null, has_more: false },
      });

      const result = await listWorkUnits("leaf-1");
      expect("data" in result).toBe(true);
      expect(mockClient.getLeaf).toHaveBeenCalledWith("leaf-1");
      expect(mockClient.listWorkUnits).toHaveBeenCalled();
    });

    it("allows an ADMIN to generate work units on another user's leaf", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockClient.getLeaf.mockResolvedValue(othersLeaf);
      mockClient.generateWorkUnits.mockResolvedValue({
        batch_id: "b1",
        work_units_created: 10,
        status: "complete",
      });

      const result = await generateWorkUnits("leaf-1");
      expect("data" in result).toBe(true);
      expect(mockClient.generateWorkUnits).toHaveBeenCalled();
    });

    it("forwards a FORBIDDEN raised by infrastructure on the work-unit call itself", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(ownedLeaf); // ownership passes
      mockClient.listWorkUnits.mockRejectedValue(
        new InfrastructureApiError("FORBIDDEN", "infra denied", 403),
      );

      const result = await listWorkUnits("leaf-1");
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
    });
  });

  // --- Successful Flows ---

  describe("listWorkUnits", () => {
    it("returns paginated work unit list", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const paginated: PaginatedResponse<WorkUnitSummary> = {
        data: [mockWorkUnitSummary],
        pagination: { next_cursor: "next-cursor", has_more: true },
      };
      mockClient.listWorkUnits.mockResolvedValue(paginated);

      const result = await listWorkUnits("leaf-1");
      expect(result).toEqual({ data: paginated });
      expect(mockClient.listWorkUnits).toHaveBeenCalledWith("leaf-1", undefined);
    });

    it("forwards optional params to infrastructure client", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const paginated: PaginatedResponse<WorkUnitSummary> = {
        data: [],
        pagination: { next_cursor: null, has_more: false },
      };
      mockClient.listWorkUnits.mockResolvedValue(paginated);

      const params = { state: "COMPLETED" as const, limit: 25, cursor: "abc" };
      await listWorkUnits("leaf-1", params);

      expect(mockClient.listWorkUnits).toHaveBeenCalledWith("leaf-1", params);
    });
  });

  describe("getWorkUnit", () => {
    it("returns full work unit", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getWorkUnit.mockResolvedValue(mockWorkUnit);

      const result = await getWorkUnit("leaf-1", "wu-1");
      expect(result).toEqual({ data: mockWorkUnit });
      expect(mockClient.getWorkUnit).toHaveBeenCalledWith("leaf-1", "wu-1");
    });
  });

  describe("generateWorkUnits", () => {
    it("generates work units", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const response: GenerateWorkUnitsResponse = {
        batch_id: "batch-new",
        work_units_created: 50,
        status: "complete",
      };
      mockClient.generateWorkUnits.mockResolvedValue(response);

      const result = await generateWorkUnits("leaf-1", { batch_size: 50 });
      expect(result).toEqual({ data: response });
      expect(mockClient.generateWorkUnits).toHaveBeenCalledWith("leaf-1", {
        batch_size: 50,
      });
    });

    it("generates work units without optional data", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const response: GenerateWorkUnitsResponse = {
        batch_id: "batch-default",
        work_units_created: 100,
        status: "complete",
      };
      mockClient.generateWorkUnits.mockResolvedValue(response);

      const result = await generateWorkUnits("leaf-1");
      expect(result).toEqual({ data: response });
      expect(mockClient.generateWorkUnits).toHaveBeenCalledWith(
        "leaf-1",
        undefined,
      );
    });
  });

  // --- Error Handling ---

  describe("error handling", () => {
    it("maps InfrastructureApiError for listWorkUnits", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.listWorkUnits.mockRejectedValue(
        new InfrastructureApiError("NOT_FOUND", "Leaf not found", 404),
      );

      const result = await listWorkUnits("leaf-1");
      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: "Leaf not found" },
      });
    });

    it("maps generic errors to INTERNAL_ERROR", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getWorkUnit.mockRejectedValue(new Error("Network failure"));

      const result = await getWorkUnit("leaf-1", "wu-1");
      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });
  });
});
