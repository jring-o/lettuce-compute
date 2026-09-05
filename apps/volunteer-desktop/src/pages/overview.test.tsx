import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { OverviewPage } from "./overview";
import { VIZ_SILENT_TIMEOUT_MS } from "@/components/viz/VizFrame";
import { makeTask } from "@/components/tasks/test-helpers";
import { ApiError, type HeadsResponse, type Notice } from "@/api/client";
import { emit } from "@tauri-apps/api/event";

// Mock all hooks used by the page
const mockUseDaemonStatus = vi.fn();
const mockUseMetrics = vi.fn();
const mockUseSystemMetrics = vi.fn();
const mockUseCredit = vi.fn();
const mockUseApiQuery = vi.fn();
const mockUseNotices = vi.fn();
const mockClient = {
  pause: vi.fn(),
  resume: vi.fn(),
  suspendTask: vi.fn(),
  resumeTask: vi.fn(),
  abortTask: vi.fn(),
  taskDetails: vi.fn(),
};
const mockUseClient = vi.fn();
const mockUseContainerRuntime = vi.fn();

vi.mock("@/hooks/use-daemon-status", () => ({
  useDaemonStatus: () => mockUseDaemonStatus(),
}));

vi.mock("@/hooks/use-metrics", () => ({
  useMetrics: () => mockUseMetrics(),
  useSystemMetrics: () => mockUseSystemMetrics(),
}));

vi.mock("@/hooks/use-credit", () => ({
  useCredit: () => mockUseCredit(),
}));

vi.mock("@/hooks/use-api", () => ({
  useClient: () => mockUseClient(),
  useApiQuery: (...args: unknown[]) => mockUseApiQuery(...args),
}));

vi.mock("@/hooks/use-notices", () => ({
  useNotices: () => mockUseNotices(),
}));

vi.mock("@/hooks/use-container-runtime", () => ({
  useContainerRuntime: () => mockUseContainerRuntime(),
}));

const noGpuMachine: HeadsResponse["machine"] = {
  runtimes: ["wasm"],
  has_gpu: false,
  max_memory_mb: 4096,
  container_vm_memory_mb: 0,
  memory_limited_by_vm: false,
  max_disk_mb: 10240,
  max_cpu_cores: 4,
  max_gpu_vram_mb: 0,
  gpu_card_vram_mb: 0,
  gpu_vram_pct: 0,
  gpu_vendors: [],
  gpu_compute_capabilities: [],
};

function setHeads(resp: HeadsResponse | null) {
  mockUseApiQuery.mockReturnValue({
    data: resp,
    isLoading: false,
    error: null,
    refetch: vi.fn(),
  });
}

function makeNotice(overrides: Partial<Notice> & { id: number }): Notice {
  return {
    level: "warn",
    code: "TEST",
    message: "Something needs attention",
    count: 1,
    first_at: new Date(Date.now() - 60_000).toISOString(),
    at: new Date(Date.now() - 60_000).toISOString(),
    ...overrides,
  };
}

