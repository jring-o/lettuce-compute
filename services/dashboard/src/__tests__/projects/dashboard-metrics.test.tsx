import { render, screen } from "@testing-library/react";
import { DashboardMetrics } from "@/components/projects/dashboard-metrics";
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

describe("DashboardMetrics", () => {
  it("renders all 6 metric cards", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={false} />);
    expect(screen.getByText("Progress")).toBeInTheDocument();
    expect(screen.getByText("Active Volunteers")).toBeInTheDocument();
    expect(screen.getByText("Completion Rate")).toBeInTheDocument();
    expect(screen.getByText("Est. Completion")).toBeInTheDocument();
    expect(screen.getByText("Agreement Rate")).toBeInTheDocument();
    expect(screen.getByText("Credit Granted")).toBeInTheDocument();
  });

  it("shows correct progress values for finite leaf", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={false} />);
    expect(screen.getByText("600")).toBeInTheDocument();
    expect(screen.getByText(/\/ 1,000/)).toBeInTheDocument();
  });

  it("shows progress bar for finite leaf", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={false} />);
    const progressBar = screen.getByTestId("progress-bar");
    expect(progressBar).toBeInTheDocument();
    // 600/1000 = 60%
    expect(progressBar).toHaveAttribute("aria-valuenow", "60");
  });

  it("shows 'completed' label for ongoing leaf without progress bar", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={true} />);
    expect(screen.getByText("completed")).toBeInTheDocument();
    expect(screen.queryByTestId("progress-bar")).not.toBeInTheDocument();
  });

  it("shows active volunteer count", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={false} />);
    expect(screen.getByText("12")).toBeInTheDocument();
    expect(screen.getByText("volunteering")).toBeInTheDocument();
  });

  it("shows completion rate", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={false} />);
    expect(screen.getByText("25.5")).toBeInTheDocument();
    expect(screen.getByText("/hr")).toBeInTheDocument();
  });

  it("shows ETC for finite leaf with throughput", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={false} />);
    // (1000 - 600) / 25.5 = 15.69 hours → 15h 41m
    expect(screen.getByText("15h 41m")).toBeInTheDocument();
  });

  it("shows dash for ETC when throughput is 0", () => {
    const zeroThroughput = { ...baseStats, throughput_per_hour: 0 };
    render(<DashboardMetrics stats={zeroThroughput} isOngoing={false} />);
    // Find the ETC card's dash
    const etcCards = screen.getAllByText("\u2014");
    expect(etcCards.length).toBeGreaterThanOrEqual(1);
  });

  it("shows dash for ETC when leaf is ongoing", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={true} />);
    const dashes = screen.getAllByText("\u2014");
    expect(dashes.length).toBeGreaterThanOrEqual(1);
  });

  it("shows agreement rate as green when >= 95%", () => {
    // agreement_rate 0.97 → 97.0%
    render(<DashboardMetrics stats={baseStats} isOngoing={false} />);
    const rate = screen.getByTestId("agreement-rate");
    expect(rate).toHaveTextContent("97.0%");
    expect(rate.className).toContain("green");
  });

  it("shows agreement rate as yellow when >= 80%", () => {
    const stats = { ...baseStats, agreement_rate: 0.85 };
    render(<DashboardMetrics stats={stats} isOngoing={false} />);
    const rate = screen.getByTestId("agreement-rate");
    expect(rate).toHaveTextContent("85.0%");
    expect(rate.className).toContain("yellow");
  });

  it("shows agreement rate as red when < 80%", () => {
    const stats = { ...baseStats, agreement_rate: 0.7 };
    render(<DashboardMetrics stats={stats} isOngoing={false} />);
    const rate = screen.getByTestId("agreement-rate");
    expect(rate).toHaveTextContent("70.0%");
    expect(rate.className).toContain("red");
  });

  it("shows dash for agreement rate when null", () => {
    const noRate = { ...baseStats, agreement_rate: null };
    render(<DashboardMetrics stats={noRate} isOngoing={false} />);
    expect(screen.queryByTestId("agreement-rate")).not.toBeInTheDocument();
  });

  it("shows total credit awarded formatted with commas", () => {
    render(<DashboardMetrics stats={baseStats} isOngoing={false} />);
    expect(screen.getByText("45,000")).toBeInTheDocument();
  });

  it("shows '< 1 min' for ETC when remaining time is under 1 minute", () => {
    // total=1000, validated=999, throughput=100/hr → (1/100)h = 0.01h = 0.6min < 1min
    const nearDone = {
      ...baseStats,
      work_units_validated: 999,
      throughput_per_hour: 100,
    };
    render(<DashboardMetrics stats={nearDone} isOngoing={false} />);
    expect(screen.getByText("< 1 min")).toBeInTheDocument();
  });

  it("shows minutes-only format for ETC under 1 hour", () => {
    // total=1000, validated=990, throughput=60/hr → 10/60=0.1667h = 10m
    const shortRemaining = {
      ...baseStats,
      total_work_units: 1000,
      work_units_validated: 990,
      throughput_per_hour: 60,
    };
    render(<DashboardMetrics stats={shortRemaining} isOngoing={false} />);
    expect(screen.getByText("10m")).toBeInTheDocument();
  });

  it("handles zero total work units without division error", () => {
    const zeroTotal = {
      ...baseStats,
      total_work_units: 0,
      work_units_validated: 0,
      work_units_queued: 0,
    };
    render(<DashboardMetrics stats={zeroTotal} isOngoing={false} />);
    // Progress bar should show 0%
    const progressBar = screen.getByTestId("progress-bar");
    expect(progressBar).toHaveAttribute("aria-valuenow", "0");
  });

  it("shows agreement rate as green at exactly 95% boundary", () => {
    const stats = { ...baseStats, agreement_rate: 0.95 };
    render(<DashboardMetrics stats={stats} isOngoing={false} />);
    const rate = screen.getByTestId("agreement-rate");
    expect(rate).toHaveTextContent("95.0%");
    expect(rate.className).toContain("green");
  });

  it("shows agreement rate as yellow at exactly 80% boundary", () => {
    const stats = { ...baseStats, agreement_rate: 0.8 };
    render(<DashboardMetrics stats={stats} isOngoing={false} />);
    const rate = screen.getByTestId("agreement-rate");
    expect(rate).toHaveTextContent("80.0%");
    expect(rate.className).toContain("yellow");
  });

  it("shows 0 volunteers when none are active", () => {
    const noVolunteers = { ...baseStats, active_volunteers: 0 };
    render(<DashboardMetrics stats={noVolunteers} isOngoing={false} />);
    expect(screen.getByText("0")).toBeInTheDocument();
  });

  it("shows 0.0 throughput when rate is zero", () => {
    const noThroughput = { ...baseStats, throughput_per_hour: 0 };
    render(<DashboardMetrics stats={noThroughput} isOngoing={false} />);
    expect(screen.getByText("0.0")).toBeInTheDocument();
  });
});
