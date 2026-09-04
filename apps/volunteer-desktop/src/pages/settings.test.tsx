import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, renderHook } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SettingsPage } from "./settings";
import { defaultCommandResult, invoke, mockManagementApi } from "@tauri-apps/api/core";
import type { ConfigResponse } from "@/api/client";

// Mock hooks
vi.mock("@/hooks/use-config", () => ({
  useConfig: vi.fn(),
}));

const mockUseSystemMetrics = vi.fn();
vi.mock("@/hooks/use-metrics", () => ({
  useMetrics: vi.fn(),
  useSystemMetrics: () => mockUseSystemMetrics(),
}));

// Mock lucide-react icons
vi.mock("lucide-react", () => ({
  ChevronDown: (props: any) => <span data-testid="chevron-down" {...props} />,
  ChevronRight: (props: any) => <span data-testid="chevron-right" {...props} />,
  Copy: (props: any) => <span data-testid="copy-icon" {...props} />,
  Check: (props: any) => <span data-testid="check-icon" {...props} />,
  AlertTriangle: (props: any) => <span data-testid="alert-icon" {...props} />,
  RefreshCw: (props: any) => <span data-testid="refresh-icon" {...props} />,
  Monitor: (props: any) => <span data-testid="monitor-icon" {...props} />,
  Sun: (props: any) => <span data-testid="sun-icon" {...props} />,
  Moon: (props: any) => <span data-testid="moon-icon" {...props} />,
}));

// Mock the schedule builder (complex component, tested separately)
vi.mock("@/components/schedule-builder", () => ({
  ScheduleBuilder: (props: any) => (
    <div data-testid="schedule-builder" data-mode={props.mode}>
      Schedule Builder Mock
    </div>
  ),
}));

// Mock the container runtime status card (tested separately)
vi.mock("@/components/container-runtime-status", () => ({
  ContainerRuntimeStatusCard: () => (
    <div data-testid="container-runtime-status-card">
      Container Runtime Status Mock
    </div>
  ),
}));

import { useConfig } from "@/hooks/use-config";
import { useMetrics } from "@/hooks/use-metrics";

const mockUseConfig = useConfig as ReturnType<typeof vi.fn>;
const mockUseMetrics = useMetrics as ReturnType<typeof vi.fn>;

function makeConfig(overrides: Partial<ConfigResponse> = {}): ConfigResponse {
  return {
    data_dir: "/home/test/.lettuce",
    public_key: "test-public-key-base64url",
    resource_limits: {
      max_cpu_cores: 4,
      max_memory_mb: 2048,
      max_disk_gb: 10,
      max_bandwidth_mbps: 0,
      max_gpu_vram_pct: 50,
      max_pids: 0,
    },
    scheduling: {
      mode: "ALWAYS",
      idle_threshold_mins: 5,
      cron_expression: "",
    },
    leafs: {
      mode: "ALL",
      leaf_ids: [],
      blocked_ids: [],
    },
    thermal: {
      enabled: true,
      cpu_pause_threshold: 85,
      cpu_resume_threshold: 75,
      gpu_pause_threshold: 80,
      gpu_resume_threshold: 70,
      poll_interval_seconds: 10,
      max_throttle_minutes: 30,
    },
    notifications: {
      credit_milestones: true,
      credit_milestone_threshold: 100,
      work_unit_completed: false,
      errors: true,
      updates: true,
    },
    servers: [],
    log_level: "info",
    max_concurrent_tasks: 1,
    work_buffer_hours: 2,
    ...overrides,
  };
}

const noGpuMachine = {
  runtimes: ["wasm"],
  has_gpu: false,
  max_memory_mb: 2048,
  max_disk_mb: 10240,
  max_cpu_cores: 4,
  max_gpu_vram_mb: 0,
  gpu_card_vram_mb: 0,
  gpu_vram_pct: 0,
  gpu_vendors: [],
  gpu_compute_capabilities: [],
};