describe("OverviewPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockClient.pause.mockResolvedValue(undefined);
    mockClient.resume.mockResolvedValue(undefined);
    mockClient.suspendTask.mockResolvedValue(undefined);
    mockClient.resumeTask.mockResolvedValue(undefined);
    mockClient.abortTask.mockResolvedValue(undefined);
    mockClient.taskDetails.mockResolvedValue(null);
    mockUseContainerRuntime.mockReturnValue({
      status: null,
      loading: false,
      error: null,
      refresh: vi.fn(),
    });
    mockUseSystemMetrics.mockReturnValue({
      system: { cpu_usage_pct: 45, memory_used_mb: 8192, memory_total_mb: 16384 },
      error: null,
    });
    mockUseNotices.mockReturnValue({ notices: [], supported: true, dismiss: vi.fn() });
    setHeads({ heads: [], machine: noGpuMachine });
  });

  function setupDefaultMocks(overrides?: {
    status?: Partial<ReturnType<typeof mockUseDaemonStatus>>;
    metrics?: Partial<ReturnType<typeof mockUseMetrics>>;
    credit?: Partial<ReturnType<typeof mockUseCredit>>;
  }) {
    mockUseDaemonStatus.mockReturnValue({
      status: {
        state: "active",
        uptime_seconds: 3600,
        connected_servers: 2,
        active_tasks: [],
        queued_tasks: [],
        failing_leafs: [],
        paused_reason: null,
        client_version: "1.4.0",
      },
      isLoading: false,
      error: null,
      refetch: vi.fn(),
      ...overrides?.status,
    });

    mockUseMetrics.mockReturnValue({
      metrics: {
        cpu_usage_pct: 0,
        gpu_usage_pct: 0,
        memory_used_mb: 0,
        memory_total_mb: 0,
        cpu_temp_c: 0,
        gpu_temp_c: 0,
        disk_used_mb: 2048,
        disk_allowance_mb: 10240,
        disk_usage_known: true,
      },
      isLoading: false,
      error: null,
      ...overrides?.metrics,
    });

    mockUseCredit.mockReturnValue({
      credit: {
        total_credit: 5000,
        today: 50,
        this_week: 200,
        this_month: 800,
        by_leaf: [
          { leaf_id: "p1", leaf_name: "Climate", credit: 3000 },
          { leaf_id: "p2", leaf_name: "Biology", credit: 2000 },
        ],
        by_head: [],
        source: "head",
      },
      isLoading: false,
      error: null,
      ...overrides?.credit,
    });
  }

  it("renders active status badge", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText("Active")).toBeInTheDocument();
  });

  it("renders paused status badge with reason", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "paused",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [],
          paused_reason: "User requested",
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Paused — User requested")).toBeInTheDocument();
  });

  it("renders uptime", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText("Running for 1h 0m")).toBeInTheDocument();
  });

  it("shows Pause button when active", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText("Pause")).toBeInTheDocument();
  });

  it("shows Resume button when paused", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "paused",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [],
          paused_reason: "user",
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Resume")).toBeInTheDocument();
  });

  it("does not show pause/resume button when stopped", () => {
    setupDefaultMocks({
      status: { status: null },
    });
    render(<OverviewPage />);
    expect(screen.queryByText("Pause")).not.toBeInTheDocument();
    expect(screen.queryByText("Resume")).not.toBeInTheDocument();
  });

  it("calls client.pause when Pause is clicked", async () => {
    const user = userEvent.setup();
    setupDefaultMocks();
    render(<OverviewPage />);

    await user.click(screen.getByText("Pause"));
    expect(mockClient.pause).toHaveBeenCalledOnce();
  });

  it("calls client.resume when Resume is clicked", async () => {
    const user = userEvent.setup();
    setupDefaultMocks({
      status: {
        status: {
          state: "paused",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [],
          paused_reason: "user",
        },
      },
    });
    render(<OverviewPage />);

    await user.click(screen.getByText("Resume"));
    expect(mockClient.resume).toHaveBeenCalledOnce();
  });

  it("renders active tasks section header", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText("Active Tasks")).toBeInTheDocument();
  });

  it("shows empty state message when no tasks and active", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(
      screen.getByText("No active tasks. Waiting for work...")
    ).toBeInTheDocument();
  });

  it("shows paused empty state when paused with no tasks", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "paused",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [],
          paused_reason: "user",
        },
      },
    });
    render(<OverviewPage />);
    expect(
      screen.getByText("Computing is paused. Resume to start contributing.")
    ).toBeInTheDocument();
  });

  it("renders active task cards with progress", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-12345678-abcd",
              leaf_name: "Climate Model",
              progress_pct: 63,
              elapsed_seconds: 1200,
              cpu_seconds: 1200,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    expect(screen.getByText("Climate Model")).toBeInTheDocument();
    expect(screen.getByText("wu-12345")).toBeInTheDocument();
    // 63% is unique — won't collide with gauge percentages
    expect(screen.getByText("63%")).toBeInTheDocument();
  });

  it("renders CPU and memory gauges from the app's own measurement and disk from the daemon", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText("Resources")).toBeInTheDocument();
    expect(screen.getByText("CPU")).toBeInTheDocument();
    expect(screen.getByText("Memory")).toBeInTheDocument();
    expect(screen.getByText("Disk")).toBeInTheDocument();
    // The GPU-usage gauge was never measured and is gone.
    expect(screen.queryByText("GPU")).not.toBeInTheDocument();
    // CPU comes from useSystemMetrics (45%), not the daemon's zeros.
    expect(screen.getByText("45%")).toBeInTheDocument();
    expect(
      screen.getByText("Lettuce is using 2.0 GB of your 10.0 GB allowance")
    ).toBeInTheDocument();
  });

  it("hides the disk gauge when the daemon does not know its disk usage", () => {
    setupDefaultMocks({
      metrics: {
        metrics: {
          cpu_usage_pct: 0,
          gpu_usage_pct: 0,
          memory_used_mb: 0,
          memory_total_mb: 0,
          cpu_temp_c: 0,
          gpu_temp_c: 0,
          disk_used_mb: 0,
          disk_allowance_mb: 10240,
          disk_usage_known: false,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("CPU")).toBeInTheDocument();
    expect(screen.queryByText("Disk")).not.toBeInTheDocument();
    expect(screen.queryByText(/allowance/)).not.toBeInTheDocument();
  });

  it("does not render resources section when neither measurement is available", () => {
    setupDefaultMocks({ metrics: { metrics: null } });
    mockUseSystemMetrics.mockReturnValue({ system: null, error: null });
    render(<OverviewPage />);
    expect(screen.queryByText("Resources")).not.toBeInTheDocument();
  });

  it("describes the GPU and its allowance from the machine capabilities", () => {
    setupDefaultMocks();
    setHeads({
      heads: [],
      machine: {
        ...noGpuMachine,
        has_gpu: true,
        gpu_vendors: ["NVIDIA"],
        gpu_card_vram_mb: 8192,
        gpu_vram_pct: 70,
        max_gpu_vram_mb: 5734,
      },
    });
    render(<OverviewPage />);
    expect(
      screen.getByText("GPU: NVIDIA, 8.0 GB card, 5.6 GB allowed for Lettuce")
    ).toBeInTheDocument();
  });

  it("does not describe a GPU when the machine has none", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.queryByText(/^GPU:/)).not.toBeInTheDocument();
  });

  it("renders credit section", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText("Credit")).toBeInTheDocument();
    expect(screen.getByText("Today")).toBeInTheDocument();
    expect(screen.getByText("All Time")).toBeInTheDocument();
  });

  it("does not render credit section when credit is null", () => {
    setupDefaultMocks({ credit: { credit: null } });
    render(<OverviewPage />);
    expect(screen.queryByText("Credit")).not.toBeInTheDocument();
  });

  it("renders quick stats footer when status and credit are available", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText(/Connected servers: 2/)).toBeInTheDocument();
    expect(screen.getByText(/Leafs: 2/)).toBeInTheDocument();
  });

  it("does not render quick stats when status is null", () => {
    setupDefaultMocks({ status: { status: null }, credit: { credit: null } });
    render(<OverviewPage />);
    expect(screen.queryByText(/Connected servers/)).not.toBeInTheDocument();
  });

  it("renders multiple active tasks as separate cards", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 2,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-11111111-aaaa",
              leaf_name: "Prime Gap Study",
              progress_pct: 40,
              elapsed_seconds: 600,
              cpu_seconds: 600,
            }),
            makeTask({
              work_unit_id: "wu-22222222-bbbb",
              leaf_name: "Mandelbrot Analysis",
              progress_pct: 75,
              elapsed_seconds: 1200,
              cpu_seconds: 1200,
            }),
            makeTask({
              work_unit_id: "wu-33333333-cccc",
              leaf_name: "GW Search",
              progress_pct: 10,
              elapsed_seconds: 300,
              cpu_seconds: 300,
            }),
          ],
          paused_reason: null,
        },
      },
    });

    render(<OverviewPage />);
    expect(screen.getByText("Prime Gap Study")).toBeInTheDocument();
    expect(screen.getByText("Mandelbrot Analysis")).toBeInTheDocument();
    expect(screen.getByText("GW Search")).toBeInTheDocument();
  });

  it("renders credit breakdown when by_head is available", () => {
    setupDefaultMocks({
      credit: {
        credit: {
          total_credit: 5000,
          today: 50,
          this_week: 200,
          this_month: 800,
          by_leaf: [
            { leaf_id: "p1", leaf_name: "Climate", credit: 3000 },
          ],
          by_head: [
            {
              head_name: "lettuce.science",
              volunteer_id: "vol-1234567890",
              total_credit: 3000,
              available: true,
            },
          ],
          source: "head",
        },
      },
    });

    render(<OverviewPage />);
    expect(screen.getByText("Credit Breakdown")).toBeInTheDocument();
  });

  // TB-57: head-derived day buckets are by UTC date and cannot follow this
  // machine's clock, so the Overview must say so beside the counters.
  it("labels head-derived credit counters as UTC days", () => {
    setupDefaultMocks({
      credit: {
        credit: {
          total_credit: 48,
          today: 44,
          this_week: 48,
          this_month: 48,
          by_leaf: [],
          by_head: [],
          source: "head",
          day_boundary: "utc",
        },
      },
    });

    render(<OverviewPage />);
    expect(screen.getByTestId("credit-day-boundary")).toBeInTheDocument();
  });

  it("quick stats footer counts leaves from by_leaf", () => {
    setupDefaultMocks({
      credit: {
        credit: {
          total_credit: 5000,
          today: 50,
          this_week: 200,
          this_month: 800,
          by_leaf: [
            { leaf_id: "a", leaf_name: "A", credit: 1000 },
            { leaf_id: "b", leaf_name: "B", credit: 1000 },
            { leaf_id: "c", leaf_name: "C", credit: 1000 },
          ],
          by_head: [
            {
              head_name: "lettuce.science",
              volunteer_id: "vol-1",
              total_credit: 3000,
              available: true,
            },
          ],
          source: "head",
        },
      },
    });

    render(<OverviewPage />);
    expect(screen.getByText(/Leafs: 3/)).toBeInTheDocument();
  });

  it("marks an unreachable head in the credit breakdown and formats decimals", async () => {
    const user = userEvent.setup();
    setupDefaultMocks({
      credit: {
        credit: {
          total_credit: 1234.5678,
          today: 0.5,
          this_week: 12.25,
          this_month: 100,
          by_leaf: [{ leaf_id: "p1", leaf_name: "Climate", credit: 1234.5678 }],
          by_head: [
            { head_name: "alpha.example", volunteer_id: "v-a", total_credit: 1234.5678, available: true },
            { head_name: "beta.example", volunteer_id: "v-b", total_credit: 0, available: false },
          ],
          source: "head",
        },
      },
    });

    render(<OverviewPage />);
    // Stat cards: fractions are cut to two decimals, whole numbers stay grouped.
    expect(screen.getAllByText("1,234.57").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("0.5")).toBeInTheDocument();
    expect(screen.getByText("12.25")).toBeInTheDocument();

    await user.click(screen.getByText("Credit Breakdown"));
    expect(screen.getByText("beta.example")).toBeInTheDocument();
    expect(screen.getByText("unreachable")).toBeInTheDocument();
  });

  it("explains local-only credit figures when no head could be reached", () => {
    setupDefaultMocks({
      credit: {
        credit: {
          total_credit: 12,
          today: 1,
          this_week: 2,
          this_month: 3,
          by_leaf: [],
          by_head: [],
          source: "local",
        },
      },
    });
    render(<OverviewPage />);
    expect(
      screen.getByText(/Figures from this machine's local history — no head could be reached/)
    ).toBeInTheDocument();
  });

  it("explains that credit moves only when a head validates results", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText(/Credit moves only when a head validates your results/)).toBeInTheDocument();
  });

  it("shows the client version in the footer", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.getByText(/Client v1\.4\.0/)).toBeInTheDocument();
  });

  it("falls back to the bundled CLI version when the daemon does not report one", async () => {
    const { invoke } = await import("@tauri-apps/api/core");
    const mockInvoke = invoke as ReturnType<typeof vi.fn>;
    mockInvoke.mockImplementation((cmd: string) =>
      Promise.resolve(cmd === "get_client_version" ? "1.3.9" : undefined)
    );
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [],
          queued_tasks: [],
          failing_leafs: [],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    await waitFor(() => {
      expect(screen.getByText(/Client v1\.3\.9/)).toBeInTheDocument();
    });
    mockInvoke.mockImplementation(() => Promise.resolve(undefined));
  });

  it("shows 'Paused — outside your schedule' for the scheduled pause reason", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "paused",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [],
          queued_tasks: [],
          failing_leafs: [],
          paused_reason: "scheduled",
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Paused — outside your schedule")).toBeInTheDocument();
  });

  it("renders a task suspended without a reported reason", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-suspended-plain",
              leaf_name: "Frozen Task",
              task_status: "suspended",
            }),
          ],
          queued_tasks: [],
          failing_leafs: [],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Paused (reason not reported)")).toBeInTheDocument();
    expect(document.querySelector(".bg-yellow-500.rounded-full")).toBeInTheDocument();
  });

  it("lists leaves failing on this machine with their reason and pause", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [],
          queued_tasks: [],
          failing_leafs: [
            {
              leaf_id: "leaf-1",
              leaf_name: "Protein Fold",
              consecutive_failures: 3,
              total_failures: 7,
              last_reason: "exit status 137 (out of memory)",
              last_failed_at: new Date().toISOString(),
              paused: true,
              paused_until: "2030-01-02T03:04:00Z",
            },
            {
              leaf_id: "leaf-2",
              leaf_name: "Prime Gap",
              consecutive_failures: 1,
              total_failures: 1,
              paused: false,
            },
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Failing on this machine")).toBeInTheDocument();
    expect(screen.getByText("Protein Fold")).toBeInTheDocument();
    expect(screen.getByText(/3 in a row/)).toBeInTheDocument();
    expect(screen.getByText(/7 total/)).toBeInTheDocument();
    expect(screen.getByText("exit status 137 (out of memory)")).toBeInTheDocument();
    expect(screen.getByText(/Not requesting more of this leaf until/)).toBeInTheDocument();
    expect(screen.getByText("Prime Gap")).toBeInTheDocument();
    expect(screen.getByText(/1 in a row/)).toBeInTheDocument();
  });

  it("does not render the failing block when nothing is failing", () => {
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.queryByText("Failing on this machine")).not.toBeInTheDocument();
  });

  it("shows an update-required banner for heads that refuse this client", () => {
    setupDefaultMocks();
    setHeads({
      heads: [
        { name: "lettuce.science", grpc_address: "a:1", status: "connected", weight: 100, leafs: [], update_required: true },
        { name: "other.example", grpc_address: "b:1", status: "connected", weight: 100, leafs: [] },
      ],
      machine: noGpuMachine,
    });
    render(<OverviewPage />);
    expect(
      screen.getByText("This app is too old for lettuce.science — update Lettuce Compute")
    ).toBeInTheDocument();
    expect(screen.queryByText(/too old for other.example/)).not.toBeInTheDocument();
  });

  it("renders the notices panel above the task list with newest first and a dismiss", async () => {
    const user = userEvent.setup();
    const dismiss = vi.fn();
    mockUseNotices.mockReturnValue({
      supported: true,
      dismiss,
      notices: [
        makeNotice({ id: 5, level: "error", message: "Head rejected result", head: "lettuce.science", leaf: "prime", count: 3 }),
        makeNotice({ id: 2, level: "warn", message: "Disk allowance nearly full" }),
      ],
    });
    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.getByText("Needs attention")).toBeInTheDocument();
    expect(screen.getByText("Head rejected result")).toBeInTheDocument();
    expect(screen.getByText("Disk allowance nearly full")).toBeInTheDocument();
    expect(screen.getByText(/lettuce\.science \/ prime/)).toBeInTheDocument();
    expect(screen.getByText(/3 times/)).toBeInTheDocument();
    expect(screen.getByLabelText("Error")).toBeInTheDocument();
    expect(screen.getByLabelText("Warning")).toBeInTheDocument();

    // Panel sits above the task list.
    const panel = screen.getByLabelText("Notices");
    const tasksHeader = screen.getByText("Active Tasks");
    expect(panel.compareDocumentPosition(tasksHeader) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    await user.click(screen.getAllByLabelText("Dismiss notice")[0]);
    expect(dismiss).toHaveBeenCalledWith(expect.objectContaining({ id: 5 }));
  });

  it("hides the notices panel when the CLI build has no notices route", () => {
    mockUseNotices.mockReturnValue({
      supported: false,
      dismiss: vi.fn(),
      notices: [makeNotice({ id: 1, message: "ignored" })],
    });
    setupDefaultMocks();
    render(<OverviewPage />);
    expect(screen.queryByText("Needs attention")).not.toBeInTheDocument();
  });

  it("renders task with zero progress as indeterminate", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-zeropr0g-ress0",
              leaf_name: "Slow Starter",
              progress_pct: 0,
              elapsed_seconds: 60,
              cpu_seconds: 60,
            }),
          ],
          paused_reason: null,
        },
      },
    });

    render(<OverviewPage />);
    expect(screen.getByText("Slow Starter")).toBeInTheDocument();
    expect(screen.getByText("In progress...")).toBeInTheDocument();
  });

  it("renders checkpoint info on active task", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-chkpoint-test1",
              leaf_name: "Checkpoint Task",
              progress_pct: 50,
              elapsed_seconds: 600,
              cpu_seconds: 600,
              checkpoint_sequence: 3,
              last_checkpoint_at: new Date(Date.now() - 30000).toISOString(),
            }),
          ],
          paused_reason: null,
        },
      },
    });

    render(<OverviewPage />);
    expect(screen.getByText(/Checkpoint: seq 3/)).toBeInTheDocument();
    expect(screen.getByText(/saved/)).toBeInTheDocument();
  });

  it("renders resumed from checkpoint badge", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-resumed0-test1",
              leaf_name: "Resumed Task",
              progress_pct: 25,
              elapsed_seconds: 300,
              cpu_seconds: 300,
              resumed_from_checkpoint: true,
            }),
          ],
          paused_reason: null,
        },
      },
    });

    render(<OverviewPage />);
    expect(screen.getByText("Resumed from checkpoint")).toBeInTheDocument();
  });

  it("credit breakdown section expands to show head/leaf details", async () => {
    const user = userEvent.setup();

    setupDefaultMocks({
      credit: {
        credit: {
          total_credit: 5000,
          today: 50,
          this_week: 200,
          this_month: 800,
          by_leaf: [
            { leaf_id: "prime", leaf_name: "Prime Study", credit: 2000 },
            { leaf_id: "mandel", leaf_name: "Mandelbrot", credit: 1000 },
          ],
          by_head: [
            {
              head_name: "lettuce.science",
              volunteer_id: "vol-abcdef12",
              total_credit: 3000,
              available: true,
            },
          ],
          source: "head",
        },
      },
    });

    render(<OverviewPage />);

    // Click to expand
    await user.click(screen.getByText("Credit Breakdown"));

    expect(screen.getByText("lettuce.science")).toBeInTheDocument();
    expect(screen.getByText("3,000")).toBeInTheDocument();
    expect(screen.getByText("Prime Study")).toBeInTheDocument();
    expect(screen.getByText("Mandelbrot")).toBeInTheDocument();
  });

  it("credit breakdown section not rendered when there is nothing to break down", () => {
    setupDefaultMocks({
      credit: {
        credit: {
          total_credit: 0,
          today: 0,
          this_week: 0,
          this_month: 0,
          by_leaf: [],
          by_head: [],
          source: "local",
        },
      },
    });

    render(<OverviewPage />);
    expect(screen.queryByText("Credit Breakdown")).not.toBeInTheDocument();
  });

  it("renders estimated time remaining for tasks with known progress", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-estimat-test1",
              leaf_name: "Estimator Task",
              progress_pct: 50,
              elapsed_seconds: 600,
              cpu_seconds: 600,
              estimated_remaining_seconds: 600,
            }),
          ],
          paused_reason: null,
        },
      },
    });

    render(<OverviewPage />);
    // 50% in 600s -> remaining ~600s = 10m
    expect(screen.getByText(/remaining/)).toBeInTheDocument();
  });

  // --- Container runtime status indicator tests (S89) ---

  it("renders container runtime running indicator", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: {
        backend: "podman",
        status: "running",
        version: "5.3.1",
        socket_path: "/run/podman/podman.sock",
        machine_required: false,
        machine_name: "",
        machine_cpus: 0,
        machine_memory_mb: 0,
        machine_disk_gb: 0,
        error: null,
      },
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.getByText("Containers: Ready")).toBeInTheDocument();
  });

  it("renders container runtime not installed indicator with Setup link", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: {
        backend: "none",
        status: "not_installed",
        version: "",
        socket_path: "",
        machine_required: false,
        machine_name: "",
        machine_cpus: 0,
        machine_memory_mb: 0,
        machine_disk_gb: 0,
        error: null,
      },
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.getByText("Containers: Not installed")).toBeInTheDocument();
    expect(screen.getByText("Setup")).toBeInTheDocument();
  });

  it("renders container runtime unavailable indicator with Start link", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: {
        backend: "podman",
        status: "stopped",
        version: "5.3.1",
        socket_path: "/run/podman/podman.sock",
        machine_required: true,
        machine_name: "default",
        machine_cpus: 4,
        machine_memory_mb: 4096,
        machine_disk_gb: 50,
        error: null,
      },
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.getByText("Containers: Unavailable")).toBeInTheDocument();
    expect(screen.getByText("Start")).toBeInTheDocument();
  });

  it("does not render container status section when status is null", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: null,
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.queryByText(/Containers:/)).not.toBeInTheDocument();
  });

  it("does not show Start/Setup link when container runtime is running", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: {
        backend: "podman",
        status: "running",
        version: "5.3.1",
        socket_path: "/run/podman/podman.sock",
        machine_required: false,
        machine_name: "",
        machine_cpus: 0,
        machine_memory_mb: 0,
        machine_disk_gb: 0,
        error: null,
      },
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.queryByText("Setup")).not.toBeInTheDocument();
    expect(screen.queryByText("Start")).not.toBeInTheDocument();
  });

  // --- Coverage gap: container runtime "starting" status ---

  it("renders container runtime starting status as Unavailable with Start link", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: {
        backend: "podman",
        status: "starting",
        version: "5.3.1",
        socket_path: "",
        machine_required: true,
        machine_name: "default",
        machine_cpus: 4,
        machine_memory_mb: 4096,
        machine_disk_gb: 50,
        error: null,
      },
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.getByText("Containers: Unavailable")).toBeInTheDocument();
    expect(screen.getByText("Start")).toBeInTheDocument();
  });

  // --- Coverage gap: container runtime "error" status ---

  it("renders container runtime error status as Unavailable with Start link", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: {
        backend: "podman",
        status: "error",
        version: "5.3.1",
        socket_path: "",
        machine_required: true,
        machine_name: "default",
        machine_cpus: 4,
        machine_memory_mb: 4096,
        machine_disk_gb: 50,
        error: "Socket connection refused",
      },
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.getByText("Containers: Unavailable")).toBeInTheDocument();
    expect(screen.getByText("Start")).toBeInTheDocument();
  });

  // --- Coverage gap: container runtime "not_initialized" status ---

  it("renders container runtime not_initialized status as Unavailable with Start link", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: {
        backend: "podman",
        status: "not_initialized",
        version: "5.3.1",
        socket_path: "",
        machine_required: true,
        machine_name: "",
        machine_cpus: 0,
        machine_memory_mb: 0,
        machine_disk_gb: 0,
        error: null,
      },
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.getByText("Containers: Unavailable")).toBeInTheDocument();
    expect(screen.getByText("Start")).toBeInTheDocument();
  });

  // --- S102: ActiveTaskCard status dot, runtime badge, deadline, CPU time tests ---

  it("renders green status dot for running task", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-statusdot-run1",
              leaf_name: "Running Task",
              progress_pct: 30,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              task_status: "running",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Running")).toBeInTheDocument();
    // The status dot should have the green class
    const dot = document.querySelector(".bg-green-500.rounded-full");
    expect(dot).toBeInTheDocument();
  });

  it("renders yellow status dot and Suspended text for suspended_user task", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-statusdot-sus1",
              leaf_name: "Paused Task",
              progress_pct: 50,
              elapsed_seconds: 1000,
              cpu_seconds: 800,
              task_status: "suspended_user",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Suspended")).toBeInTheDocument();
    const dot = document.querySelector(".bg-yellow-500.rounded-full");
    expect(dot).toBeInTheDocument();
  });

  it("renders Suspended -- thermal text for suspended_thermal task", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-statusdot-thm1",
              leaf_name: "Hot Task",
              progress_pct: 40,
              elapsed_seconds: 800,
              cpu_seconds: 700,
              task_status: "suspended_thermal",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText(/Suspended.*thermal/)).toBeInTheDocument();
  });

  it("renders red status dot for error task", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-statusdot-err1",
              leaf_name: "Broken Task",
              progress_pct: 10,
              elapsed_seconds: 100,
              cpu_seconds: 100,
              task_status: "error",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Error")).toBeInTheDocument();
    const dot = document.querySelector(".bg-red-500.rounded-full");
    expect(dot).toBeInTheDocument();
  });

  it("renders Native runtime badge", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-runtime-natv1",
              leaf_name: "Native Task",
              progress_pct: 20,
              elapsed_seconds: 200,
              cpu_seconds: 200,
              runtime_type: "native",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Native")).toBeInTheDocument();
  });

  it("renders Container runtime badge", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-runtime-cntr1",
              leaf_name: "Container Task",
              progress_pct: 30,
              elapsed_seconds: 300,
              cpu_seconds: 300,
              runtime_type: "container",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("Container")).toBeInTheDocument();
  });

  it("renders WASM runtime badge", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-runtime-wasm1",
              leaf_name: "WASM Task",
              progress_pct: 40,
              elapsed_seconds: 400,
              cpu_seconds: 400,
              runtime_type: "wasm",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("WASM")).toBeInTheDocument();
  });

  it("renders CPU time (Crunching label)", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-cputime-test1",
              leaf_name: "Cruncher",
              progress_pct: 50,
              elapsed_seconds: 7200,
              cpu_seconds: 3600,
              task_status: "running",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    // cpu_seconds = 3600 -> "1h 0m"
    expect(screen.getByText(/Crunching: 1h 0m/)).toBeInTheDocument();
  });

  it("shows pause breakdown when task is suspended", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-paused0-test1",
              leaf_name: "Half Paused",
              progress_pct: 50,
              elapsed_seconds: 7200,
              cpu_seconds: 3600,
              task_status: "suspended_user",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    // Paused = elapsed - cpu = 7200 - 3600 = 3600 -> "1h 0m"
    expect(screen.getByText(/Paused 1h 0m/)).toBeInTheDocument();
  });

  it("renders deadline in muted color for tasks with >2h deadline", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-deadline-ok01",
              leaf_name: "Plenty of Time",
              progress_pct: 20,
              elapsed_seconds: 100,
              cpu_seconds: 100,
              deadline_seconds: 86400, // 24 hours
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    // 86400s = "24h 0m deadline"
    expect(screen.getByText(/24h 0m deadline/)).toBeInTheDocument();
    // The deadline text should have muted-foreground class (default)
    const deadlineEl = screen.getByText(/24h 0m deadline/);
    expect(deadlineEl.className).toContain("text-muted-foreground");
  });

  it("renders deadline in yellow for tasks with <2h deadline", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-deadline-urg1",
              leaf_name: "Hurry Up",
              progress_pct: 80,
              elapsed_seconds: 3000,
              cpu_seconds: 3000,
              deadline_seconds: 3600, // 1 hour — less than 7200
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    const deadlineEl = screen.getByText(/1h 0m deadline/);
    expect(deadlineEl.className).toContain("text-yellow-500");
  });

  it("renders overdue deadline in red", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-deadline-late",
              leaf_name: "Late Task",
              progress_pct: 90,
              elapsed_seconds: 5000,
              cpu_seconds: 5000,
              deadline_seconds: -3600, // overdue by 1h
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    const deadlineEl = screen.getByText(/Overdue by 1h 0m/);
    expect(deadlineEl.className).toContain("text-red-500");
  });

  it("renders progress bar with correct width style", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-progbar-test1",
              leaf_name: "Progress Bar Task",
              progress_pct: 42,
              elapsed_seconds: 420,
              cpu_seconds: 420,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    // The progress bar inner element should have width: 42%
    const progressBar = document.querySelector('[style*="width: 42%"]');
    expect(progressBar).toBeInTheDocument();
    expect(screen.getByText("42%")).toBeInTheDocument();
  });

  it("renders work unit ID prefix (first 8 chars)", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "abcdefgh-1234-5678-90ab-cdef12345678",
              leaf_name: "ID Display",
              progress_pct: 10,
              elapsed_seconds: 100,
              cpu_seconds: 100,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText("abcdefgh")).toBeInTheDocument();
  });

  it("shows Resume in overflow menu for suspended task", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-menu-sus-test",
              leaf_name: "Suspended Menu",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 400,
              task_status: "suspended_user",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    // The overflow menu trigger contains "···"
    const trigger = screen.getByText("···");
    expect(trigger).toBeInTheDocument();
  });

  // --- S102 CRITICAL: Overflow menu interaction tests ---

  it("calls client.suspendTask when Suspend is clicked in overflow menu", async () => {
    const user = userEvent.setup();
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-suspend-click1",
              leaf_name: "Suspendable Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              task_status: "running",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Open the dropdown menu
    await user.click(screen.getByText("···"));
    // Click Suspend
    const suspendItem = await screen.findByText("Suspend");
    await user.click(suspendItem);
    expect(mockClient.suspendTask).toHaveBeenCalledWith("wu-suspend-click1");
  });

  it("calls client.resumeTask when Resume is clicked in overflow menu for suspended task", async () => {
    const user = userEvent.setup();
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-resume-click1",
              leaf_name: "Resumable Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 400,
              task_status: "suspended_user",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Open the dropdown menu
    await user.click(screen.getByText("···"));
    // Click Resume
    const resumeItem = await screen.findByText("Resume");
    await user.click(resumeItem);
    expect(mockClient.resumeTask).toHaveBeenCalledWith("wu-resume-click1");
  });

  it("opens abort confirm dialog and calls client.abortTask on confirm", async () => {
    const user = userEvent.setup();
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-abort-click01",
              leaf_name: "Abortable Task",
              progress_pct: 30,
              elapsed_seconds: 300,
              cpu_seconds: 300,
              task_status: "running",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Open the dropdown menu
    await user.click(screen.getByText("···"));
    // Click Abort (should open the ConfirmDialog, not call abortTask yet)
    const abortItem = await screen.findByRole("menuitem", { name: "Abort" });
    await user.click(abortItem);

    // The ConfirmDialog should now be open
    expect(await screen.findByText("Abort this task?")).toBeInTheDocument();
    expect(screen.getByText("This will kill the process and the work unit will be reassigned.")).toBeInTheDocument();

    // abortTask should NOT have been called yet
    expect(mockClient.abortTask).not.toHaveBeenCalled();

    // Confirm the abort
    const confirmButton = screen.getByRole("button", { name: "Abort" });
    await user.click(confirmButton);
    expect(mockClient.abortTask).toHaveBeenCalledWith("wu-abort-click01");
  });

  it("does not call abortTask when Cancel is clicked in abort confirm dialog", async () => {
    const user = userEvent.setup();
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-abort-cancel1",
              leaf_name: "Keep Me Running",
              progress_pct: 70,
              elapsed_seconds: 700,
              cpu_seconds: 700,
              task_status: "running",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Open the dropdown menu and click Abort
    await user.click(screen.getByText("···"));
    const abortItem = await screen.findByRole("menuitem", { name: "Abort" });
    await user.click(abortItem);

    // Cancel the dialog
    const cancelButton = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelButton);

    // abortTask should never be called
    expect(mockClient.abortTask).not.toHaveBeenCalled();
  });

  it("calls navigator.clipboard.writeText when Copy Work Unit ID is clicked", async () => {
    const user = userEvent.setup();
    const writeTextMock = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: writeTextMock },
      writable: true,
      configurable: true,
    });

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-copy-id-test1",
              leaf_name: "Copy ID Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              task_status: "running",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Open the dropdown menu
    await user.click(screen.getByText("···"));
    // Click Copy Work Unit ID
    const copyItem = await screen.findByText("Copy Work Unit ID");
    await user.click(copyItem);
    expect(writeTextMock).toHaveBeenCalledWith("wu-copy-id-test1");
  });

  // --- S102: Edge case — unknown task_status fallback ---

  it("falls back to gray dot and raw status text for unknown task_status", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-unknown-stat1",
              leaf_name: "Mystery Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              task_status: "some_future_status" as any,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    // Should display raw status text as fallback
    expect(screen.getByText("some_future_status")).toBeInTheDocument();
    // Should use gray dot fallback
    const dots = document.querySelectorAll(".rounded-full");
    const grayDot = Array.from(dots).find(d => d.classList.contains("bg-gray-500") && d.classList.contains("h-2.5"));
    expect(grayDot).toBeTruthy();
  });

  // --- S102: Edge case — suspended_scheduled status ---

  it("renders Suspended -- scheduled text for suspended_scheduled task", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-sched-susp001",
              leaf_name: "Scheduled Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 400,
              task_status: "suspended_scheduled",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);
    expect(screen.getByText(/Suspended.*scheduled/)).toBeInTheDocument();
  });

  // --- S103: View toggle tests ---

  it("shows view toggle buttons when there are active tasks", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-toggle-show01",
              leaf_name: "Toggle Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    expect(screen.getByTitle("Card view")).toBeInTheDocument();
    expect(screen.getByTitle("Table view")).toBeInTheDocument();
  });

  it("does not show view toggle buttons when there are no active tasks", () => {
    setupDefaultMocks();
    render(<OverviewPage />);

    expect(screen.queryByTitle("Card view")).not.toBeInTheDocument();
    expect(screen.queryByTitle("Table view")).not.toBeInTheDocument();
  });

  it("defaults to card view", () => {
    // Clear any saved preference
    localStorage.removeItem("lettuce-task-view-mode");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-default-view1",
              leaf_name: "Card View Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // In card view, the "Crunching:" label is present (from ActiveTaskCard)
    expect(screen.getByText(/Crunching:/)).toBeInTheDocument();
    // Table headers should NOT be present
    expect(screen.queryByText("WU ID")).not.toBeInTheDocument();
  });

  it("switches to table view when table toggle is clicked", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("lettuce-task-view-mode");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-to-table-test",
              leaf_name: "Table View Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Click the table view toggle
    await user.click(screen.getByTitle("Table view"));

    // Table headers should now be visible
    expect(screen.getByText("Leaf")).toBeInTheDocument();
    expect(screen.getByText("WU ID")).toBeInTheDocument();
    expect(screen.getByText("CPU Time")).toBeInTheDocument();
    // Card-specific text should NOT be present
    expect(screen.queryByText(/Crunching:/)).not.toBeInTheDocument();
  });

  it("switches back to card view when card toggle is clicked", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("lettuce-task-view-mode");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-back-to-card1",
              leaf_name: "Back to Cards",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Switch to table
    await user.click(screen.getByTitle("Table view"));
    expect(screen.getByText("WU ID")).toBeInTheDocument();

    // Switch back to cards
    await user.click(screen.getByTitle("Card view"));
    expect(screen.getByText(/Crunching:/)).toBeInTheDocument();
    expect(screen.queryByText("WU ID")).not.toBeInTheDocument();
  });

  it("persists view mode to localStorage", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("lettuce-task-view-mode");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-persist-mode1",
              leaf_name: "Persist Mode",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Switch to table
    await user.click(screen.getByTitle("Table view"));
    expect(localStorage.getItem("lettuce-task-view-mode")).toBe("table");

    // Switch back to cards
    await user.click(screen.getByTitle("Card view"));
    expect(localStorage.getItem("lettuce-task-view-mode")).toBe("cards");
  });

  it("restores table view from localStorage", () => {
    localStorage.setItem("lettuce-task-view-mode", "table");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-restore-table",
              leaf_name: "Restored Table",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Should be in table mode already
    expect(screen.getByText("WU ID")).toBeInTheDocument();
    expect(screen.queryByText(/Crunching:/)).not.toBeInTheDocument();
  });

  it("renders table view with task data when in table mode", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("lettuce-task-view-mode");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 2,
          active_tasks: [
            makeTask({
              work_unit_id: "aaaaaaaa-1234-5678-abcd-000000000001",
              leaf_name: "Alpha Study",
              progress_pct: 25,
              elapsed_seconds: 600,
              cpu_seconds: 600,
              task_status: "running",
            }),
            makeTask({
              work_unit_id: "bbbbbbbb-1234-5678-abcd-000000000002",
              leaf_name: "Beta Analysis",
              progress_pct: 75,
              elapsed_seconds: 1200,
              cpu_seconds: 1200,
              task_status: "suspended_user",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Switch to table view
    await user.click(screen.getByTitle("Table view"));

    // Both tasks should appear in table
    expect(screen.getByText("Alpha Study")).toBeInTheDocument();
    expect(screen.getByText("Beta Analysis")).toBeInTheDocument();
    expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    expect(screen.getByText("bbbbbbbb")).toBeInTheDocument();
  });

  // --- S103: Right-click on task cards ---

  it("opens context menu when right-clicking on a task card", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("lettuce-task-view-mode");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-rclick-card01",
              leaf_name: "Right Click Me",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              task_status: "running",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Right-click on the card — find the card container element
    const cardText = screen.getByText("Right Click Me");
    const card = cardText.closest("[class*='group']")!;
    await user.pointer({ keys: "[MouseRight]", target: card });

    // Context menu should open with task-specific actions
    expect(await screen.findByRole("menuitem", { name: "Abort" })).toBeInTheDocument();
    expect(screen.getByText("Show Details")).toBeInTheDocument();
    expect(screen.getByText("Copy Work Unit ID")).toBeInTheDocument();
  });

  // --- S105: Viz selection, Viz badge, ring highlight, auto-clear ---

  it("renders Viz badge on task cards with viz_bundle_path", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-vizbadge-test",
              leaf_name: "Viz Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              viz_bundle_path: "/path/to/.lettuce-viz",
              work_dir: "/tmp/wu-viz",
            }),
            makeTask({
              work_unit_id: "wu-noviz-badge01",
              leaf_name: "No Viz Task",
              progress_pct: 30,
              elapsed_seconds: 300,
              cpu_seconds: 300,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // "Viz" badge should appear on the viz-capable task
    const vizBadges = screen.getAllByText("Viz");
    expect(vizBadges.length).toBeGreaterThanOrEqual(1);
  });

  it("does not render Viz badge on task without viz_bundle_path", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-noviz-only001",
              leaf_name: "Plain Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              viz_bundle_path: null,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    expect(screen.queryByText("Viz")).not.toBeInTheDocument();
  });

  it("applies ring highlight to the currently visualized task card", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-vizring-test1",
              leaf_name: "Highlighted Viz Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              viz_bundle_path: "/path/to/.lettuce-viz",
              work_dir: "/tmp/wu-viz",
            }),
            makeTask({
              work_unit_id: "wu-vizring-test2",
              leaf_name: "Other Task",
              progress_pct: 30,
              elapsed_seconds: 300,
              cpu_seconds: 300,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // The viz task card should have the ring highlight class
    const vizTaskCard = screen.getByText("Highlighted Viz Task").closest("[class*='group']");
    expect(vizTaskCard?.className).toContain("ring-2");
    expect(vizTaskCard?.className).toContain("ring-primary/30");

    // The non-viz task should NOT have ring
    const otherCard = screen.getByText("Other Task").closest("[class*='group']");
    expect(otherCard?.className).not.toContain("ring-2");
  });

  it("clicking a viz-capable task card in card view updates viz selection", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("lettuce-task-view-mode");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-vizclick-aaa1",
              leaf_name: "First Viz",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              viz_bundle_path: "/path/to/.lettuce-viz",
              work_dir: "/tmp/wu-viz-a",
            }),
            makeTask({
              work_unit_id: "wu-vizclick-bbb2",
              leaf_name: "Second Viz",
              progress_pct: 30,
              elapsed_seconds: 300,
              cpu_seconds: 300,
              viz_bundle_path: "/path/to/.lettuce-viz-b",
              work_dir: "/tmp/wu-viz-b",
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Initially, the first viz task should be highlighted (fallback)
    const firstCard = screen.getByText("First Viz").closest("[class*='group']");
    expect(firstCard?.className).toContain("ring-2");

    // Click on second viz task
    await user.click(screen.getByText("Second Viz"));

    // Now second card should have the ring (since we selected it)
    const secondCard = screen.getByText("Second Viz").closest("[class*='group']");
    expect(secondCard?.className).toContain("ring-2");
  });

  it("shows viz frame placeholder when no tasks have viz", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-noviz-frame01",
              leaf_name: "Non-viz Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              viz_bundle_path: null,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // Should show the "Computing in progress..." placeholder
    expect(screen.getByText("Computing in progress...")).toBeInTheDocument();
  });

  // --- S105 CRITICAL: auto-clear selectedVizTaskId when selected task disappears ---

  it("auto-clears viz selection when selected task is removed from active tasks", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("lettuce-task-view-mode");

    const taskA = makeTask({
      work_unit_id: "wu-autoclear-aaa",
      leaf_name: "Viz Task A",
      progress_pct: 50,
      elapsed_seconds: 500,
      cpu_seconds: 500,
      viz_bundle_path: "/path/to/.lettuce-viz-a",
      work_dir: "/tmp/wu-viz-a",
    });
    const taskB = makeTask({
      work_unit_id: "wu-autoclear-bbb",
      leaf_name: "Viz Task B",
      progress_pct: 30,
      elapsed_seconds: 300,
      cpu_seconds: 300,
      viz_bundle_path: "/path/to/.lettuce-viz-b",
      work_dir: "/tmp/wu-viz-b",
    });

    // Start with both tasks
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [taskA, taskB],
          paused_reason: null,
        },
      },
    });
    const { rerender } = render(<OverviewPage />);

    // Click task B to select it
    await user.click(screen.getByText("Viz Task B"));

    // Verify B is highlighted
    const bCard = screen.getByText("Viz Task B").closest("[class*='group']");
    expect(bCard?.className).toContain("ring-2");

    // Now re-render with task B removed (simulating it completing)
    mockUseDaemonStatus.mockReturnValue({
      status: {
        state: "active",
        uptime_seconds: 3600,
        connected_servers: 1,
        active_tasks: [taskA], // B is gone
        paused_reason: null,
      },
      isLoading: false,
      error: null,
      refetch: vi.fn(),
    });
    rerender(<OverviewPage />);

    // A should now be the viz task (fallback) and have the ring
    const aCard = screen.getByText("Viz Task A").closest("[class*='group']");
    expect(aCard?.className).toContain("ring-2");
  });

  // --- S105 CRITICAL: clicking non-viz task does NOT change viz selection ---

  it("clicking a non-viz task does not change viz selection", async () => {
    const user = userEvent.setup();
    localStorage.removeItem("lettuce-task-view-mode");

    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [
            makeTask({
              work_unit_id: "wu-vizonly-aaa01",
              leaf_name: "Viz Capable Task",
              progress_pct: 50,
              elapsed_seconds: 500,
              cpu_seconds: 500,
              viz_bundle_path: "/path/to/.lettuce-viz",
              work_dir: "/tmp/wu-viz",
            }),
            makeTask({
              work_unit_id: "wu-noviz-click01",
              leaf_name: "Plain Task",
              progress_pct: 30,
              elapsed_seconds: 300,
              cpu_seconds: 300,
              viz_bundle_path: null,
            }),
          ],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    // The viz-capable task should have the ring initially
    const vizCard = screen.getByText("Viz Capable Task").closest("[class*='group']");
    expect(vizCard?.className).toContain("ring-2");

    // Click the plain (non-viz) task
    await user.click(screen.getByText("Plain Task"));

    // The viz-capable task should still have the ring (viz selection unchanged)
    const vizCardAfter = screen.getByText("Viz Capable Task").closest("[class*='group']");
    expect(vizCardAfter?.className).toContain("ring-2");

    // The plain task should NOT have the ring
    const plainCard = screen.getByText("Plain Task").closest("[class*='group']");
    expect(plainCard?.className).not.toContain("ring-2");
  });

  // --- S105: idle state with no tasks shows correct viz placeholder ---

  it("shows 'Start computing' placeholder when no tasks exist", () => {
    setupDefaultMocks({
      status: {
        status: {
          state: "active",
          uptime_seconds: 3600,
          connected_servers: 1,
          active_tasks: [],
          paused_reason: null,
        },
      },
    });
    render(<OverviewPage />);

    expect(screen.getByText("Start computing to see a simulation")).toBeInTheDocument();
  });

  // --- TB-69: a bundle with no live view collapses the panel to one line ---

  describe("TB-69: the panel collapses when the shown unit's bundle has no live view", () => {
    /** Dispatch a message as the frame's page would post it (source = the frame's window). */
    function postFromFrame(data: unknown) {
      const iframe = document.querySelector("iframe");
      expect(iframe?.contentWindow).toBeTruthy();
      window.dispatchEvent(new MessageEvent("message", { data, source: iframe!.contentWindow }));
    }

    function twoVizUnits() {
      return [
        makeTask({
          work_unit_id: "wu-tb69-replay-only",
          leaf_name: "Beyblade Arena",
          viz_bundle_path: "/w1/.lettuce-viz",
          work_dir: "/w1",
        }),
        makeTask({
          work_unit_id: "wu-tb69-second-unit",
          leaf_name: "Other Leaf",
          viz_bundle_path: "/w2/.lettuce-viz",
          work_dir: "/w2",
        }),
      ];
    }

    function setupWithVizUnits() {
      setupDefaultMocks({
        status: {
          status: {
            state: "active",
            uptime_seconds: 10,
            connected_servers: 1,
            active_tasks: twoVizUnits(),
            paused_reason: null,
          },
        },
      });
    }

    it("replaces the 320 px frame with a one-line note naming the leaf when the page declares replay only", async () => {
      setupWithVizUnits();
      render(<OverviewPage />);
      await waitFor(() => expect(document.querySelector("iframe")).toBeTruthy());

      await act(async () => {
        postFromFrame({ type: "vizReady", modes: ["replay"] });
      });

      const note = screen.getByTestId("viz-unavailable-note");
      expect(note).toHaveTextContent(
        "Beyblade Arena has no live view. Finished units can be replayed from History."
      );
      expect(note).toHaveStyle({ height: "80px" });
      expect(document.querySelector("iframe")).toBeNull();
      // The frame's own in-place note went with the frame; the page shows one line, not two.
      expect(screen.queryByTestId("viz-unavailable")).toBeNull();
      expect(screen.queryByText("Computing in progress...")).not.toBeInTheDocument();
    });

    it("switching to another unit mounts a fresh frame with no note", async () => {
      setupWithVizUnits();
      const user = userEvent.setup();
      render(<OverviewPage />);
      await waitFor(() => expect(document.querySelector("iframe")).toBeTruthy());

      await act(async () => {
        postFromFrame({ type: "vizReady", modes: ["replay"] });
      });
      expect(screen.getByTestId("viz-unavailable-note")).toBeInTheDocument();

      await user.click(screen.getByText("Other Leaf"));
      await waitFor(() => expect(document.querySelector("iframe")).toBeTruthy());
      expect(screen.queryByTestId("viz-unavailable-note")).toBeNull();
    });

    it("a page that never starts collapses the panel to the could-not-start note", async () => {
      vi.useFakeTimers();
      try {
        setupWithVizUnits();
        await act(async () => {
          render(<OverviewPage />);
          await vi.advanceTimersByTimeAsync(0);
        });
        const iframe = document.querySelector("iframe");
        expect(iframe).toBeTruthy();

        await act(async () => {
          iframe!.dispatchEvent(new Event("load"));
          await vi.advanceTimersByTimeAsync(VIZ_SILENT_TIMEOUT_MS);
        });

        expect(screen.getByTestId("viz-unavailable-note")).toHaveTextContent(
          "The visualization for Beyblade Arena could not start on this machine."
        );
        expect(document.querySelector("iframe")).toBeNull();
      } finally {
        vi.useRealTimers();
      }
    });
  });
  describe("TB-72: a pause the daemon will not undo offers no Resume", () => {
    function pausedFor(reason: "user" | "scheduled" | "thermal") {
      setupDefaultMocks({
        status: {
          status: {
            state: "paused",
            uptime_seconds: 3600,
            connected_servers: 1,
            active_tasks: [],
            queued_tasks: [],
            failing_leafs: [],
            paused_reason: reason,
          },
        },
      });
    }

    it("shows Change schedule instead of Resume during a schedule pause, and it opens Settings", async () => {
      const user = userEvent.setup();
      pausedFor("scheduled");
      render(<OverviewPage />);

      expect(screen.queryByText("Resume")).not.toBeInTheDocument();
      expect(screen.queryByText("Pause")).not.toBeInTheDocument();
      expect(screen.getByText(/Computing is paused by your schedule/)).toBeInTheDocument();

      await user.click(screen.getByRole("button", { name: "Change schedule" }));
      expect(emit).toHaveBeenCalledWith("navigate:settings");
      expect(mockClient.resume).not.toHaveBeenCalled();
    });

    it("offers nothing to click during a thermal pause and says it ends on its own", () => {
      pausedFor("thermal");
      render(<OverviewPage />);

      expect(screen.queryByText("Resume")).not.toBeInTheDocument();
      expect(screen.queryByText("Change schedule")).not.toBeInTheDocument();
      expect(screen.getByText(/cools down/)).toBeInTheDocument();
    });

    it("shows the daemon's own reason when a resume is refused", async () => {
      const user = userEvent.setup();
      pausedFor("user");
      mockClient.resume.mockRejectedValueOnce(new ApiError("CONFLICT", "not paused", 409));
      render(<OverviewPage />);

      await user.click(screen.getByText("Resume"));
      expect(await screen.findByRole("alert")).toHaveTextContent("Could not resume: not paused");
    });
  });
});
