import { render, screen } from "@testing-library/react";
import type { Leaf, WorkUnitSummary, Result, PaginatedResponse } from "@/types/infrastructure";

// --- Mocks (must be declared before imports that use them) ---

const mockNotFound = jest.fn();
jest.mock("next/navigation", () => ({
  notFound: () => {
    mockNotFound();
    // Throw to halt execution like the real notFound() does
    throw new Error("NEXT_NOT_FOUND");
  },
}));

jest.mock("next/link", () => {
  return function MockLink({
    children,
    href,
  }: {
    children: React.ReactNode;
    href: string;
  }) {
    return <a href={href}>{children}</a>;
  };
});

jest.mock("lucide-react", () => ({
  ArrowLeft: () => <span data-testid="arrow-left-icon" />,
}));

const mockGetLeaf = jest.fn();
const mockListWorkUnits = jest.fn();
const mockListResults = jest.fn();

jest.mock("@/lib/infrastructure-client", () => {
  // Import the real error class shape for instanceof checks
  class InfrastructureApiError extends Error {
    code: string;
    status: number;
    details?: Record<string, unknown>;
    constructor(code: string, message: string, status: number) {
      super(message);
      this.name = "InfrastructureApiError";
      this.code = code;
      this.status = status;
    }
  }

  return {
    InfrastructureApiError,
    infrastructureClient: {
      getLeaf: (...args: unknown[]) => mockGetLeaf(...args),
      listWorkUnits: (...args: unknown[]) => mockListWorkUnits(...args),
      listResults: (...args: unknown[]) => mockListResults(...args),
    },
  };
});

jest.mock("@/components/visualization/VisualizationPage", () => ({
  VisualizationPage: (props: Record<string, unknown>) => (
    <div data-testid="visualization-page" data-props={JSON.stringify(props)} />
  ),
}));

// Import after all mocks are set up
import VisualizePage from "@/app/leafs/[slug]/visualize/page";
import { InfrastructureApiError } from "@/lib/infrastructure-client";

// --- Helpers ---

function makeLeaf(overrides: Partial<Leaf> = {}): Leaf {
  return {
    id: "leaf-123",
    name: "N-Body Simulation",
    slug: "nbody-sim",
    description: "An N-body gravity simulation",
    state: "ACTIVE",
    task_pattern: "PARAMETER_SWEEP",
    research_area: "Physics",
    creator_id: "user-1",
    execution_config: {
      runtime: "CONTAINER",
      binaries: { viz: "https://example.com/viz-bundle.tar.gz" },
      gpu_required: false,
      gpu_type: "ANY",
      min_vram_gb: 0,
      network_access: false,
      max_memory_mb: 512,
      max_disk_mb: 1024,
      max_cpu_seconds: 3600,
    },
    validation_config: null,
    fault_tolerance_config: null,
    data_config: null,
    credit_config: null,
    resource_requirements: null,
    is_ongoing: false,
    visibility: "PUBLIC",
    stats_cache_seconds: 60,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-03-01T00:00:00Z",
    ...overrides,
  };
}

function makeWorkUnit(
  overrides: Partial<WorkUnitSummary> = {},
): WorkUnitSummary {
  return {
    id: "wu-001",
    leaf_id: "leaf-123",
    batch_id: null,
    state: "VALIDATED",
    priority: "NORMAL",
    assigned_to: null,
    attempts: 1,
    flagged_for_review: false,
    created_at: "2026-03-15T10:00:00Z",
    updated_at: "2026-03-15T12:30:00Z",
    ...overrides,
  };
}

function makeResult(overrides: Partial<Result> = {}): Result {
  return {
    id: "result-1",
    work_unit_id: "wu-001",
    volunteer_id: "vol-1",
    output_data: { particles: [1, 2, 3] },
    output_checksum: "abc123",
    execution_metadata: {
      wall_clock_seconds: 10,
      cpu_seconds_user: 8,
      cpu_seconds_system: 1,
      cpu_cores_used: 2,
      gpu_seconds: 0,
      gpu_vram_used_mb: 0,
      peak_memory_mb: 256,
      disk_read_mb: 1,
      disk_write_mb: 0.5,
      network_rx_mb: 0,
      network_tx_mb: 0,
    },
    validation_status: "AGREED",
    submitted_at: "2026-03-15T12:00:00Z",
    created_at: "2026-03-15T12:00:00Z",
    updated_at: "2026-03-15T12:00:00Z",
    ...overrides,
  };
}

