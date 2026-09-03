import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { invoke, defaultCommandResult } from "@tauri-apps/api/core";
import { StatusBar } from "./status-bar";

const mockUseDaemonStatus = vi.fn();
const mockUseSystemMetrics = vi.fn();

vi.mock("@/hooks/use-daemon-status", () => ({
  useDaemonStatus: () => mockUseDaemonStatus(),
}));

vi.mock("@/hooks/use-metrics", () => ({
  useSystemMetrics: () => mockUseSystemMetrics(),
}));

function status(overrides: Record<string, unknown>) {
  return {
    status: {
      state: "active",
      uptime_seconds: 10,
      connected_servers: 1,
      active_tasks: [],
      queued_tasks: [],
      failing_leafs: [],
      paused_reason: null,
      ...overrides,
    },
    isLoading: false,
    error: null,
    refetch: vi.fn(),
  };
}

describe("StatusBar", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseSystemMetrics.mockReturnValue({
      system: { cpu_usage_pct: 33.4, memory_used_mb: 2048, memory_total_mb: 8192 },
      error: null,
    });
  });

  it("shows host CPU and memory from the app's own measurement", () => {
    mockUseDaemonStatus.mockReturnValue(status({}));
    render(<StatusBar />);
    expect(screen.getByText("CPU 33%")).toBeInTheDocument();
    expect(screen.getByText("MEM 2.0 GB")).toBeInTheDocument();
    expect(screen.getByText("Active — waiting for tasks")).toBeInTheDocument();
  });

  it("words the scheduled pause as being outside the schedule", () => {
    mockUseDaemonStatus.mockReturnValue(status({ state: "paused", paused_reason: "scheduled" }));
    render(<StatusBar />);
    expect(screen.getByText("Paused — outside your schedule")).toBeInTheDocument();
  });

  it("passes other pause reasons through", () => {
    mockUseDaemonStatus.mockReturnValue(status({ state: "paused", paused_reason: "thermal" }));
    render(<StatusBar />);
    expect(screen.getByText("Paused — thermal")).toBeInTheDocument();
  });

  it("omits the host figures until they are measured", () => {
    mockUseSystemMetrics.mockReturnValue({ system: null, error: null });
    mockUseDaemonStatus.mockReturnValue(status({}));
    render(<StatusBar />);
    expect(screen.queryByText(/CPU/)).not.toBeInTheDocument();
  });

  // TB-52: while the daemon is not answering, the bar asks the host what the
  // daemon process is doing instead of saying "Stopped" for a daemon that is
  // starting, or for one that refused to start and said why.
  describe("while the daemon is not answering", () => {
    const unreachable = { status: null, isLoading: false, error: null, refetch: vi.fn() };

    function processState(state: unknown) {
      vi.mocked(invoke).mockImplementation(async (cmd: string) =>
        cmd === "get_daemon_process_state" ? state : defaultCommandResult(cmd)
      );
    }

    afterEach(() => {
      vi.mocked(invoke).mockImplementation(async (cmd: string) => defaultCommandResult(cmd));
    });

    it("says Starting while the daemon the app started is coming up", async () => {
      processState({ state: "starting" });
      mockUseDaemonStatus.mockReturnValue(unreachable);
      render(<StatusBar />);
      expect(await screen.findByText("Starting…")).toBeInTheDocument();
    });

    it("quotes the daemon's own reason after it exited", async () => {
      processState({
        state: "exited",
        reason: "no servers configured. Run `lettuce-volunteer attach --server <host>` first",
        code: 1,
      });
      mockUseDaemonStatus.mockReturnValue(unreachable);
      render(<StatusBar />);
      expect(
        await screen.findByText(
          "Stopped — no servers configured. Run `lettuce-volunteer attach --server <host>` first"
        )
      ).toBeInTheDocument();
    });

    it("says Stopped when nothing is running and nothing was started", async () => {
      processState({ state: "stopped" });
      mockUseDaemonStatus.mockReturnValue(unreachable);
      render(<StatusBar />);
      expect(await screen.findByText("Stopped")).toBeInTheDocument();
    });
  });
});
