// --- Mocks ---

const mockOrderBy = jest.fn().mockResolvedValue([
  { id: "ra-1", slug: "physics", name: "Physics", description: "Physics research", displayOrder: 1 },
  { id: "ra-2", slug: "biology", name: "Biology", description: "Biology research", displayOrder: 2 },
  { id: "ra-3", slug: "climate-science", name: "Climate Science", description: null, displayOrder: 3 },
]);
const mockFrom = jest.fn().mockReturnValue({ orderBy: mockOrderBy });
const mockSelect = jest.fn().mockReturnValue({ from: mockFrom });

jest.mock("@/lib/db", () => ({
  db: {
    select: (...args: unknown[]) => mockSelect(...args),
  },
}));

jest.mock("@/lib/db/schema", () => ({
  researchAreas: {
    displayOrder: "display_order_column",
  },
}));

jest.mock("drizzle-orm", () => ({
  asc: jest.fn((col: unknown) => col),
}));

import { listResearchAreas } from "@/lib/actions/research-areas";

beforeEach(() => {
  jest.clearAllMocks();
  // Restore default chain after clearing
  mockSelect.mockReturnValue({ from: mockFrom });
  mockFrom.mockReturnValue({ orderBy: mockOrderBy });
});

describe("Research Areas Server Action", () => {
  describe("listResearchAreas", () => {
    it("returns research areas ordered by displayOrder", async () => {
      const result = await listResearchAreas();

      expect(result).toHaveLength(3);
      expect(result[0].slug).toBe("physics");
      expect(result[1].slug).toBe("biology");
      expect(result[2].slug).toBe("climate-science");
    });

    it("calls db.select().from(researchAreas).orderBy()", async () => {
      await listResearchAreas();

      expect(mockSelect).toHaveBeenCalled();
      expect(mockFrom).toHaveBeenCalled();
      expect(mockOrderBy).toHaveBeenCalled();
    });

    it("returns empty array when no research areas exist", async () => {
      mockOrderBy.mockResolvedValueOnce([]);

      const result = await listResearchAreas();
      expect(result).toEqual([]);
    });

    it("includes all expected fields", async () => {
      const result = await listResearchAreas();

      const firstArea = result[0];
      expect(firstArea).toEqual({
        id: "ra-1",
        slug: "physics",
        name: "Physics",
        description: "Physics research",
        displayOrder: 1,
      });
    });

    it("handles null description", async () => {
      const result = await listResearchAreas();

      const climateArea = result[2];
      expect(climateArea.description).toBeNull();
    });

    it("propagates database errors to caller", async () => {
      mockOrderBy.mockRejectedValueOnce(new Error("Connection refused"));

      await expect(listResearchAreas()).rejects.toThrow("Connection refused");
    });
  });
});