function makeParams(slug: string, searchParams: Record<string, string> = {}) {
  return {
    params: Promise.resolve({ slug }),
    searchParams: Promise.resolve(searchParams as { [key: string]: string | string[] | undefined }),
  };
}

describe("VisualizePage (server component)", () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it('shows "This leaf does not include visualization" when binaries.viz is missing', async () => {
    const leaf = makeLeaf({
      execution_config: {
        runtime: "CONTAINER",
        binaries: {},
        gpu_required: false,
        gpu_type: "ANY",
        min_vram_gb: 0,
        network_access: false,
        max_memory_mb: 512,
        max_disk_mb: 1024,
        max_cpu_seconds: 3600,
      },
    });
    mockGetLeaf.mockResolvedValue(leaf);

    const page = await VisualizePage(makeParams("nbody-sim"));
    render(page);

    expect(
      screen.getByText("This leaf does not include visualization."),
    ).toBeInTheDocument();
    expect(
      screen.queryByTestId("visualization-page"),
    ).not.toBeInTheDocument();
  });

  it('shows "This leaf does not include visualization" when execution_config is null', async () => {
    const leaf = makeLeaf({ execution_config: null });
    mockGetLeaf.mockResolvedValue(leaf);

    const page = await VisualizePage(makeParams("nbody-sim"));
    render(page);

    expect(
      screen.getByText("This leaf does not include visualization."),
    ).toBeInTheDocument();
  });

  it("renders VisualizationPage with correct props when binaries.viz exists", async () => {
    const workUnits = [makeWorkUnit({ id: "wu-001" })];
    const result = makeResult({ work_unit_id: "wu-001" });

    mockGetLeaf.mockResolvedValue(makeLeaf());
    mockListWorkUnits.mockResolvedValue({
      data: workUnits,
      pagination: { next_cursor: null, has_more: false },
    } satisfies PaginatedResponse<WorkUnitSummary>);
    mockListResults.mockResolvedValue({
      data: [result],
      pagination: { next_cursor: null, has_more: false },
    } satisfies PaginatedResponse<Result>);

    const page = await VisualizePage(makeParams("nbody-sim"));
    render(page);

    const vizPage = screen.getByTestId("visualization-page");
    expect(vizPage).toBeInTheDocument();

    const passedProps = JSON.parse(vizPage.getAttribute("data-props")!);
    expect(passedProps.vizBundleUrl).toBe(
      "https://example.com/viz-bundle.tar.gz",
    );
    expect(passedProps.leafSlug).toBe("nbody-sim");
    expect(passedProps.leafId).toBe("leaf-123");
    expect(passedProps.workUnits).toHaveLength(1);
    expect(passedProps.initialResult).toBeTruthy();
    expect(passedProps.initialResult.work_unit_id).toBe("wu-001");
  });

  it("calls notFound() when leaf is PRIVATE", async () => {
    mockGetLeaf.mockResolvedValue(makeLeaf({ visibility: "PRIVATE" }));

    await expect(VisualizePage(makeParams("nbody-sim"))).rejects.toThrow(
      "NEXT_NOT_FOUND",
    );
    expect(mockNotFound).toHaveBeenCalled();
  });

  it("calls notFound() when leaf fetch returns 404", async () => {
    mockGetLeaf.mockRejectedValue(
      new InfrastructureApiError("NOT_FOUND", "Leaf not found", 404),
    );

    await expect(VisualizePage(makeParams("nbody-sim"))).rejects.toThrow(
      "NEXT_NOT_FOUND",
    );
    expect(mockNotFound).toHaveBeenCalled();
  });

  it("rethrows non-404 API errors", async () => {
    mockGetLeaf.mockRejectedValue(
      new InfrastructureApiError("SERVER_ERROR", "Internal server error", 500),
    );

    await expect(VisualizePage(makeParams("nbody-sim"))).rejects.toThrow(
      "Internal server error",
    );
    expect(mockNotFound).not.toHaveBeenCalled();
  });

  it("passes null initialResult when no work units exist", async () => {
    mockGetLeaf.mockResolvedValue(makeLeaf());
    mockListWorkUnits.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    });

    const page = await VisualizePage(makeParams("nbody-sim"));
    render(page);

    const vizPage = screen.getByTestId("visualization-page");
    const passedProps = JSON.parse(vizPage.getAttribute("data-props")!);
    expect(passedProps.workUnits).toHaveLength(0);
    expect(passedProps.initialResult).toBeNull();
  });

  it("passes null initialResult when listResults throws", async () => {
    const workUnits = [makeWorkUnit()];
    mockGetLeaf.mockResolvedValue(makeLeaf());
    mockListWorkUnits.mockResolvedValue({
      data: workUnits,
      pagination: { next_cursor: null, has_more: false },
    });
    mockListResults.mockRejectedValue(new Error("DB connection failed"));

    const page = await VisualizePage(makeParams("nbody-sim"));
    render(page);

    const vizPage = screen.getByTestId("visualization-page");
    const passedProps = JSON.parse(vizPage.getAttribute("data-props")!);
    expect(passedProps.initialResult).toBeNull();
  });

  it("renders back link pointing to the leaf page", async () => {
    mockGetLeaf.mockResolvedValue(makeLeaf());
    mockListWorkUnits.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    });

    const page = await VisualizePage(makeParams("nbody-sim"));
    render(page);

    const backLink = screen.getByText(`Back to N-Body Simulation`);
    expect(backLink.closest("a")).toHaveAttribute(
      "href",
      "/leafs/nbody-sim",
    );
  });

  // --- S109: volunteer filter passthrough ---

  it("passes volunteerFilter to VisualizationPage when volunteer search param is set", async () => {
    const workUnits = [makeWorkUnit({ id: "wu-001" })];
    const result = makeResult({ work_unit_id: "wu-001" });

    mockGetLeaf.mockResolvedValue(makeLeaf());
    mockListWorkUnits.mockResolvedValue({
      data: workUnits,
      pagination: { next_cursor: null, has_more: false },
    } satisfies PaginatedResponse<WorkUnitSummary>);
    mockListResults.mockResolvedValue({
      data: [result],
      pagination: { next_cursor: null, has_more: false },
    } satisfies PaginatedResponse<Result>);

    const page = await VisualizePage(makeParams("nbody-sim", { volunteer: "vol-abc123" }));
    render(page);

    const vizPage = screen.getByTestId("visualization-page");
    const passedProps = JSON.parse(vizPage.getAttribute("data-props")!);
    expect(passedProps.volunteerFilter).toBe("vol-abc123");
  });

  it("passes volunteer_id filter to listResults when volunteer search param is set", async () => {
    const workUnits = [makeWorkUnit({ id: "wu-001" })];

    mockGetLeaf.mockResolvedValue(makeLeaf());
    mockListWorkUnits.mockResolvedValue({
      data: workUnits,
      pagination: { next_cursor: null, has_more: false },
    } satisfies PaginatedResponse<WorkUnitSummary>);
    mockListResults.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    } satisfies PaginatedResponse<Result>);

    await VisualizePage(makeParams("nbody-sim", { volunteer: "vol-xyz789" }));

    expect(mockListResults).toHaveBeenCalledWith("leaf-123", {
      work_unit_id: "wu-001",
      validation_status: "AGREED",
      limit: 1,
      volunteer_id: "vol-xyz789",
    });
  });

  it("does not pass volunteer_id when volunteer search param is absent", async () => {
    const workUnits = [makeWorkUnit({ id: "wu-001" })];

    mockGetLeaf.mockResolvedValue(makeLeaf());
    mockListWorkUnits.mockResolvedValue({
      data: workUnits,
      pagination: { next_cursor: null, has_more: false },
    } satisfies PaginatedResponse<WorkUnitSummary>);
    mockListResults.mockResolvedValue({
      data: [],
      pagination: { next_cursor: null, has_more: false },
    } satisfies PaginatedResponse<Result>);

    await VisualizePage(makeParams("nbody-sim"));

    expect(mockListResults).toHaveBeenCalledWith("leaf-123", {
      work_unit_id: "wu-001",
      validation_status: "AGREED",
      limit: 1,
    });
  });
});
