import { render, screen } from "@testing-library/react";
import { StatusPanel } from "@/components/contribute/status-panel";
import type { PoolStats } from "@/lib/volunteer/pool-manager";

const defaultStats: PoolStats = {
  activeWorkers: 2,
  completedWorkUnits: 10,
  failedWorkUnits: 0,
  totalRuntimeSeconds: 120,
};

describe("StatusPanel", () => {
  it("renders nothing when not running", () => {
    const { container } = render(
      <StatusPanel stats={defaultStats} workerCount={4} running={false} />
    );
    expect(container.innerHTML).toBe("");
  });

  it("renders the Status heading when running", () => {
    render(
      <StatusPanel stats={defaultStats} workerCount={4} running={true} />
    );
    expect(screen.getByText("Status")).toBeInTheDocument();
  });

  it("displays active workers out of total", () => {
    render(
      <StatusPanel stats={defaultStats} workerCount={4} running={true} />
    );
    expect(screen.getByText("2 / 4")).toBeInTheDocument();
  });

  it("displays completed work units count", () => {
    render(
      <StatusPanel stats={defaultStats} workerCount={4} running={true} />
    );
    // "Work Units Completed" label is present
    expect(screen.getByText("Work Units Completed")).toBeInTheDocument();
    // The value 10 appears (twice — once for completed, once for estimated credit)
    const tens = screen.getAllByText("10");
    expect(tens.length).toBeGreaterThanOrEqual(1);
  });

  it("displays compute time starting at 00:00:00", () => {
    render(
      <StatusPanel stats={defaultStats} workerCount={4} running={true} />
    );
    expect(screen.getByText("Total Compute Time")).toBeInTheDocument();
    // Timer starts at 0 on mount
    expect(screen.getByText("00:00:00")).toBeInTheDocument();
  });

  it("shows worker progress bars when activeWorkers > 0", () => {
    render(
      <StatusPanel stats={defaultStats} workerCount={4} running={true} />
    );
    expect(screen.getByText("Worker Activity")).toBeInTheDocument();
    expect(screen.getByText("Worker 1")).toBeInTheDocument();
    expect(screen.getByText("Worker 2")).toBeInTheDocument();
    expect(screen.getByText("Worker 3")).toBeInTheDocument();
    expect(screen.getByText("Worker 4")).toBeInTheDocument();
  });

  it("hides worker activity section when no active workers", () => {
    const zeroActiveStats: PoolStats = { ...defaultStats, activeWorkers: 0 };
    render(
      <StatusPanel stats={zeroActiveStats} workerCount={2} running={true} />
    );
    expect(screen.queryByText("Worker Activity")).not.toBeInTheDocument();
  });

  it("shows failed work units message when failures exist", () => {
    const failedStats: PoolStats = { ...defaultStats, failedWorkUnits: 3 };
    render(
      <StatusPanel stats={failedStats} workerCount={4} running={true} />
    );
    expect(screen.getByText("3 work units failed")).toBeInTheDocument();
  });

  it("uses singular 'unit' when exactly 1 failure", () => {
    const failedStats: PoolStats = { ...defaultStats, failedWorkUnits: 1 };
    render(
      <StatusPanel stats={failedStats} workerCount={4} running={true} />
    );
    expect(screen.getByText("1 work unit failed")).toBeInTheDocument();
  });

  it("hides failure message when no failures", () => {
    render(
      <StatusPanel stats={defaultStats} workerCount={4} running={true} />
    );
    expect(screen.queryByText(/work unit.*failed/)).not.toBeInTheDocument();
  });
});
