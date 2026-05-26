import { render, screen } from "@testing-library/react";
import { ProjectDetail } from "@/components/projects/project-detail";
import type { Leaf, LeafStats } from "@/types/infrastructure";

const mockLeaf: Leaf = {
  id: "p1",
  name: "Climate Simulation",
  slug: "climate-simulation",
  description: "# Overview\n\nA leaf for **climate modeling** with [docs](https://example.com).",
  state: "ACTIVE",
  task_pattern: "PARAMETER_SWEEP",
  research_area: "Climate Science",
  creator_id: "user-1",
  execution_config: {
    runtime: "NATIVE",
    gpu_required: false,
    gpu_type: "ANY",
    min_vram_gb: 0,
    network_access: false,
    max_memory_mb: 1024,
    max_disk_mb: 2048,
    max_cpu_seconds: 3600,
  },
  validation_config: null,
  fault_tolerance_config: null,
  data_config: null,
  credit_config: null,
  resource_requirements: {
    min_cpu_cores: 2,
    min_disk_mb: 2048,
    gpu_required: false,
  },
  is_ongoing: false,
  visibility: "PUBLIC",
  stats_cache_seconds: 60,
  created_at: "2026-01-15T00:00:00Z",
  updated_at: "2026-03-14T00:00:00Z",
};

const mockStats: LeafStats = {
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

const mockCreator = {
  username: "researcher1",
  displayName: "Dr. Jane Smith",
  createdAt: new Date("2026-01-15T12:00:00Z"),
};

const defaultProps = {
  leaf: mockLeaf,
  stats: mockStats,
  creator: mockCreator,
  serverHost: "infra.example.com",
};

describe("ProjectDetail", () => {
  it("renders leaf name", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("leaf-name")).toHaveTextContent(
      "Climate Simulation",
    );
  });

  it("renders state badge", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("state-badge")).toHaveTextContent("ACTIVE");
  });

  it("renders research area badge", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("research-area-badge")).toHaveTextContent(
      "Climate Science",
    );
  });

  it("renders Markdown description", () => {
    render(<ProjectDetail {...defaultProps} />);
    // Markdown should render heading and bold text
    expect(screen.getByText("Overview")).toBeInTheDocument();
    expect(screen.getByText("climate modeling")).toBeInTheDocument();
  });

  it("renders creator card with username and member since", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("creator-username")).toHaveTextContent(
      "@researcher1",
    );
    expect(screen.getByTestId("creator-display-name")).toHaveTextContent(
      "Dr. Jane Smith",
    );
    expect(screen.getByTestId("creator-member-since")).toHaveTextContent(
      "Member since January 2026",
    );
  });

  it("renders resource requirements", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("cpu-requirement")).toHaveTextContent(
      "2 CPU cores",
    );
    expect(screen.getByTestId("memory-requirement")).toHaveTextContent(
      "1.0 GB RAM",
    );
    expect(screen.getByTestId("disk-requirement")).toHaveTextContent(
      "2048 MB disk",
    );
  });

  it("renders statistics", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("stat-volunteers")).toHaveTextContent("12");
    expect(screen.getByTestId("stat-completed")).toHaveTextContent("600");
    expect(screen.getByTestId("stat-credit")).toHaveTextContent("45,000");
    expect(screen.getByTestId("stat-avg-time")).toHaveTextContent("2m");
  });

  it("renders contribution instructions with correct server URL", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("contribute-section")).toBeInTheDocument();
    expect(screen.getByTestId("attach-command")).toHaveTextContent(
      "lettuce-volunteer attach --server infra.example.com",
    );
  });

  it("hides research area badge when null", () => {
    const noArea = {
      ...defaultProps,
      leaf: { ...mockLeaf, research_area: null },
    };
    render(<ProjectDetail {...noArea} />);
    expect(screen.queryByTestId("research-area-badge")).not.toBeInTheDocument();
  });

  it("handles null stats gracefully", () => {
    render(<ProjectDetail {...defaultProps} stats={null} />);
    expect(screen.queryByTestId("statistics-card")).not.toBeInTheDocument();
  });

  it("handles null creator gracefully", () => {
    render(<ProjectDetail {...defaultProps} creator={null} />);
    expect(screen.queryByTestId("creator-card")).not.toBeInTheDocument();
  });

  it("renders default resource requirements when null", () => {
    const noReqs = {
      ...defaultProps,
      leaf: { ...mockLeaf, resource_requirements: null, execution_config: null },
    };
    render(<ProjectDetail {...noReqs} />);
    expect(screen.getByTestId("cpu-requirement")).toHaveTextContent(
      "1 CPU core",
    );
    // Memory comes from execution_config.max_memory_mb; absent -> em dash.
    expect(screen.getByTestId("memory-requirement")).toHaveTextContent("—");
    expect(screen.getByTestId("disk-requirement")).toHaveTextContent(
      "1024 MB disk",
    );
  });

  it("renders agreement rate when work units are completed", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("stat-agreement")).toHaveTextContent("97.0%");
  });

  // --- Pattern card ---

  it("renders pattern card with pattern name and description", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.getByTestId("pattern-card")).toBeInTheDocument();
    expect(screen.getByTestId("pattern-name")).toHaveTextContent("Parameter Sweep");
    expect(screen.getByTestId("pattern-description")).toHaveTextContent(
      "Explores a defined parameter space by running each combination as an independent work unit.",
    );
  });

  // --- Aggregation card ---

  it("renders aggregation card when aggregation prop is provided", () => {
    const aggregation = {
      status: "complete" as const,
      format: "json" as const,
      work_units_aggregated: 500,
      work_units_total: 1000,
      aggregated_at: "2026-03-14T12:00:00Z",
    };
    render(<ProjectDetail {...defaultProps} aggregation={aggregation} />);
    const card = screen.getByTestId("aggregation-card");
    expect(card).toBeInTheDocument();
    expect(card).toHaveTextContent("complete");
    expect(card).toHaveTextContent("500");
    expect(card).toHaveTextContent("1,000");
  });

  it("does not render aggregation card when aggregation is null", () => {
    render(<ProjectDetail {...defaultProps} aggregation={null} />);
    expect(screen.queryByTestId("aggregation-card")).not.toBeInTheDocument();
  });

  it("does not render aggregation card when aggregation is not provided", () => {
    render(<ProjectDetail {...defaultProps} />);
    expect(screen.queryByTestId("aggregation-card")).not.toBeInTheDocument();
  });
});
