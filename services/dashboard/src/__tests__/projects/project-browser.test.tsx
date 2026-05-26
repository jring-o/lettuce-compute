import { render, screen, fireEvent, waitFor, act } from "@testing-library/react";
import { ProjectBrowser } from "@/components/projects/project-browser";
import type { LeafWithStats } from "@/lib/actions/public-projects";
import type { Pagination, LeafStats } from "@/types/infrastructure";

// Mock next/navigation
const mockPush = jest.fn();
const mockSearchParams = new URLSearchParams();
jest.mock("next/navigation", () => ({
  useRouter: () => ({ push: mockPush }),
  useSearchParams: () => mockSearchParams,
}));

// Mock the server action
jest.mock("@/lib/actions/public-projects", () => ({
  listPublicLeafs: jest.fn(),
}));

const { listPublicLeafs } = jest.requireMock("@/lib/actions/public-projects");

const baseStats: LeafStats = {
  id: "00000000-0000-0000-0000-000000000001",
  leaf_id: "p1",
  snapshot_at: "2026-03-14T00:00:00Z",
  total_work_units: 100,
  work_units_queued: 20,
  work_units_assigned: 10,
  work_units_running: 10,
  work_units_completed: 60,
  work_units_validated: 60,
  work_units_failed: 0,
  active_volunteers: 5,
  total_credit_granted: 500,
  avg_completion_seconds: 60,
  agreement_rate: 1.0,
  throughput_per_hour: 10,
  created_at: "2026-03-14T00:00:00Z",
};

function makeLeaf(overrides: Partial<LeafWithStats> = {}): LeafWithStats {
  return {
    id: "p1",
    name: "Test Leaf",
    slug: "test-leaf",
    description: "A test leaf description.",
    research_area: "Climate Science",
    state: "ACTIVE",
    task_pattern: "PARAMETER_SWEEP",
    resource_requirements: null,
    runtime: "CONTAINER" as const,
    is_ongoing: false,
    visibility: "PUBLIC",
    stats_cache_seconds: 60,
    active_volunteers: 0,
    progress_pct: null,
    created_at: "2026-01-01T00:00:00Z",
    stats: baseStats,
    ...overrides,
  };
}

const researchAreas = [
  { id: "ra1", slug: "climate-science", name: "Climate Science", description: null },
  { id: "ra2", slug: "genomics", name: "Genomics", description: null },
];

const defaultPagination: Pagination = {
  next_cursor: null,
  has_more: false,
};

