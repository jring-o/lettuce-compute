import { render, screen, waitFor } from "@testing-library/react";
import { ProjectDashboard } from "@/components/projects/project-dashboard";
import type { Leaf, LeafState, LeafStats } from "@/types/infrastructure";

// --- Mocks ---

jest.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: jest.fn() }),
}));

const mockGetLeafStats = jest.fn();
jest.mock("@/lib/actions/stats", () => ({
  getLeafStats: (...args: unknown[]) => mockGetLeafStats(...args),
}));

const mockListWorkUnits = jest.fn();
jest.mock("@/lib/actions/work-units", () => ({
  listWorkUnits: (...args: unknown[]) => mockListWorkUnits(...args),
}));

jest.mock("@/lib/actions/projects", () => ({
  pauseLeaf: jest.fn().mockResolvedValue({ data: {} }),
  resumeLeaf: jest.fn().mockResolvedValue({ data: {} }),
  archiveLeaf: jest.fn().mockResolvedValue({ data: {} }),
}));

const mockTriggerAggregation = jest.fn();
jest.mock("@/lib/actions/aggregation", () => ({
  triggerLeafAggregation: (...args: unknown[]) => mockTriggerAggregation(...args),
}));

// --- Fixtures ---

const baseLeaf: Leaf = {
  id: "p1",
  name: "Monte Carlo Simulation",
  slug: "monte-carlo-sim",
  description: "A test leaf for Monte Carlo methods",
  state: "ACTIVE",
  task_pattern: "PARAMETER_SWEEP",
  research_area: "physics",
  creator_id: "u1",
  execution_config: null,
  validation_config: null,
  fault_tolerance_config: null,
  data_config: null,
  credit_config: null,
  resource_requirements: null,
  is_ongoing: false,
  visibility: "PUBLIC",
  stats_cache_seconds: 60,
  created_at: "2026-03-14T00:00:00Z",
  updated_at: "2026-03-14T00:00:00Z",
};

const baseStats: LeafStats = {
  id: "00000000-0000-0000-0000-000000000001",
  leaf_id: "p1",
  snapshot_at: "2026-03-14T00:00:00Z",
  total_work_units: 1000,
  work_units_queued: 200,
  work_units_assigned: 100,
  work_units_running: 50,
  work_units_completed: 600,
  work_units_validated: 600,
  work_units_failed: 50,
  active_volunteers: 12,
  total_credit_granted: 45000,
  avg_completion_seconds: 120,
  agreement_rate: 0.97,
  throughput_per_hour: 25.5,
  created_at: "2026-03-14T00:00:00Z",
};

beforeEach(() => {
  jest.clearAllMocks();
  jest.useFakeTimers();
  mockListWorkUnits.mockResolvedValue({
    data: { data: [], pagination: { next_cursor: null, has_more: false } },
  });
});

afterEach(() => {
  jest.useRealTimers();
});

