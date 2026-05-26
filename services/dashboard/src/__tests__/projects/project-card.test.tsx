import { render, screen } from "@testing-library/react";
import { ProjectCard } from "@/components/projects/project-card";
import type { LeafWithStats } from "@/lib/actions/public-projects";
import type { LeafStats } from "@/types/infrastructure";

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

const baseLeaf: LeafWithStats = {
  id: "p1",
  name: "Climate Modeling Alpha",
  slug: "climate-modeling-alpha",
  description: "A distributed compute leaf that models climate patterns across the globe using advanced atmospheric simulations and high-resolution data to predict weather outcomes with improved accuracy.",
  research_area: "Climate Science",
  state: "ACTIVE",
  task_pattern: "PARAMETER_SWEEP",
  resource_requirements: {
    min_cpu_cores: 2,
    max_memory_mb: 512,
    gpu_required: false,
  },
  runtime: "NATIVE",
  is_ongoing: false,
  visibility: "PUBLIC",
  stats_cache_seconds: 60,
  active_volunteers: 0,
  progress_pct: null,
  created_at: "2026-01-01T00:00:00Z",
  stats: baseStats,
};

describe("ProjectCard", () => {
  it("renders leaf name as link to detail page", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    const link = screen.getByTestId("leaf-card");
    expect(link).toHaveAttribute("href", "/leafs/climate-modeling-alpha");
    expect(screen.getByText("Climate Modeling Alpha")).toBeInTheDocument();
  });

  it("truncates description at 150 chars", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    const desc = screen.getByTestId("description");
    // Original is 207 chars, should be truncated
    expect(desc.textContent!.length).toBeLessThanOrEqual(151); // 150 + ellipsis
    expect(desc.textContent).toContain("\u2026");
  });

  it("does not truncate short description", () => {
    const short = { ...baseLeaf, description: "A short description." };
    render(<ProjectCard leaf={short} />);
    const desc = screen.getByTestId("description");
    expect(desc.textContent).toBe("A short description.");
  });

  it("shows research area badges", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    expect(screen.getByTestId("research-area-badge")).toHaveTextContent("Climate Science");
  });

  it("hides research area when null", () => {
    const noArea = { ...baseLeaf, research_area: null };
    render(<ProjectCard leaf={noArea} />);
    expect(screen.queryByTestId("research-area-badge")).not.toBeInTheDocument();
  });

  it("shows CPU resource requirements text", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    const req = screen.getByTestId("resource-requirements");
    expect(req.textContent).toContain("CPU");
    expect(req.textContent).toContain("2 cores");
    expect(req.textContent).toContain("512 MB RAM");
  });

  it("shows GPU resource requirements when gpu_required", () => {
    const gpu = {
      ...baseLeaf,
      resource_requirements: {
        gpu_required: true,
        gpu_min_vram_mb: 4096,
      },
    };
    render(<ProjectCard leaf={gpu} />);
    const req = screen.getByTestId("resource-requirements");
    expect(req.textContent).toContain("GPU");
    expect(req.textContent).toContain("4096 MB VRAM");
  });

  it("shows active volunteer count from stats", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    expect(screen.getByTestId("volunteer-count")).toHaveTextContent("12");
  });

  it("shows progress percentage for finite leafs", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    expect(screen.getByTestId("progress-label")).toHaveTextContent("60%");
    expect(screen.getByTestId("progress-bar")).toBeInTheDocument();
  });

  it("shows 'ongoing' label for ongoing leafs", () => {
    const ongoing = { ...baseLeaf, is_ongoing: true };
    render(<ProjectCard leaf={ongoing} />);
    expect(screen.getByTestId("progress-label")).toHaveTextContent("ongoing");
    expect(screen.queryByTestId("progress-bar")).not.toBeInTheDocument();
  });

  it("shows green Active health badge for ACTIVE state", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    const badge = screen.getByTestId("health-badge");
    expect(badge).toHaveTextContent("Active");
    expect(badge.className).toContain("green");
  });

  it("shows blue New health badge when created within last 7 days", () => {
    const recent = {
      ...baseLeaf,
      created_at: new Date().toISOString(),
    };
    render(<ProjectCard leaf={recent} />);
    const badge = screen.getByTestId("health-badge");
    expect(badge).toHaveTextContent("New");
    expect(badge.className).toContain("blue");
  });

  it("shows yellow Paused health badge for PAUSED state", () => {
    const paused = { ...baseLeaf, state: "PAUSED" as const };
    render(<ProjectCard leaf={paused} />);
    const badge = screen.getByTestId("health-badge");
    expect(badge).toHaveTextContent("Paused");
    expect(badge.className).toContain("yellow");
  });

  it("handles null stats gracefully", () => {
    const noStats = { ...baseLeaf, stats: null };
    render(<ProjectCard leaf={noStats} />);
    expect(screen.getByTestId("volunteer-count")).toHaveTextContent("\u2014");
    expect(screen.getByTestId("progress-label")).toHaveTextContent("0%");
  });

  it("handles null resource_requirements gracefully", () => {
    const noReqs = { ...baseLeaf, resource_requirements: null };
    render(<ProjectCard leaf={noReqs} />);
    expect(screen.getByTestId("resource-requirements")).toHaveTextContent("CPU");
  });

  // --- Additional edge cases for resource requirements ---

  it("shows GPU without VRAM when gpu_min_vram_mb is not set", () => {
    const gpuNoVram = {
      ...baseLeaf,
      resource_requirements: {
        gpu_required: true,
      },
    };
    render(<ProjectCard leaf={gpuNoVram} />);
    const req = screen.getByTestId("resource-requirements");
    expect(req.textContent).toBe("GPU");
    expect(req.textContent).not.toContain("VRAM");
  });

  it("shows CPU with only cores when memory is not set", () => {
    const coresOnly = {
      ...baseLeaf,
      resource_requirements: {
        min_cpu_cores: 4,
        gpu_required: false,
      },
    };
    render(<ProjectCard leaf={coresOnly} />);
    const req = screen.getByTestId("resource-requirements");
    expect(req.textContent).toContain("CPU");
    expect(req.textContent).toContain("4 cores");
    expect(req.textContent).not.toContain("RAM");
  });

  it("shows CPU with only memory when cores is not set", () => {
    const memOnly = {
      ...baseLeaf,
      resource_requirements: {
        max_memory_mb: 1024,
        gpu_required: false,
      },
    };
    render(<ProjectCard leaf={memOnly} />);
    const req = screen.getByTestId("resource-requirements");
    expect(req.textContent).toContain("CPU");
    expect(req.textContent).toContain("1.0 GB RAM");
    expect(req.textContent).not.toContain("core");
  });

  it("shows singular 'core' for single CPU core", () => {
    const singleCore = {
      ...baseLeaf,
      resource_requirements: {
        min_cpu_cores: 1,
        gpu_required: false,
      },
    };
    render(<ProjectCard leaf={singleCore} />);
    const req = screen.getByTestId("resource-requirements");
    expect(req.textContent).toContain("1 core");
    expect(req.textContent).not.toContain("cores");
  });

  it("shows just 'CPU' when requirements have no cores or memory", () => {
    const emptyReqs = {
      ...baseLeaf,
      resource_requirements: {
        gpu_required: false,
      },
    };
    render(<ProjectCard leaf={emptyReqs} />);
    const req = screen.getByTestId("resource-requirements");
    expect(req.textContent).toBe("CPU");
  });

  // --- Description boundary ---

  it("does not truncate description of exactly 150 characters", () => {
    const exact150 = {
      ...baseLeaf,
      description: "x".repeat(150),
    };
    render(<ProjectCard leaf={exact150} />);
    const desc = screen.getByTestId("description");
    expect(desc.textContent).toBe("x".repeat(150));
    expect(desc.textContent).not.toContain("\u2026");
  });

  it("truncates description of 151 characters", () => {
    const just151 = {
      ...baseLeaf,
      description: "x".repeat(151),
    };
    render(<ProjectCard leaf={just151} />);
    const desc = screen.getByTestId("description");
    expect(desc.textContent).toBe("x".repeat(150) + "\u2026");
  });

  // --- Progress edge cases ---

  it("shows 0% progress when stats have zero total work units", () => {
    const zeroTotal = {
      ...baseLeaf,
      stats: { ...baseStats, total_work_units: 0, work_units_validated: 0 },
    };
    render(<ProjectCard leaf={zeroTotal} />);
    expect(screen.getByTestId("progress-label")).toHaveTextContent("0%");
  });

  it("shows 100% progress when all work units are completed", () => {
    const allComplete = {
      ...baseLeaf,
      stats: { ...baseStats, total_work_units: 50, work_units_validated: 50 },
    };
    render(<ProjectCard leaf={allComplete} />);
    expect(screen.getByTestId("progress-label")).toHaveTextContent("100%");
  });

  it("rounds progress to nearest integer", () => {
    const oddRatio = {
      ...baseLeaf,
      stats: { ...baseStats, total_work_units: 3, work_units_validated: 1 },
    };
    render(<ProjectCard leaf={oddRatio} />);
    // 1/3 = 33.33... -> rounds to 33.3 (one decimal place)
    expect(screen.getByTestId("progress-label")).toHaveTextContent("33.3%");
  });

  // --- Volunteer count ---

  it("shows zero volunteer count when stats report 0", () => {
    const noVolunteers = {
      ...baseLeaf,
      stats: { ...baseStats, active_volunteers: 0 },
    };
    render(<ProjectCard leaf={noVolunteers} />);
    expect(screen.getByTestId("volunteer-count")).toHaveTextContent("0");
  });

  // --- Runtime badge ---

  it("shows Native runtime badge for NATIVE leafs", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    const badge = screen.getByTestId("runtime-badge");
    expect(badge).toHaveTextContent("Native");
  });

  it("shows Container runtime badge for CONTAINER leafs", () => {
    const container = { ...baseLeaf, runtime: "CONTAINER" as const };
    render(<ProjectCard leaf={container} />);
    const badge = screen.getByTestId("runtime-badge");
    expect(badge).toHaveTextContent("Container");
  });

  // --- Pattern badge ---

  it("shows 'Parameter Sweep' pattern badge for PARAMETER_SWEEP", () => {
    render(<ProjectCard leaf={baseLeaf} />);
    const badge = screen.getByTestId("pattern-badge");
    expect(badge).toHaveTextContent("Parameter Sweep");
  });

  it("shows 'Map-Reduce' pattern badge for MAP_REDUCE", () => {
    const mapReduce = { ...baseLeaf, task_pattern: "MAP_REDUCE" as const };
    render(<ProjectCard leaf={mapReduce} />);
    const badge = screen.getByTestId("pattern-badge");
    expect(badge).toHaveTextContent("Map-Reduce");
  });

  it("shows 'Monte Carlo' pattern badge for MONTE_CARLO", () => {
    const monteCarlo = { ...baseLeaf, task_pattern: "MONTE_CARLO" as const };
    render(<ProjectCard leaf={monteCarlo} />);
    const badge = screen.getByTestId("pattern-badge");
    expect(badge).toHaveTextContent("Monte Carlo");
  });

  it("shows 'Custom' pattern badge for CUSTOM", () => {
    const custom = { ...baseLeaf, task_pattern: "CUSTOM" as const };
    render(<ProjectCard leaf={custom} />);
    const badge = screen.getByTestId("pattern-badge");
    expect(badge).toHaveTextContent("Custom");
  });
});
