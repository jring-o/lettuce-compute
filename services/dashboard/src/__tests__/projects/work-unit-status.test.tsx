import { render, screen } from "@testing-library/react";
import { WorkUnitStatus } from "@/components/projects/work-unit-status";
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

describe("WorkUnitStatus", () => {
  it("renders the stacked bar and legend for non-zero stats", () => {
    render(<WorkUnitStatus stats={baseStats} />);
    expect(screen.getByTestId("work-unit-status")).toBeInTheDocument();
  });

  it("returns null when total_work_units is 0", () => {
    const zeroStats = { ...baseStats, total_work_units: 0 };
    const { container } = render(<WorkUnitStatus stats={zeroStats} />);
    expect(container.innerHTML).toBe("");
  });

  it("renders legend entries only for non-zero segments", () => {
    render(<WorkUnitStatus stats={baseStats} />);
    expect(screen.getByText(/Queued/)).toBeInTheDocument();
    expect(screen.getByText(/Assigned/)).toBeInTheDocument();
    expect(screen.getByText(/Running/)).toBeInTheDocument();
    expect(screen.getByText(/Completed/)).toBeInTheDocument();
    expect(screen.getByText(/Validated/)).toBeInTheDocument();
    expect(screen.getByText(/Failed/)).toBeInTheDocument();
  });

  it("shows formatted counts in legend", () => {
    render(<WorkUnitStatus stats={baseStats} />);
    // 600 completed / 600 validated (both show 600)
    expect(screen.getAllByText(/600/).length).toBeGreaterThanOrEqual(1);
    // 200 queued
    expect(screen.getByText(/200/)).toBeInTheDocument();
  });

  it("renders all segments when all categories have values", () => {
    // baseStats already has all segments non-zero
    render(<WorkUnitStatus stats={baseStats} />);
    expect(screen.getByText(/Queued/)).toBeInTheDocument();
    expect(screen.getByText(/Assigned/)).toBeInTheDocument();
    expect(screen.getByText(/Running/)).toBeInTheDocument();
    expect(screen.getByText(/Completed/)).toBeInTheDocument();
    expect(screen.getByText(/Validated/)).toBeInTheDocument();
    expect(screen.getByText(/Failed/)).toBeInTheDocument();
  });

  it("renders only one segment when all work units are validated", () => {
    const allValidated: LeafStats = {
      ...baseStats,
      total_work_units: 500,
      work_units_queued: 0,
      work_units_assigned: 0,
      work_units_running: 0,
      work_units_completed: 0,
      work_units_validated: 500,
      work_units_failed: 0,
    };
    render(<WorkUnitStatus stats={allValidated} />);
    expect(screen.getByText(/Validated/)).toBeInTheDocument();
    expect(screen.queryByText(/Queued/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Failed/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Running/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Completed/)).not.toBeInTheDocument();
  });

  it("formats large counts with locale separators", () => {
    const largeStats: LeafStats = {
      ...baseStats,
      total_work_units: 100000,
      work_units_completed: 0,
      work_units_validated: 50000,
      work_units_queued: 50000,
      work_units_assigned: 0,
      work_units_running: 0,
      work_units_failed: 0,
    };
    render(<WorkUnitStatus stats={largeStats} />);
    // toLocaleString produces "50,000" in en-US — both Queued and Validated show it
    const matches = screen.getAllByText(/50,000/);
    expect(matches).toHaveLength(2);
  });
});