describe("ProjectDashboard", () => {
  it("renders leaf name and state badge", () => {
    render(<ProjectDashboard leaf={baseLeaf} initialStats={baseStats} />);
    expect(screen.getByText("Monte Carlo Simulation")).toBeInTheDocument();
    expect(screen.getByText("ACTIVE")).toBeInTheDocument();
  });

  it("renders description when present", () => {
    render(<ProjectDashboard leaf={baseLeaf} initialStats={baseStats} />);
    expect(
      screen.getByText("A test leaf for Monte Carlo methods"),
    ).toBeInTheDocument();
  });

  it("does not render description when absent", () => {
    const noDesc = { ...baseLeaf, description: "" };
    render(<ProjectDashboard leaf={noDesc} initialStats={baseStats} />);
    expect(
      screen.queryByText("A test leaf for Monte Carlo methods"),
    ).not.toBeInTheDocument();
  });

  it("shows setup message for DRAFT leafs", () => {
    const draft = { ...baseLeaf, state: "DRAFT" as const };
    render(<ProjectDashboard leaf={draft} initialStats={baseStats} />);
    expect(
      screen.getByText("Leaf is being set up..."),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Metrics will appear once the leaf is active."),
    ).toBeInTheDocument();
    // Should NOT show metrics
    expect(screen.queryByTestId("dashboard-metrics")).not.toBeInTheDocument();
  });

  it("shows setup message for CONFIGURING leafs", () => {
    const configuring = { ...baseLeaf, state: "CONFIGURING" as const };
    render(
      <ProjectDashboard leaf={configuring} initialStats={baseStats} />,
    );
    expect(
      screen.getByText("Leaf is being set up..."),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("dashboard-metrics")).not.toBeInTheDocument();
  });

  it("shows metrics and work unit sections for ACTIVE leafs", () => {
    render(<ProjectDashboard leaf={baseLeaf} initialStats={baseStats} />);
    expect(screen.getByTestId("dashboard-metrics")).toBeInTheDocument();
    expect(screen.getByText("Work Unit Distribution")).toBeInTheDocument();
    expect(screen.getByText("Work Units")).toBeInTheDocument();
  });

  it("shows metrics for PAUSED leafs", () => {
    const paused = { ...baseLeaf, state: "PAUSED" as const };
    render(<ProjectDashboard leaf={paused} initialStats={baseStats} />);
    expect(screen.getByTestId("dashboard-metrics")).toBeInTheDocument();
  });

  it("shows metrics for COMPLETED leafs", () => {
    const completed = { ...baseLeaf, state: "COMPLETED" as const };
    render(<ProjectDashboard leaf={completed} initialStats={baseStats} />);
    expect(screen.getByTestId("dashboard-metrics")).toBeInTheDocument();
  });

  it("shows metrics for ARCHIVED leafs (no auto-refresh)", () => {
    const archived = { ...baseLeaf, state: "ARCHIVED" as const };
    render(<ProjectDashboard leaf={archived} initialStats={baseStats} />);
    expect(screen.getByTestId("dashboard-metrics")).toBeInTheDocument();
  });

  it("does not set up auto-refresh for DRAFT leafs", () => {
    const draft = { ...baseLeaf, state: "DRAFT" as const };
    render(<ProjectDashboard leaf={draft} initialStats={baseStats} />);

    // Advance past the refresh interval
    jest.advanceTimersByTime(60_000);
    expect(mockGetLeafStats).not.toHaveBeenCalled();
  });

  it("does not set up auto-refresh for ARCHIVED leafs", () => {
    const archived = { ...baseLeaf, state: "ARCHIVED" as const };
    render(<ProjectDashboard leaf={archived} initialStats={baseStats} />);

    jest.advanceTimersByTime(60_000);
    expect(mockGetLeafStats).not.toHaveBeenCalled();
  });

  it("sets up auto-refresh for ACTIVE leafs", () => {
    mockGetLeafStats.mockResolvedValue({ data: baseStats });
    render(<ProjectDashboard leaf={baseLeaf} initialStats={baseStats} />);

    // Should not have called yet
    expect(mockGetLeafStats).not.toHaveBeenCalled();

    // Advance past 30s refresh interval
    jest.advanceTimersByTime(30_000);
    expect(mockGetLeafStats).toHaveBeenCalledWith("p1");
  });

  it("sets up auto-refresh for PAUSED leafs", () => {
    mockGetLeafStats.mockResolvedValue({ data: baseStats });
    const paused = { ...baseLeaf, state: "PAUSED" as const };
    render(<ProjectDashboard leaf={paused} initialStats={baseStats} />);

    jest.advanceTimersByTime(30_000);
    expect(mockGetLeafStats).toHaveBeenCalledWith("p1");
  });

  it("sets up auto-refresh for COMPLETED leafs", () => {
    mockGetLeafStats.mockResolvedValue({ data: baseStats });
    const completed = { ...baseLeaf, state: "COMPLETED" as const };
    render(<ProjectDashboard leaf={completed} initialStats={baseStats} />);

    jest.advanceTimersByTime(30_000);
    expect(mockGetLeafStats).toHaveBeenCalledWith("p1");
  });

  it("updates displayed stats after auto-refresh", async () => {
    const updatedStats: LeafStats = {
      ...baseStats,
      work_units_completed: 800,
      work_units_validated: 800,
      active_volunteers: 20,
      total_credit_granted: 60000,
    };
    mockGetLeafStats.mockResolvedValue({ data: updatedStats });

    render(<ProjectDashboard leaf={baseLeaf} initialStats={baseStats} />);

    // Initially shows original values
    expect(screen.getByText("12")).toBeInTheDocument(); // original active_volunteers
    expect(screen.getByText("45,000")).toBeInTheDocument(); // original credit

    // Advance past refresh interval
    jest.advanceTimersByTime(30_000);

    // Wait for updated values to appear
    await waitFor(() => {
      expect(screen.getByText("20")).toBeInTheDocument(); // updated volunteers
      expect(screen.getByText("60,000")).toBeInTheDocument(); // updated credit
    });
  });

  it("keeps previous stats when auto-refresh returns error", async () => {
    mockGetLeafStats.mockResolvedValue({
      error: { code: "INTERNAL", message: "Server error" },
    });

    render(<ProjectDashboard leaf={baseLeaf} initialStats={baseStats} />);

    // Initial values
    expect(screen.getByText("12")).toBeInTheDocument();

    jest.advanceTimersByTime(30_000);

    // Stats should remain unchanged after failed refresh
    await waitFor(() => {
      expect(mockGetLeafStats).toHaveBeenCalled();
    });
    expect(screen.getByText("12")).toBeInTheDocument();
    expect(screen.getByText("45,000")).toBeInTheDocument();
  });

  it("falls back to outline badge variant for unknown leaf state", () => {
    const unknownState = { ...baseLeaf, state: "UNKNOWN_STATE" as LeafState };
    render(<ProjectDashboard leaf={unknownState} initialStats={baseStats} />);
    // Should render without crashing — the Badge gets "outline" variant
    expect(screen.getByText("UNKNOWN_STATE")).toBeInTheDocument();
  });

  // --- Aggregation section ---

  it("renders aggregation section with data including status badge and count", () => {
    const aggregation = {
      status: "complete" as const,
      format: "json" as const,
      work_units_aggregated: 500,
      work_units_total: 1000,
      aggregated_at: "2026-03-14T12:00:00Z",
    };
    render(
      <ProjectDashboard
        leaf={baseLeaf}
        initialStats={baseStats}
        aggregation={aggregation}
      />,
    );
    expect(screen.getByTestId("aggregation-section")).toBeInTheDocument();
    expect(screen.getByTestId("aggregation-status")).toHaveTextContent("complete");
    expect(screen.getByTestId("aggregation-count")).toHaveTextContent("500 / 1,000");
  });

  it("renders aggregation section empty state when no aggregation", () => {
    render(<ProjectDashboard leaf={baseLeaf} initialStats={baseStats} />);
    expect(screen.getByTestId("aggregation-section")).toBeInTheDocument();
    expect(
      screen.getByText("No aggregation results yet. Trigger aggregation once work units are validated."),
    ).toBeInTheDocument();
    expect(screen.queryByTestId("aggregation-status")).not.toBeInTheDocument();
  });

  it("renders trigger aggregation button", () => {
    render(<ProjectDashboard leaf={baseLeaf} initialStats={baseStats} />);
    const button = screen.getByTestId("trigger-aggregation");
    expect(button).toBeInTheDocument();
    expect(button).toHaveTextContent("Trigger Aggregation");
  });
});