/** Route the daemon's `/api/v1/heads` (machine capabilities and per-head ids). */
function mockHeads(resp: { heads: unknown[]; machine: typeof noGpuMachine }) {
  mockManagementApi({ "GET /api/v1/heads": resp, "GET /api/v1/status": {} });
}

describe("SettingsPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.removeItem("lettuce-theme");
    mockUseMetrics.mockReturnValue({
      metrics: null,
      isLoading: false,
      error: null,
    });
    mockUseSystemMetrics.mockReturnValue({
      system: { cpu_usage_pct: 12, memory_used_mb: 4096, memory_total_mb: 16384 },
      error: null,
    });
    mockHeads({ heads: [], machine: noGpuMachine });
  });

  it("renders loading skeleton when config is loading", () => {
    mockUseConfig.mockReturnValue({
      config: null,
      isLoading: true,
      updateConfig: vi.fn(),
      toast: null,
    });

    const { container } = render(<SettingsPage />);
    const skeletons = container.querySelectorAll(".animate-pulse");
    expect(skeletons.length).toBeGreaterThan(0);
  });

  it("renders Resource Limits section", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Resource Limits")).toBeInTheDocument();
  });

  it("renders CPU cores slider", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("CPU Cores")).toBeInTheDocument();
  });

  it("renders Memory slider", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Memory")).toBeInTheDocument();
  });

  it("renders GPU allowance slider", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("GPU allowance")).toBeInTheDocument();
    expect(screen.queryByText("GPU VRAM")).not.toBeInTheDocument();
  });

  it("explains the GPU allowance arithmetic from the machine capabilities", async () => {
    mockHeads({
      heads: [],
      machine: {
        ...noGpuMachine,
        has_gpu: true,
        gpu_vendors: ["NVIDIA"],
        gpu_card_vram_mb: 8192,
        gpu_vram_pct: 50,
        max_gpu_vram_mb: 4096,
      },
    });
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    await waitFor(() => {
      expect(
        screen.getByText(/your card 8\.0 GB × 50% = 4\.0 GB allowed/)
      ).toBeInTheDocument();
    });
    expect(screen.queryByText("No GPU detected")).not.toBeInTheDocument();
  });

  it("sizes the memory slider from the app's own memory measurement", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("2.0 GB / 16.0 GB")).toBeInTheDocument();
  });

  it("explains the disk allowance and the 2 GB headroom rule", () => {
    mockUseMetrics.mockReturnValue({
      metrics: {
        cpu_usage_pct: 0,
        gpu_usage_pct: 0,
        memory_used_mb: 0,
        memory_total_mb: 0,
        cpu_temp_c: 0,
        gpu_temp_c: 0,
        disk_used_mb: 3072,
        disk_allowance_mb: 10240,
        disk_usage_known: true,
      },
      isLoading: false,
      error: null,
    });
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(
      screen.getByText(/A leaf is fetched only when its declared need plus 2 GB of headroom fits/)
    ).toBeInTheDocument();
    expect(screen.getByText(/Lettuce is using 3\.0 GB right now/)).toBeInTheDocument();
  });

  it("shows the daemon's work buffer in hours (0–12, step 0.5) and saves work_buffer_hours", () => {
    const updateConfig = vi.fn();
    mockUseConfig.mockReturnValue({
      config: makeConfig({ work_buffer_hours: 4.5 }),
      isLoading: false,
      updateConfig,
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Work buffer")).toBeInTheDocument();
    expect(screen.getByText("4.5 h of work per task")).toBeInTheDocument();
    expect(screen.getByText(/How many hours of work to keep fetched ahead/)).toBeInTheDocument();

    const sliders = screen.getAllByRole("slider") as HTMLInputElement[];
    const buffer = sliders.find((el) => el.min === "0" && el.max === "12" && el.step === "0.5");
    expect(buffer).toBeDefined();
    fireEvent.change(buffer!, { target: { value: "6" } });
    expect(updateConfig).toHaveBeenCalledWith({ work_buffer_hours: 6 });
  });

  it("describes a zero work buffer as the daemon's fixed unit-count fallback", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig({ work_buffer_hours: 0 }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Fixed unit count (daemon fallback)")).toBeInTheDocument();
  });

  it("renders the thermal section and saves a threshold on blur", async () => {
    const user = userEvent.setup();
    const updateConfig = vi.fn();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig,
      toast: null,
    });

    render(<SettingsPage />);
    await user.click(screen.getByText("Thermal"));

    expect(screen.getByText("Pause when hot")).toBeInTheDocument();
    expect(screen.getByLabelText("CPU pause above")).toHaveValue(85);
    expect(screen.getByLabelText("CPU resume below")).toHaveValue(75);
    expect(screen.getByLabelText("GPU pause above")).toHaveValue(80);
    expect(screen.getByLabelText("GPU resume below")).toHaveValue(70);
    expect(screen.getByLabelText("Check every")).toHaveValue(10);
    expect(screen.getByLabelText("Longest thermal pause")).toHaveValue(30);
    expect(
      screen.getByText(/Work is never released while the sensor still reads above the resume temperature/)
    ).toBeInTheDocument();

    const field = screen.getByLabelText("Longest thermal pause");
    await user.clear(field);
    await user.type(field, "45");
    // Nothing is saved while typing.
    expect(updateConfig).not.toHaveBeenCalled();
    await user.tab();
    expect(updateConfig).toHaveBeenCalledWith({
      thermal: expect.objectContaining({ max_throttle_minutes: 45, cpu_pause_threshold: 85 }),
    });
  });

  it("toggles thermal monitoring", async () => {
    const user = userEvent.setup();
    const updateConfig = vi.fn();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig,
      toast: null,
    });

    render(<SettingsPage />);
    await user.click(screen.getByText("Thermal"));
    await user.click(screen.getByRole("switch", { name: "Pause when hot" }));
    expect(updateConfig).toHaveBeenCalledWith({
      thermal: expect.objectContaining({ enabled: false }),
    });
  });

  it("restarts Lettuce from the General section and clears any pending restart notice", async () => {
    const { invoke } = await import("@tauri-apps/api/core");
    const mockInvoke = invoke as ReturnType<typeof vi.fn>;
    mockInvoke.mockImplementation((cmd: string) => {
      if (cmd === "is_autostart_enabled") return Promise.resolve(false);
      if (cmd === "restart_daemon") return Promise.resolve(undefined);
      if (cmd === "mgmt_request") return Promise.resolve({});
      return Promise.resolve(undefined);
    });
    const { markRestartRequired, resetRestartRequiredForTest, useRestartRequired } =
      await import("@/hooks/use-restart-required");
    resetRestartRequiredForTest();
    markRestartRequired("A setting is waiting.");
    const { result: restart } = renderHook(() => useRestartRequired());
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
      refetch: vi.fn(),
    });

    const user = userEvent.setup();
    render(<SettingsPage />);
    // The notice itself lives in the tab layout, not on this page.
    expect(screen.queryByText("Restart required")).not.toBeInTheDocument();

    await user.click(screen.getByText("General"));
    await user.click(screen.getByRole("button", { name: /Restart Lettuce/ }));
    await waitFor(() => {
      expect(mockInvoke).toHaveBeenCalledWith("restart_daemon");
    });
    await waitFor(() => {
      expect(screen.getByText("Lettuce restarted.")).toBeInTheDocument();
    });
    expect(restart.current.restartRequired).toBe(false);
  });

  it("reports a failed restart", async () => {
    const { invoke } = await import("@tauri-apps/api/core");
    const mockInvoke = invoke as ReturnType<typeof vi.fn>;
    mockInvoke.mockImplementation((cmd: string) => {
      if (cmd === "restart_daemon") return Promise.reject(new Error("Timed out waiting for the restarted daemon to start"));
      if (cmd === "mgmt_request") return Promise.resolve({});
      return Promise.resolve(undefined);
    });
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
      refetch: vi.fn(),
    });

    const user = userEvent.setup();
    render(<SettingsPage />);
    await user.click(screen.getByText("General"));
    await user.click(screen.getByRole("button", { name: /Restart Lettuce/ }));
    await waitFor(() => {
      expect(screen.getByText(/Restart failed: Timed out/)).toBeInTheDocument();
    });
  });

  it("explains the account model and lists per-head volunteer ids", async () => {
    mockHeads({
      heads: [
        { name: "lettuce.science", grpc_address: "a:1", status: "connected", weight: 100, leafs: [], volunteer_id: "vol-11111111" },
        { name: "pending.example", grpc_address: "b:1", status: "disconnected", weight: 100, leafs: [] },
      ],
      machine: noGpuMachine,
    });
    mockUseConfig.mockReturnValue({
      config: makeConfig({ public_key: "abc123-test-key" }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    const user = userEvent.setup();
    render(<SettingsPage />);
    await user.click(screen.getByText("Identity"));

    expect(screen.getByText(/Your keypair is your account/)).toBeInTheDocument();
    expect(screen.getByText(/up to 10 machines/)).toBeInTheDocument();
    expect(screen.getByText("identity.key")).toBeInTheDocument();
    expect(screen.getByText("identity.pub")).toBeInTheDocument();
    expect(screen.getByText(/Never re-run setup to fix a key problem/)).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.getByText("vol-11111111")).toBeInTheDocument();
    });
    expect(screen.getByText("lettuce.science")).toBeInTheDocument();
    expect(screen.getByText("pending.example")).toBeInTheDocument();
    expect(screen.getByText("not registered yet")).toBeInTheDocument();
  });

  it("shows the data directory the app resolved, falling back to the daemon's", async () => {
    mockHeads({ heads: [], machine: noGpuMachine });
    mockManagementApi(
      { "GET /api/v1/heads": { heads: [], machine: noGpuMachine }, "GET /api/v1/status": {} },
      (cmd) => (cmd === "get_data_dir" ? "D:\profiles\second" : undefined)
    );
    mockUseConfig.mockReturnValue({
      config: makeConfig({ data_dir: "/home/test/.lettuce" }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    const user = userEvent.setup();
    render(<SettingsPage />);
    await user.click(screen.getByText("Identity"));

    expect(screen.getByText("Data directory")).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText("D:\profiles\second")).toBeInTheDocument();
    });
    expect(screen.queryByText("/home/test/.lettuce")).not.toBeInTheDocument();
    expect(screen.getByText(/from the data directory above/)).toBeInTheDocument();
  });

  it("falls back to the daemon's data_dir when the host cannot report one", async () => {
    mockManagementApi(
      { "GET /api/v1/heads": { heads: [], machine: noGpuMachine }, "GET /api/v1/status": {} },
      (cmd) => (cmd === "get_data_dir" ? Promise.reject("no host") : undefined)
    );
    mockUseConfig.mockReturnValue({
      config: makeConfig({ data_dir: "/home/test/.lettuce" }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    const user = userEvent.setup();
    render(<SettingsPage />);
    await user.click(screen.getByText("Identity"));

    expect(screen.getByText("/home/test/.lettuce")).toBeInTheDocument();
  });

  it("remembers the chosen theme in localStorage and restores it", async () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    const user = userEvent.setup();
    const first = render(<SettingsPage />);
    await user.click(screen.getByText("General"));
    await user.click(screen.getByText("Dark"));
    expect(localStorage.getItem("lettuce-theme")).toBe("dark");
    first.unmount();

    render(<SettingsPage />);
    await user.click(screen.getByText("General"));
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    // The Dark button is the selected one.
    expect(screen.getByText("Dark").closest("button")?.className).toContain("bg-background");

    // Leave the document clean for other tests.
    await user.click(screen.getByText("Light"));
  });

  it("renders Disk Storage slider", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Disk Storage")).toBeInTheDocument();
  });

  it("renders Network Bandwidth slider", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Network Bandwidth")).toBeInTheDocument();
  });

  it("renders Schedule section with schedule builder", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Schedule")).toBeInTheDocument();
    expect(screen.getByTestId("schedule-builder")).toBeInTheDocument();
  });

  it("passes correct mode to schedule builder", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig({
        scheduling: {
          mode: "WHEN_IDLE",
          idle_threshold_mins: 10,
          cron_expression: "",
        },
      }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByTestId("schedule-builder")).toHaveAttribute(
      "data-mode",
      "WHEN_IDLE"
    );
  });

  it("renders collapsible sections", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    // Default open sections
    expect(screen.getByText("Resource Limits")).toBeInTheDocument();
    expect(screen.getByText("Schedule")).toBeInTheDocument();

    // Collapsed sections (still render headers)
    expect(screen.getByText("Thermal")).toBeInTheDocument();
    expect(screen.getByText("Container Runtime")).toBeInTheDocument();
    expect(screen.getByText("Identity")).toBeInTheDocument();
    expect(screen.getByText("General")).toBeInTheDocument();
  });

  it("renders Container Runtime section with status card", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Container Runtime")).toBeInTheDocument();
    expect(
      screen.getByTestId("container-runtime-status-card")
    ).toBeInTheDocument();
  });

  it("can expand collapsed sections", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    // General section is collapsed by default
    expect(screen.queryByText("Theme")).not.toBeInTheDocument();

    // Click to expand
    await user.click(screen.getByText("General"));
    expect(screen.getByText("Theme")).toBeInTheDocument();
  });

  it("renders theme toggle buttons when General is expanded", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("General"));

    expect(screen.getByText("System")).toBeInTheDocument();
    expect(screen.getByText("Light")).toBeInTheDocument();
    expect(screen.getByText("Dark")).toBeInTheDocument();
  });

  it("switches theme when theme buttons are clicked", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("General"));

    // Click Dark theme
    await user.click(screen.getByText("Dark"));
    expect(document.documentElement.classList.contains("dark")).toBe(true);

    // Click Light theme
    await user.click(screen.getByText("Light"));
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });

  it("renders log level select when General is expanded", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("General"));
    expect(screen.getByText("Log Level")).toBeInTheDocument();
  });

  it("calls updateConfig when log level changes", async () => {
    const user = userEvent.setup();
    const updateConfig = vi.fn();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig,
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("General"));

    // Find the log level select and change it
    const logSelect = screen.getByDisplayValue("Info");
    await user.selectOptions(logSelect, "debug");
    expect(updateConfig).toHaveBeenCalledWith({ log_level: "debug" });
  });

  it("renders toast when toast message is present", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: "Saved",
    });

    render(<SettingsPage />);
    expect(screen.getByText("Saved")).toBeInTheDocument();
  });

  it("renders error toast with destructive styling", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: "Error: Invalid value",
    });

    const { container } = render(<SettingsPage />);
    const toast = screen.getByText("Error: Invalid value");
    expect(toast).toBeInTheDocument();
    // Error toasts have destructive styling
    expect(toast.className).toContain("destructive");
  });

  it("shows GPU disabled text when max_gpu_vram_pct is 0", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig({
        resource_limits: {
          max_cpu_cores: 4,
          max_memory_mb: 2048,
          max_disk_gb: 10,
          max_bandwidth_mbps: 0,
          max_gpu_vram_pct: 0,
          max_pids: 0,
        },
      }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("GPU disabled")).toBeInTheDocument();
  });

  it("shows Unlimited for bandwidth when max_bandwidth_mbps is 0", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig({
        resource_limits: {
          max_cpu_cores: 4,
          max_memory_mb: 2048,
          max_disk_gb: 10,
          max_bandwidth_mbps: 0,
          max_gpu_vram_pct: 50,
          max_pids: 0,
        },
      }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("Unlimited")).toBeInTheDocument();
  });

  it("shows Identity section with public key when expanded", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig({ public_key: "abc123-test-key" }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("Identity"));
    expect(screen.getByText("Public Key")).toBeInTheDocument();
    expect(screen.getByText("abc123-test-key")).toBeInTheDocument();
  });

  it("shows notification toggles when General is expanded", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("General"));
    expect(screen.getByText("Credit milestones")).toBeInTheDocument();
    expect(screen.getByText("Errors requiring attention")).toBeInTheDocument();
    expect(screen.getByText("Work unit completed")).toBeInTheDocument();
    expect(screen.getByText("Update available")).toBeInTheDocument();
  });

  it("shows Regenerate Keypair button", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("Identity"));
    expect(screen.getByText("Regenerate Keypair")).toBeInTheDocument();
  });

  it("shows confirmation dialog when Regenerate Keypair is clicked", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("Identity"));
    await user.click(screen.getByText("Regenerate Keypair"));

    expect(
      screen.getByText(/This will generate a new identity/)
    ).toBeInTheDocument();
    expect(screen.getByText("Yes, Regenerate")).toBeInTheDocument();
    expect(screen.getByText("Cancel")).toBeInTheDocument();
  });

  it("hides confirmation when Cancel is clicked on regenerate", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("Identity"));
    await user.click(screen.getByText("Regenerate Keypair"));

    expect(
      screen.getByText(/This will generate a new identity/)
    ).toBeInTheDocument();

    await user.click(screen.getByText("Cancel"));

    expect(
      screen.queryByText(/This will generate a new identity/)
    ).not.toBeInTheDocument();
    // Regenerate button should reappear
    expect(screen.getByText("Regenerate Keypair")).toBeInTheDocument();
  });

  it("calls updateConfig with correct partial when CPU cores slider changes", async () => {
    const updateConfig = vi.fn();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig,
      toast: null,
    });

    render(<SettingsPage />);

    // Find the CPU cores slider (first range input in the Resource Limits section)
    const sliders = screen.getAllByRole("slider");
    // First slider is CPU cores
    fireEvent.change(sliders[0], { target: { value: "2" } });

    expect(updateConfig).toHaveBeenCalledWith({
      resource_limits: expect.objectContaining({ max_cpu_cores: 2 }),
    });
  });

  it("shows 'No GPU detected' when the machine has no GPU", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("No GPU detected")).toBeInTheDocument();
  });

  it("applies system theme via matchMedia listener", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("General"));

    // System is the default, matchMedia mock returns matches: false
    // so dark class should not be present
    await user.click(screen.getByText("System"));
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });

  it("can collapse a section that is open by default", async () => {
    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    // Resource Limits is open by default and contains "CPU Cores"
    expect(screen.getByText("CPU Cores")).toBeInTheDocument();

    // Click to collapse
    await user.click(screen.getByText("Resource Limits"));
    expect(screen.queryByText("CPU Cores")).not.toBeInTheDocument();

    // Click again to expand
    await user.click(screen.getByText("Resource Limits"));
    expect(screen.getByText("CPU Cores")).toBeInTheDocument();
  });

  it("calls updateConfig with correct notifications partial when credit milestones toggle is clicked", async () => {
    const user = userEvent.setup();
    const updateConfig = vi.fn();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig,
      toast: null,
    });

    render(<SettingsPage />);

    // Expand General section
    await user.click(screen.getByText("General"));

    // Credit milestones toggle is currently true (default). Click to disable.
    const creditToggle = screen.getByText("Credit milestones")
      .closest(".flex")!
      .querySelector("[role='switch']")!;
    await user.click(creditToggle);

    expect(updateConfig).toHaveBeenCalledWith({
      notifications: expect.objectContaining({
        credit_milestones: false,
      }),
    });
  });

  it("calls invoke('regenerate_keypair') when Yes, Regenerate is clicked", async () => {
    const { invoke } = await import("@tauri-apps/api/core");
    const mockInvoke = invoke as ReturnType<typeof vi.fn>;
    mockInvoke.mockImplementation((cmd: string) => {
      if (cmd === "is_autostart_enabled") return Promise.resolve(false);
      if (cmd === "regenerate_keypair") return Promise.resolve("new-public-key");
      return Promise.resolve(undefined);
    });

    const user = userEvent.setup();
    const refetch = vi.fn();
    mockUseConfig.mockReturnValue({
      config: makeConfig({ public_key: "old-key" }),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
      refetch,
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("Identity"));
    await user.click(screen.getByText("Regenerate Keypair"));
    await user.click(screen.getByText("Yes, Regenerate"));

    await waitFor(() => {
      expect(mockInvoke).toHaveBeenCalledWith("regenerate_keypair");
    });

    // Should call refetch to reload config with new key
    await waitFor(() => {
      expect(refetch).toHaveBeenCalled();
    });
  });

  it("shows error message when regenerate fails", async () => {
    const { invoke } = await import("@tauri-apps/api/core");
    const mockInvoke = invoke as ReturnType<typeof vi.fn>;
    mockInvoke.mockImplementation((cmd: string) => {
      if (cmd === "is_autostart_enabled") return Promise.resolve(false);
      if (cmd === "regenerate_keypair") return Promise.reject(new Error("failed"));
      return Promise.resolve(undefined);
    });

    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
      refetch: vi.fn(),
    });

    render(<SettingsPage />);

    await user.click(screen.getByText("Identity"));
    await user.click(screen.getByText("Regenerate Keypair"));
    await user.click(screen.getByText("Yes, Regenerate"));

    // After failure, error message should appear and dialog stays open
    await waitFor(() => {
      expect(screen.getByText("failed")).toBeInTheDocument();
    });

    // Confirm dialog should still be visible
    expect(
      screen.getByText(/This will generate a new identity/)
    ).toBeInTheDocument();
  });

  it("calls invoke for autostart on mount and toggles autostart", async () => {
    const { invoke } = await import("@tauri-apps/api/core");
    const mockInvoke = invoke as ReturnType<typeof vi.fn>;
    mockInvoke.mockImplementation((cmd: string) => {
      if (cmd === "is_autostart_enabled") return Promise.resolve(true);
      return Promise.resolve(undefined);
    });

    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    // Verify is_autostart_enabled was called on mount
    expect(mockInvoke).toHaveBeenCalledWith("is_autostart_enabled");

    // Expand General section
    await user.click(screen.getByText("General"));

    // Find the autostart toggle (the one next to "Start on boot")
    const autostartToggle = screen.getByText("Start on boot")
      .closest(".flex")!
      .querySelector("[role='switch']")!;

    // Click to disable autostart (was true from mock)
    await user.click(autostartToggle);

    expect(mockInvoke).toHaveBeenCalledWith("set_autostart", { enabled: false });
  });

  it("reverts autostart toggle on failure", async () => {
    const { invoke } = await import("@tauri-apps/api/core");
    const mockInvoke = invoke as ReturnType<typeof vi.fn>;
    mockInvoke.mockImplementation((cmd: string) => {
      if (cmd === "is_autostart_enabled") return Promise.resolve(true);
      if (cmd === "set_autostart") return Promise.reject(new Error("fail"));
      return Promise.resolve(undefined);
    });

    const user = userEvent.setup();
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);

    // Expand General section
    await user.click(screen.getByText("General"));

    // Find the autostart toggle
    const autostartToggle = screen.getByText("Start on boot")
      .closest(".flex")!
      .querySelector("[role='switch']")!;

    // Initially checked=true (from is_autostart_enabled mock)
    expect(autostartToggle).toHaveAttribute("aria-checked", "true");

    // Click to disable -- this will fail and should revert
    await user.click(autostartToggle);

    // After the rejection, the toggle should revert back to true
    await waitFor(() => {
      expect(autostartToggle).toHaveAttribute("aria-checked", "true");
    });
  });

  /**
   * TB-47: the core maximum used to come from `navigator.hardwareConcurrency`,
   * which WebKit (the Linux and macOS web view) caps at 8. The number is the
   * daemon's hard CPU quota, so a 256-thread host was clamped to 8 cores the
   * first time its slider was touched. The Rust host's count is the source
   * now; the browser figure is only a fallback and never a clamp.
   */
  describe("core count comes from the host, not the web view (TB-47)", () => {
    /** The range input of the `ResourceSlider` labelled `label`. */
    function sliderFor(label: string): HTMLInputElement {
      const root = screen.getByText(label).parentElement!.parentElement!;
      return root.querySelector("input[type='range']") as HTMLInputElement;
    }

    /** Route `get_system_cpu_count` beside the usual heads/status routes. */
    function mockHostCpuCount(count: number) {
      mockManagementApi(
        { "GET /api/v1/heads": { heads: [], machine: noGpuMachine }, "GET /api/v1/status": {} },
        (cmd) => (cmd === "get_system_cpu_count" ? count : defaultCommandResult(cmd))
      );
    }

    beforeEach(() => {
      // What a WebKit web view reports on any machine with 8 or more threads.
      Object.defineProperty(navigator, "hardwareConcurrency", {
        value: 8,
        configurable: true,
      });
    });

    afterEach(() => {
      delete (navigator as { hardwareConcurrency?: number }).hardwareConcurrency;
    });

    it("sizes the CPU cores and concurrent-tasks sliders from the host's core count", async () => {
      mockHostCpuCount(256);
      mockUseConfig.mockReturnValue({
        config: makeConfig({ max_concurrent_tasks: 1 }),
        isLoading: false,
        updateConfig: vi.fn(),
        toast: null,
      });

      render(<SettingsPage />);

      await waitFor(() => {
        expect(screen.getByText("4 / 256 cores")).toBeInTheDocument();
      });
      expect(sliderFor("CPU Cores")).toHaveAttribute("max", "256");
      expect(sliderFor("Concurrent Tasks")).toHaveAttribute("max", "256");
    });

    it("never clamps a saved core count above the detected total", async () => {
      // The CLI's default is NumCPU / 2 (128 on the reporter's box); the app
      // must show that value, not truncate it to the slider's maximum.
      mockHostCpuCount(8);
      mockUseConfig.mockReturnValue({
        config: makeConfig({
          resource_limits: { ...makeConfig().resource_limits, max_cpu_cores: 128 },
        }),
        isLoading: false,
        updateConfig: vi.fn(),
        toast: null,
      });

      render(<SettingsPage />);

      await waitFor(() => {
        expect(screen.getByText("128 / 8 cores")).toBeInTheDocument();
      });
      expect(sliderFor("CPU Cores")).toHaveAttribute("max", "128");
      expect(sliderFor("CPU Cores")).toHaveValue("128");
    });

    it("falls back to the web view's figure only when the host cannot report a count", async () => {
      mockHostCpuCount(0);
      mockUseConfig.mockReturnValue({
        config: makeConfig(),
        isLoading: false,
        updateConfig: vi.fn(),
        toast: null,
      });

      render(<SettingsPage />);

      // Let the (empty) host answer land before asserting on the fallback.
      await waitFor(() => {
        expect(
          vi.mocked(invoke).mock.calls.some(([cmd]) => cmd === "get_system_cpu_count")
        ).toBe(true);
      });
      expect(screen.getByText("4 / 8 cores")).toBeInTheDocument();
      expect(sliderFor("CPU Cores")).toHaveAttribute("max", "8");
    });
  });

  // --- TB-66: the Memory slider's maximum never clamps a saved allowance ---

  it("the Memory slider steps by 256 MB up to 90 % of RAM, and never clamps an allowance saved above that", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });
    const { unmount } = render(<SettingsPage />);
    let memory = (screen.getAllByRole("slider") as HTMLInputElement[]).find(
      (el) => el.step === "256"
    );
    expect(memory).toBeDefined();
    expect(memory!.min).toBe("256");
    expect(memory!.max).toBe("14746");
    unmount();

    const raised = makeConfig();
    raised.resource_limits = { ...raised.resource_limits, max_memory_mb: 16000 };
    mockUseConfig.mockReturnValue({
      config: raised,
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });
    render(<SettingsPage />);
    memory = (screen.getAllByRole("slider") as HTMLInputElement[]).find((el) => el.step === "256");
    expect(memory!.max).toBe("16000");
    expect(memory!.value).toBe("16000");
  });
});
