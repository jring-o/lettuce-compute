import type { Leaf } from "@/types/infrastructure";

// --- Mocks ---

const mockAuth = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: () => mockAuth(),
}));

const mockClient = {
  createLeaf: jest.fn(),
  getLeaf: jest.fn(),
  listLeafs: jest.fn(),
  updateLeaf: jest.fn(),
  deleteLeaf: jest.fn(),
  activateLeaf: jest.fn(),
  pauseLeaf: jest.fn(),
  resumeLeaf: jest.fn(),
  archiveLeaf: jest.fn(),
  configureLeaf: jest.fn(),
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
  createLeaf,
  getLeaf,
  listMyLeafs,
  updateLeaf,
  deleteLeaf,
  activateLeaf,
  pauseLeaf,
  resumeLeaf,
  archiveLeaf,
  configureLeaf,
} from "@/lib/actions/projects";

// --- Fixtures ---

const authenticatedSession = {
  user: { id: "user-1", username: "alice", role: "USER" },
};

const mockLeaf: Leaf = {
  id: "leaf-1",
  name: "Test Leaf",
  slug: "test-leaf",
  description: "A test leaf",
  state: "DRAFT",
  task_pattern: "PARAMETER_SWEEP",
  research_area: "physics",
  creator_id: "user-1",
  execution_config: null,
  validation_config: null,
  fault_tolerance_config: null,
  data_config: null,
  credit_config: null,
  resource_requirements: null,
  is_ongoing: false,
  visibility: "PUBLIC",
  stats_cache_seconds: 60,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function makeFormData(data: Record<string, string>): FormData {
  const fd = new FormData();
  for (const [key, value] of Object.entries(data)) {
    fd.set(key, value);
  }
  return fd;
}

const adminSession = {
  user: { id: "admin-1", username: "admin", role: "ADMIN" },
};

beforeEach(() => {
  jest.clearAllMocks();
  // Default: ownership lookups (withOwnership) resolve to a leaf owned by
  // the standard authenticated user. Individual tests override as needed.
  mockClient.getLeaf.mockResolvedValue(mockLeaf);
});

describe("Leaf Server Actions", () => {
  // --- Authentication ---

  describe("authentication", () => {
    it("returns UNAUTHENTICATED when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await createLeaf(
        makeFormData({
          name: "Test",
          description: "Desc",
          task_pattern: "PARAMETER_SWEEP",
        }),
      );

      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
      expect(mockClient.createLeaf).not.toHaveBeenCalled();
    });

    it("returns UNAUTHENTICATED for getLeaf when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await getLeaf("leaf-1");
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
    });

    it("returns UNAUTHENTICATED for listMyLeafs when not signed in", async () => {
      mockAuth.mockResolvedValue(null);

      const result = await listMyLeafs();
      expect(result).toEqual({
        error: { code: "UNAUTHENTICATED", message: expect.any(String) },
      });
    });
  });

  // --- Authorization ---
  // The dashboard uses one shared service key, so it enforces per-user
  // ownership itself: it fetches the leaf and allows owner-or-ADMIN only.

  describe("authorization", () => {
    it("returns FORBIDDEN when a non-owner non-admin tries to update", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue({ ...mockLeaf, creator_id: "someone-else" });

      const result = await updateLeaf("leaf-1", { name: "Hacked" });
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockClient.updateLeaf).not.toHaveBeenCalled();
    });

    it("returns FORBIDDEN when a non-owner non-admin tries to delete", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue({ ...mockLeaf, creator_id: "someone-else" });

      const result = await deleteLeaf("leaf-1");
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
      expect(mockClient.deleteLeaf).not.toHaveBeenCalled();
    });

    it("fetches the leaf to verify ownership before mutating", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(mockLeaf); // owned by user-1
      mockClient.updateLeaf.mockResolvedValue({ ...mockLeaf, name: "Updated" });

      await updateLeaf("leaf-1", { name: "Updated" });
      expect(mockClient.getLeaf).toHaveBeenCalledWith("leaf-1");
    });

    it("allows the owner to update their own leaf", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(mockLeaf); // creator_id === user-1
      mockClient.updateLeaf.mockResolvedValue({ ...mockLeaf, name: "Updated" });

      const result = await updateLeaf("leaf-1", { name: "Updated" });
      expect(result).toEqual({
        data: expect.objectContaining({ name: "Updated" }),
      });
    });

    it("allows an ADMIN to update a leaf owned by someone else", async () => {
      mockAuth.mockResolvedValue(adminSession);
      mockClient.getLeaf.mockResolvedValue({ ...mockLeaf, creator_id: "someone-else" });
      mockClient.updateLeaf.mockResolvedValue({ ...mockLeaf, name: "Admin Edit" });

      const result = await updateLeaf("leaf-1", { name: "Admin Edit" });
      expect(result).toEqual({
        data: expect.objectContaining({ name: "Admin Edit" }),
      });
      expect(mockClient.updateLeaf).toHaveBeenCalled();
    });

    it("forwards a FORBIDDEN raised by infrastructure on the mutation itself", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(mockLeaf); // ownership passes
      const { InfrastructureApiError } = jest.requireMock("@/lib/infrastructure-client");
      mockClient.updateLeaf.mockRejectedValue(
        new InfrastructureApiError("FORBIDDEN", "infra denied", 403),
      );

      const result = await updateLeaf("leaf-1", { name: "x" });
      expect(result).toEqual({
        error: { code: "FORBIDDEN", message: expect.any(String) },
      });
    });
  });

  // --- Successful Flows ---

  describe("createLeaf", () => {
    it("creates leaf with creator_id from session", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.createLeaf.mockResolvedValue(mockLeaf);

      const result = await createLeaf(
        makeFormData({
          name: "Test Leaf",
          description: "A test leaf",
          task_pattern: "PARAMETER_SWEEP",
          research_area: "physics",
        }),
      );

      expect(result).toEqual({ data: mockLeaf });
      expect(mockClient.createLeaf).toHaveBeenCalledWith(
        expect.objectContaining({
          name: "Test Leaf",
          description: "A test leaf",
          task_pattern: "PARAMETER_SWEEP",
          research_area: "physics",
          creator_id: "user-1",
        }),
      );
    });

    it("returns validation error for missing name", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);

      const result = await createLeaf(
        makeFormData({
          name: "",
          description: "A valid description for testing",
          task_pattern: "PARAMETER_SWEEP",
        }),
      );

      expect(result).toEqual({
        error: { code: "VALIDATION_ERROR", message: expect.any(String) },
      });
      expect(mockClient.createLeaf).not.toHaveBeenCalled();
    });
  });

  describe("getLeaf", () => {
    it("returns leaf", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue(mockLeaf);

      const result = await getLeaf("leaf-1");
      expect(result).toEqual({ data: mockLeaf });
    });

    // --- Visibility / IDOR (H4) ---
    // The dashboard uses one shared service key, so the infra read returns ANY
    // leaf regardless of caller. getLeaf must enforce visibility itself:
    // PRIVATE leafs are readable only by their creator or an ADMIN; everyone
    // else gets NOT_FOUND (existence is never leaked). PUBLIC/UNLISTED stay
    // readable by any authenticated user.

    it("allows the owner to read their own PRIVATE leaf", async () => {
      mockAuth.mockResolvedValue(authenticatedSession); // user-1
      const privateOwned = {
        ...mockLeaf,
        visibility: "PRIVATE" as const,
        creator_id: "user-1",
      };
      mockClient.getLeaf.mockResolvedValue(privateOwned);

      const result = await getLeaf("leaf-1");
      expect(result).toEqual({ data: privateOwned });
    });

    it("allows an ADMIN to read any PRIVATE leaf", async () => {
      mockAuth.mockResolvedValue(adminSession);
      const privateOther = {
        ...mockLeaf,
        visibility: "PRIVATE" as const,
        creator_id: "someone-else",
      };
      mockClient.getLeaf.mockResolvedValue(privateOther);

      const result = await getLeaf("leaf-1");
      expect(result).toEqual({ data: privateOther });
    });

    it("returns NOT_FOUND when a non-owner non-admin reads a PRIVATE leaf", async () => {
      mockAuth.mockResolvedValue(authenticatedSession); // user-1
      mockClient.getLeaf.mockResolvedValue({
        ...mockLeaf,
        visibility: "PRIVATE",
        creator_id: "someone-else",
      });

      const result = await getLeaf("leaf-1");
      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: expect.any(String) },
      });
    });

    it("does not leak the PRIVATE leaf data to a non-owner non-admin", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockResolvedValue({
        ...mockLeaf,
        visibility: "PRIVATE",
        creator_id: "someone-else",
        description: "secret execution config",
      });

      const result = await getLeaf("leaf-1");
      expect("data" in result).toBe(false);
    });

    it("lets any authed user read a PUBLIC leaf they do not own", async () => {
      mockAuth.mockResolvedValue(authenticatedSession); // user-1
      const publicOther = {
        ...mockLeaf,
        visibility: "PUBLIC" as const,
        creator_id: "someone-else",
      };
      mockClient.getLeaf.mockResolvedValue(publicOther);

      const result = await getLeaf("leaf-1");
      expect(result).toEqual({ data: publicOther });
    });

    it("lets any authed user read an UNLISTED leaf they do not own", async () => {
      mockAuth.mockResolvedValue(authenticatedSession); // user-1
      const unlistedOther = {
        ...mockLeaf,
        visibility: "UNLISTED" as const,
        creator_id: "someone-else",
      };
      mockClient.getLeaf.mockResolvedValue(unlistedOther);

      const result = await getLeaf("leaf-1");
      expect(result).toEqual({ data: unlistedOther });
    });
  });

  describe("listMyLeafs", () => {
    it("lists leafs with creator_id from session", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const paginated = {
        data: [mockLeaf],
        pagination: { next_cursor: null, has_more: false },
      };
      mockClient.listLeafs.mockResolvedValue(paginated);

      const result = await listMyLeafs();
      expect(result).toEqual({ data: paginated });
      expect(mockClient.listLeafs).toHaveBeenCalledWith(
        expect.objectContaining({ creator_id: "user-1" }),
      );
    });

    it("forwards cursor parameter", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.listLeafs.mockResolvedValue({
        data: [],
        pagination: { next_cursor: null, has_more: false },
      });

      await listMyLeafs("cursor-abc");
      expect(mockClient.listLeafs).toHaveBeenCalledWith(
        expect.objectContaining({ cursor: "cursor-abc" }),
      );
    });
  });

  describe("updateLeaf", () => {
    it("updates leaf when authenticated", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.updateLeaf.mockResolvedValue({
        ...mockLeaf,
        name: "Updated",
      });

      const result = await updateLeaf("leaf-1", { name: "Updated" });
      expect(result).toEqual({
        data: expect.objectContaining({ name: "Updated" }),
      });
    });
  });

  describe("deleteLeaf", () => {
    it("deletes leaf when authenticated", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.deleteLeaf.mockResolvedValue(undefined);

      const result = await deleteLeaf("leaf-1");
      expect(result).toEqual({ data: undefined });
    });
  });

  describe("state transitions", () => {
    it.each([
      ["activateLeaf", activateLeaf, "activateLeaf"],
      ["pauseLeaf", pauseLeaf, "pauseLeaf"],
      ["resumeLeaf", resumeLeaf, "resumeLeaf"],
      ["archiveLeaf", archiveLeaf, "archiveLeaf"],
      ["configureLeaf", configureLeaf, "configureLeaf"],
    ] as const)("%s succeeds when authenticated", async (_name, action, clientMethod) => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const updatedLeaf = { ...mockLeaf, state: "ACTIVE" };
      mockClient[clientMethod].mockResolvedValue(updatedLeaf);

      const result = await action("leaf-1");
      expect(result).toEqual({ data: updatedLeaf });
    });
  });

  // --- Validation Edge Cases ---

  describe("createLeaf validation", () => {
    it("returns validation error for missing description", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);

      const result = await createLeaf(
        makeFormData({
          name: "Test",
          description: "",
          task_pattern: "PARAMETER_SWEEP",
        }),
      );

      expect(result).toEqual({
        error: { code: "VALIDATION_ERROR", message: expect.any(String) },
      });
      expect(mockClient.createLeaf).not.toHaveBeenCalled();
    });

    it("returns validation error for invalid task_pattern", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);

      const result = await createLeaf(
        makeFormData({
          name: "Test",
          description: "A valid leaf description",
          task_pattern: "INVALID_PATTERN",
        }),
      );

      expect(result).toEqual({
        error: { code: "VALIDATION_ERROR", message: expect.any(String) },
      });
      expect(mockClient.createLeaf).not.toHaveBeenCalled();
    });

    it("returns validation error for missing task_pattern", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);

      const result = await createLeaf(
        makeFormData({
          name: "Test",
          description: "A valid leaf description",
        }),
      );

      expect(result).toEqual({
        error: { code: "VALIDATION_ERROR", message: expect.any(String) },
      });
      expect(mockClient.createLeaf).not.toHaveBeenCalled();
    });

    it("transforms boolean string fields correctly", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.createLeaf.mockResolvedValue(mockLeaf);

      await createLeaf(
        makeFormData({
          name: "Test",
          description: "A valid leaf description",
          task_pattern: "PARAMETER_SWEEP",
          is_ongoing: "true",
        }),
      );

      expect(mockClient.createLeaf).toHaveBeenCalledWith(
        expect.objectContaining({
          is_ongoing: true,
        }),
      );
    });

    it("returns validation error for name exceeding 100 characters", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);

      const result = await createLeaf(
        makeFormData({
          name: "a".repeat(101),
          description: "A valid leaf description",
          task_pattern: "PARAMETER_SWEEP",
        }),
      );

      expect(result).toEqual({
        error: { code: "VALIDATION_ERROR", message: expect.any(String) },
      });
      expect(mockClient.createLeaf).not.toHaveBeenCalled();
    });
  });

  // --- Infrastructure Error Paths ---

  describe("infrastructure error handling", () => {
    it("maps InfrastructureApiError on createLeaf failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const { InfrastructureApiError } = jest.requireMock("@/lib/infrastructure-client");
      mockClient.createLeaf.mockRejectedValue(
        new InfrastructureApiError("DUPLICATE_NAME", "Leaf name already exists", 409),
      );

      const result = await createLeaf(
        makeFormData({
          name: "Duplicate",
          description: "A valid leaf description",
          task_pattern: "PARAMETER_SWEEP",
        }),
      );

      expect(result).toEqual({
        error: { code: "DUPLICATE_NAME", message: "Leaf name already exists" },
      });
    });

    it("maps generic error on createLeaf failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.createLeaf.mockRejectedValue(new Error("Network failure"));

      const result = await createLeaf(
        makeFormData({
          name: "Test",
          description: "A valid leaf description",
          task_pattern: "PARAMETER_SWEEP",
        }),
      );

      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });

    it("maps InfrastructureApiError on getLeaf failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const { InfrastructureApiError } = jest.requireMock("@/lib/infrastructure-client");
      mockClient.getLeaf.mockRejectedValue(
        new InfrastructureApiError("NOT_FOUND", "Leaf not found", 404),
      );

      const result = await getLeaf("missing-leaf");

      expect(result).toEqual({
        error: { code: "NOT_FOUND", message: "Leaf not found" },
      });
    });

    it("maps generic error on getLeaf failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.getLeaf.mockRejectedValue(new Error("Connection refused"));

      const result = await getLeaf("leaf-1");

      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });

    it("maps InfrastructureApiError on listMyLeafs failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const { InfrastructureApiError } = jest.requireMock("@/lib/infrastructure-client");
      mockClient.listLeafs.mockRejectedValue(
        new InfrastructureApiError("INTERNAL_ERROR", "Service unavailable", 503),
      );

      const result = await listMyLeafs();

      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "Service unavailable" },
      });
    });

    it("maps generic error on listMyLeafs failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.listLeafs.mockRejectedValue(new Error("Timeout"));

      const result = await listMyLeafs();

      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });

    it("maps generic error on updateLeaf failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      mockClient.updateLeaf.mockRejectedValue(new Error("Network failure"));

      const result = await updateLeaf("leaf-1", { name: "New" });

      expect(result).toEqual({
        error: { code: "INTERNAL_ERROR", message: "An unexpected error occurred." },
      });
    });

    it("maps InfrastructureApiError on updateLeaf call failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const { InfrastructureApiError } = jest.requireMock("@/lib/infrastructure-client");
      mockClient.updateLeaf.mockRejectedValue(
        new InfrastructureApiError("STATE_CONFLICT", "Cannot update active leaf", 409),
      );

      const result = await updateLeaf("leaf-1", { name: "New" });

      expect(result).toEqual({
        error: { code: "STATE_CONFLICT", message: "Cannot update active leaf" },
      });
    });

    it("maps InfrastructureApiError on deleteLeaf call failure", async () => {
      mockAuth.mockResolvedValue(authenticatedSession);
      const { InfrastructureApiError } = jest.requireMock("@/lib/infrastructure-client");
      mockClient.deleteLeaf.mockRejectedValue(
        new InfrastructureApiError("STATE_CONFLICT", "Cannot delete active leaf", 409),
      );

      const result = await deleteLeaf("leaf-1");

      expect(result).toEqual({
        error: { code: "STATE_CONFLICT", message: "Cannot delete active leaf" },
      });
    });
  });
});