describe("ProjectBrowser", () => {
  beforeEach(() => {
    jest.clearAllMocks();
    jest.useFakeTimers();
  });

  afterEach(() => {
    jest.useRealTimers();
  });

  it("renders grid of leaf cards", () => {
    const leafs = [
      makeLeaf({ id: "p1", name: "Leaf One", slug: "leaf-one" }),
      makeLeaf({ id: "p2", name: "Leaf Two", slug: "leaf-two" }),
    ];
    render(
      <ProjectBrowser
        initialLeafs={leafs}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );
    expect(screen.getByTestId("leaf-grid")).toBeInTheDocument();
    expect(screen.getByText("Leaf One")).toBeInTheDocument();
    expect(screen.getByText("Leaf Two")).toBeInTheDocument();
  });

  it("shows empty state when no leafs", () => {
    render(
      <ProjectBrowser
        initialLeafs={[]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );
    expect(screen.getByTestId("empty-state")).toBeInTheDocument();
    expect(
      screen.getByText("No active leafs yet."),
    ).toBeInTheDocument();
  });

  it("shows Load More button when has_more is true", () => {
    const leafs = [makeLeaf()];
    const pagination: Pagination = {
      next_cursor: "cursor123",
      has_more: true,
    };
    render(
      <ProjectBrowser
        initialLeafs={leafs}
        initialPagination={pagination}
        researchAreas={researchAreas}
      />,
    );
    expect(screen.getByTestId("load-more-button")).toBeInTheDocument();
    expect(screen.getByText("Load More Leafs")).toBeInTheDocument();
  });

  it("hides Load More button when has_more is false", () => {
    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );
    expect(screen.queryByTestId("load-more-button")).not.toBeInTheDocument();
  });

  it("loads more leafs when Load More clicked", async () => {
    const newLeaf = makeLeaf({ id: "p2", name: "Loaded Leaf", slug: "loaded" });
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      data: {
        leafs: [newLeaf],
        pagination: { next_cursor: null, has_more: false },
      },
    });

    const pagination: Pagination = {
      next_cursor: "cursor123",
      has_more: true,
    };
    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={pagination}
        researchAreas={researchAreas}
      />,
    );

    await act(async () => {
      fireEvent.click(screen.getByTestId("load-more-button"));
    });

    await waitFor(() => {
      expect(screen.getByText("Loaded Leaf")).toBeInTheDocument();
    });
  });

  it("renders search, filter, and sort controls", () => {
    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );
    expect(screen.getByTestId("browser-controls")).toBeInTheDocument();
    expect(screen.getByTestId("search-input")).toBeInTheDocument();
    expect(screen.getByTestId("research-area-filter")).toBeInTheDocument();
    expect(screen.getByTestId("sort-select")).toBeInTheDocument();
  });

  it("appends loaded leafs to existing list after Load More", async () => {
    const originalLeaf = makeLeaf({ id: "p1", name: "Original", slug: "original" });
    const loadedLeaf = makeLeaf({ id: "p2", name: "Loaded", slug: "loaded" });
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      data: {
        leafs: [loadedLeaf],
        pagination: { next_cursor: null, has_more: false },
      },
    });

    const pagination: Pagination = {
      next_cursor: "cursor-1",
      has_more: true,
    };
    render(
      <ProjectBrowser
        initialLeafs={[originalLeaf]}
        initialPagination={pagination}
        researchAreas={researchAreas}
      />,
    );

    await act(async () => {
      fireEvent.click(screen.getByTestId("load-more-button"));
    });

    await waitFor(() => {
      // Both original and loaded should be present
      expect(screen.getByText("Original")).toBeInTheDocument();
      expect(screen.getByText("Loaded")).toBeInTheDocument();
    });
  });

  it("hides Load More button after loading final page", async () => {
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      data: {
        leafs: [makeLeaf({ id: "p2", name: "Last Page", slug: "last-page" })],
        pagination: { next_cursor: null, has_more: false },
      },
    });

    const pagination: Pagination = {
      next_cursor: "cursor-1",
      has_more: true,
    };
    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={pagination}
        researchAreas={researchAreas}
      />,
    );

    expect(screen.getByTestId("load-more-button")).toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByTestId("load-more-button"));
    });

    await waitFor(() => {
      expect(screen.queryByTestId("load-more-button")).not.toBeInTheDocument();
    });
  });

  it("handles error from Load More gracefully", async () => {
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      error: { code: "INTERNAL_ERROR", message: "Service unavailable" },
    });

    const pagination: Pagination = {
      next_cursor: "cursor-1",
      has_more: true,
    };
    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={pagination}
        researchAreas={researchAreas}
      />,
    );

    await act(async () => {
      fireEvent.click(screen.getByTestId("load-more-button"));
    });

    // Original leaf should still be displayed
    await waitFor(() => {
      expect(screen.getByText("Test Leaf")).toBeInTheDocument();
    });
  });

  it("passes cursor to listPublicLeafs on Load More", async () => {
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      data: {
        leafs: [],
        pagination: { next_cursor: null, has_more: false },
      },
    });

    const pagination: Pagination = {
      next_cursor: "my-cursor-123",
      has_more: true,
    };
    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={pagination}
        researchAreas={researchAreas}
      />,
    );

    await act(async () => {
      fireEvent.click(screen.getByTestId("load-more-button"));
    });

    expect(listPublicLeafs).toHaveBeenCalledWith(
      expect.objectContaining({ cursor: "my-cursor-123" }),
    );
  });

  it("renders empty state message when no leafs", () => {
    render(
      <ProjectBrowser
        initialLeafs={[]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );
    expect(screen.getByText("No active leafs yet.")).toBeInTheDocument();
  });

  it("shows empty-search state when no leafs but search filter is active", () => {
    mockSearchParams.set("search", "nonexistent");
    render(
      <ProjectBrowser
        initialLeafs={[]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );
    expect(screen.getByTestId("empty-search")).toBeInTheDocument();
    expect(screen.queryByTestId("empty-state")).not.toBeInTheDocument();
    mockSearchParams.delete("search");
  });

  it("shows empty-search state when no leafs but research area filter is active", () => {
    mockSearchParams.set("research_area", "genomics");
    render(
      <ProjectBrowser
        initialLeafs={[]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );
    expect(screen.getByTestId("empty-search")).toBeInTheDocument();
    expect(screen.queryByTestId("empty-state")).not.toBeInTheDocument();
    mockSearchParams.delete("research_area");
  });

  it("updates URL and fetches leafs when search changes", async () => {
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      data: {
        leafs: [makeLeaf({ id: "p2", name: "Found", slug: "found" })],
        pagination: { next_cursor: null, has_more: false },
      },
    });

    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );

    const input = screen.getByTestId("search-input");
    fireEvent.change(input, { target: { value: "climate" } });

    // Debounce fires after 300ms
    await act(async () => {
      jest.advanceTimersByTime(300);
    });

    expect(mockPush).toHaveBeenCalledWith("/leafs?search=climate");
    expect(listPublicLeafs).toHaveBeenCalledWith(
      expect.objectContaining({ search: "climate" }),
    );
  });

  it("updates URL and fetches leafs when research area changes", async () => {
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      data: {
        leafs: [],
        pagination: { next_cursor: null, has_more: false },
      },
    });

    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );

    const select = screen.getByTestId("research-area-filter");
    fireEvent.change(select, { target: { value: "genomics" } });

    expect(mockPush).toHaveBeenCalledWith("/leafs?research_area=genomics");
    expect(listPublicLeafs).toHaveBeenCalledWith(
      expect.objectContaining({ research_area: "genomics" }),
    );
  });

  it("updates URL and fetches leafs when sort changes", async () => {
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      data: {
        leafs: [],
        pagination: { next_cursor: null, has_more: false },
      },
    });

    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );

    const select = screen.getByTestId("sort-select");
    fireEvent.change(select, { target: { value: "created_at" } });

    expect(mockPush).toHaveBeenCalledWith("/leafs?sort=created_at");
    expect(listPublicLeafs).toHaveBeenCalledWith(
      expect.objectContaining({ sort: "created_at" }),
    );
  });

  it("removes sort param from URL when selecting default sort", async () => {
    mockSearchParams.set("sort", "created_at");
    (listPublicLeafs as jest.Mock).mockResolvedValue({
      data: {
        leafs: [],
        pagination: { next_cursor: null, has_more: false },
      },
    });

    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={defaultPagination}
        researchAreas={researchAreas}
      />,
    );

    const select = screen.getByTestId("sort-select");
    fireEvent.change(select, { target: { value: "updated_at" } });

    // updated_at is the default, so sort should be removed from URL
    expect(mockPush).toHaveBeenCalledWith("/leafs");
    mockSearchParams.delete("sort");
  });

  it("does not call listPublicLeafs when Load More has no cursor", async () => {
    render(
      <ProjectBrowser
        initialLeafs={[makeLeaf()]}
        initialPagination={{ next_cursor: null, has_more: false }}
        researchAreas={researchAreas}
      />,
    );

    // Load More button should not be visible at all
    expect(screen.queryByTestId("load-more-button")).not.toBeInTheDocument();
    expect(listPublicLeafs).not.toHaveBeenCalled();
  });
});
