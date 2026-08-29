import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SettingsPage } from "./settings";
import type { ConfigResponse, MetricsResponse } from "@/api/client";

// Mock hooks
vi.mock("@/hooks/use-config", () => ({
  useConfig: vi.fn(),
}));

vi.mock("@/hooks/use-metrics", () => ({
  useMetrics: vi.fn(),
}));

// Mock lucide-react icons
vi.mock("lucide-react", () => ({
  ChevronDown: (props: any) => <span data-testid="chevron-down" {...props} />,
  ChevronRight: (props: any) => <span data-testid="chevron-right" {...props} />,
  Copy: (props: any) => <span data-testid="copy-icon" {...props} />,
  Check: (props: any) => <span data-testid="check-icon" {...props} />,
  AlertTriangle: (props: any) => <span data-testid="alert-icon" {...props} />,
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
    available_runtimes: ["NATIVE"],
    ...overrides,
  };
}

describe("SettingsPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseMetrics.mockReturnValue({
      metrics: null,
      isLoading: false,
      error: null,
    });
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

  it("renders GPU VRAM slider", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    render(<SettingsPage />);
    expect(screen.getByText("GPU VRAM")).toBeInTheDocument();
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

  it("shows 'No GPU detected' when no GPU is available", () => {
    mockUseConfig.mockReturnValue({
      config: makeConfig(),
      isLoading: false,
      updateConfig: vi.fn(),
      toast: null,
    });

    mockUseMetrics.mockReturnValue({
      metrics: {
        cpu_usage_pct: 45,
        gpu_usage_pct: -1,
        memory_used_mb: 4096,
        memory_total_mb: 8192,
        disk_used_gb: 50,
        disk_total_gb: 200,
        cpu_temp_c: 60,
        gpu_temp_c: 0,
      },
      isLoading: false,
      error: null,
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
});
