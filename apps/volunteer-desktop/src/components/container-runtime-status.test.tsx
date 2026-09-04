import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ContainerRuntimeStatusCard } from "./container-runtime-status";
import type { ContainerRuntimeStatus } from "@/api/client";

// Mock the hook
const mockRefresh = vi.fn();
const mockUseContainerRuntime = vi.fn();

vi.mock("@/hooks/use-container-runtime", () => ({
  useContainerRuntime: () => mockUseContainerRuntime(),
}));

// Mock the api/client actions
const mockSetupContainerRuntime = vi.fn();
const mockStartContainerRuntime = vi.fn();
const mockStopContainerRuntime = vi.fn();

const mockInstallPodman = vi.fn();
const mockRedetectContainerRuntime = vi.fn();

vi.mock("@/api/client", () => ({
  setupContainerRuntime: (...args: unknown[]) =>
    mockSetupContainerRuntime(...args),
  startContainerRuntime: (...args: unknown[]) =>
    mockStartContainerRuntime(...args),
  stopContainerRuntime: (...args: unknown[]) =>
    mockStopContainerRuntime(...args),
  installPodman: (...args: unknown[]) =>
    mockInstallPodman(...args),
  redetectContainerRuntime: (...args: unknown[]) =>
    mockRedetectContainerRuntime(...args),
}));

function makeStatus(
  overrides: Partial<ContainerRuntimeStatus> = {}
): ContainerRuntimeStatus {
  return {
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
    ...overrides,
  };
}

describe("ContainerRuntimeStatusCard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockRefresh.mockResolvedValue(undefined);
  });

  it("renders loading skeleton when loading and no status", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: null,
      loading: true,
      error: null,
      refresh: mockRefresh,
    });

    const { container } = render(<ContainerRuntimeStatusCard />);
    expect(container.querySelector(".animate-pulse")).toBeInTheDocument();
  });

  it("renders error state with retry button when error and no status", async () => {
    const user = userEvent.setup();
    mockUseContainerRuntime.mockReturnValue({
      status: null,
      loading: false,
      error: "Connection failed",
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Container Runtime")).toBeInTheDocument();
    expect(screen.getByText("Connection failed")).toBeInTheDocument();

    await user.click(screen.getByText("Retry"));
    expect(mockRefresh).toHaveBeenCalledOnce();
  });

  it("returns null when no status and no error and not loading", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: null,
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    const { container } = render(<ContainerRuntimeStatusCard />);
    expect(container.firstChild).toBeNull();
  });

  // State: Running — Podman
  it("renders running Podman state with version", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "podman",
        status: "running",
        version: "5.3.1",
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Podman 5.3.1")).toBeInTheDocument();
  });

  it("renders machine info when machine_required is true", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "podman",
        status: "running",
        version: "5.3.1",
        machine_required: true,
        machine_name: "lettuce-vm",
        machine_cpus: 4,
        machine_memory_mb: 4096,
        machine_disk_gb: 50,
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Machine: lettuce-vm")).toBeInTheDocument();
    expect(
      screen.getByText("Resources: 4 CPUs, 4 GB RAM, 50 GB disk")
    ).toBeInTheDocument();
  });

  it("shows default machine name when machine_name is empty", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "podman",
        status: "running",
        machine_required: true,
        machine_name: "",
        machine_cpus: 2,
        machine_memory_mb: 2048,
        machine_disk_gb: 20,
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);
    expect(screen.getByText("Machine: default")).toBeInTheDocument();
  });

  it("shows Stop Machine button for Podman with machine_required", async () => {
    const user = userEvent.setup();
    mockStopContainerRuntime.mockResolvedValue({
      status: "ok",
      message: "stopped",
    });

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "podman",
        status: "running",
        machine_required: true,
        machine_cpus: 2,
        machine_memory_mb: 2048,
        machine_disk_gb: 20,
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    const stopBtn = screen.getByText("Stop Machine");
    await user.click(stopBtn);

    expect(mockStopContainerRuntime).toHaveBeenCalledOnce();
    await waitFor(() => {
      expect(mockRefresh).toHaveBeenCalled();
    });
  });

  // State: Running — Docker
  it("renders running Docker state", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "docker",
        status: "running",
        version: "24.0.7",
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Docker 24.0.7")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Using system Docker. Install Podman for a lighter alternative."
      )
    ).toBeInTheDocument();
  });

  // State: Stopped
  it("renders stopped state with Start Machine button", async () => {
    const user = userEvent.setup();
    mockStartContainerRuntime.mockResolvedValue({
      status: "ok",
      message: "started",
    });

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "stopped" }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Machine stopped")).toBeInTheDocument();
    expect(
      screen.getByText("Container leafs unavailable until machine is started.")
    ).toBeInTheDocument();

    await user.click(screen.getByText("Start Machine"));
    expect(mockStartContainerRuntime).toHaveBeenCalledOnce();
    await waitFor(() => {
      expect(mockRefresh).toHaveBeenCalled();
    });
  });

  // State: Starting
  it("renders starting state", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "starting" }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Starting...")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Container runtime is starting up. This may take a moment."
      )
    ).toBeInTheDocument();
  });

  // State: Not Initialized
  it("renders not_initialized state with Setup button", async () => {
    const user = userEvent.setup();
    mockSetupContainerRuntime.mockResolvedValue({
      status: "ok",
      message: "initialized",
    });

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "not_initialized" }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Setup Required")).toBeInTheDocument();
    expect(
      screen.getByText("Podman is installed but needs initial setup.")
    ).toBeInTheDocument();

    await user.click(screen.getByText("Setup Container Runtime"));
    expect(mockSetupContainerRuntime).toHaveBeenCalledOnce();
    await waitFor(() => {
      expect(mockRefresh).toHaveBeenCalled();
    });
  });

  // TB-59: while the daemon keeps probing for an engine, the card says so
  // and offers an immediate re-check; a daemon that is not probing (no head
  // trusted for containers, or an older build) gets neither.
  it("offers an immediate re-check while the daemon is probing for an engine", async () => {
    const user = userEvent.setup();
    mockRedetectContainerRuntime.mockResolvedValue({ status: "checking", message: "" });
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "not_installed", backend: "none", redetecting: true }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText(/checks for an engine every minute/)).toBeInTheDocument();
    await user.click(screen.getByText("Check again now"));
    expect(mockRedetectContainerRuntime).toHaveBeenCalledOnce();
    await waitFor(() => expect(mockRefresh).toHaveBeenCalled());
  });

  it("does not offer a re-check when the daemon is not probing", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "not_installed", backend: "none", redetecting: false }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.queryByText("Check again now")).not.toBeInTheDocument();
    expect(screen.queryByText(/checks for an engine every minute/)).not.toBeInTheDocument();
  });

  // State: Not Installed
  it("renders not_installed state on linux", () => {
    // Override navigator.userAgent for the component's platform detection
    // The component reads navigator.userAgent at module load time, so we
    // test the fallback text that is always present regardless of platform
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "not_installed", backend: "none" }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(
      screen.getByText("No container runtime installed")
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Container leafs will be unavailable until a runtime is installed/)
    ).toBeInTheDocument();
    // jsdom reports no Windows/Mac user agent, so the Linux guidance renders:
    // it must point at the real install path, never a bundled binary.
    expect(screen.getByText(/systemctl --user enable --now podman.socket/)).toBeInTheDocument();
    expect(screen.queryByText(/bundled Podman binary/)).not.toBeInTheDocument();
    expect(screen.queryByText("Install Podman")).not.toBeInTheDocument();
  });

  // State: Error
  it("renders error state with retry button", async () => {
    const user = userEvent.setup();
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        status: "error",
        error: "Socket connection refused",
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Container Runtime Error")).toBeInTheDocument();
    expect(
      screen.getByText("Socket connection refused")
    ).toBeInTheDocument();

    await user.click(screen.getByText("Retry"));
    expect(mockRefresh).toHaveBeenCalledOnce();
  });

  // Action error display
  it("displays action error when stop fails", async () => {
    const user = userEvent.setup();
    mockStopContainerRuntime.mockRejectedValue(new Error("Stop failed"));

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "podman",
        status: "running",
        machine_required: true,
        machine_cpus: 2,
        machine_memory_mb: 2048,
        machine_disk_gb: 20,
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    await user.click(screen.getByText("Stop Machine"));

    await waitFor(() => {
      expect(screen.getByText("Stop failed")).toBeInTheDocument();
    });
  });

  it("displays action error when start fails", async () => {
    const user = userEvent.setup();
    mockStartContainerRuntime.mockRejectedValue(
      new Error("Already starting")
    );

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "stopped" }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    await user.click(screen.getByText("Start Machine"));

    await waitFor(() => {
      expect(screen.getByText("Already starting")).toBeInTheDocument();
    });
  });

  it("shows Stopping... text while action is in progress", async () => {
    const user = userEvent.setup();
    let resolveStop: (value: unknown) => void;
    mockStopContainerRuntime.mockReturnValue(
      new Promise((resolve) => {
        resolveStop = resolve;
      })
    );

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "podman",
        status: "running",
        machine_required: true,
        machine_cpus: 2,
        machine_memory_mb: 2048,
        machine_disk_gb: 20,
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    await user.click(screen.getByText("Stop Machine"));

    expect(screen.getByText("Stopping...")).toBeInTheDocument();

    // Resolve to clean up
    await act(async () => {
      resolveStop!({ status: "ok", message: "stopped" });
    });
  });

  it("shows Starting... text while start action is in progress", async () => {
    const user = userEvent.setup();
    let resolveStart: (value: unknown) => void;
    mockStartContainerRuntime.mockReturnValue(
      new Promise((resolve) => {
        resolveStart = resolve;
      })
    );

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "stopped" }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    await user.click(screen.getByText("Start Machine"));

    expect(screen.getByText("Starting...")).toBeInTheDocument();

    // Resolve to clean up
    await act(async () => {
      resolveStart!({ status: "ok", message: "started" });
    });
  });

  it("shows Setting up... text while setup action is in progress", async () => {
    const user = userEvent.setup();
    let resolveSetup: (value: unknown) => void;
    mockSetupContainerRuntime.mockReturnValue(
      new Promise((resolve) => {
        resolveSetup = resolve;
      })
    );

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "not_initialized" }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    await user.click(screen.getByText("Setup Container Runtime"));

    expect(screen.getByText("Setting up...")).toBeInTheDocument();

    // Resolve to clean up
    await act(async () => {
      resolveSetup!({ status: "ok", message: "initialized" });
    });
  });

  // --- Coverage gap: action error for non-Error rejections ---

  it("displays action error for non-Error rejection (raw string)", async () => {
    const user = userEvent.setup();
    mockStopContainerRuntime.mockRejectedValue("raw stop error");

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "podman",
        status: "running",
        machine_required: true,
        machine_cpus: 2,
        machine_memory_mb: 2048,
        machine_disk_gb: 20,
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    await user.click(screen.getByText("Stop Machine"));

    await waitFor(() => {
      expect(screen.getByText("raw stop error")).toBeInTheDocument();
    });
  });

  // --- Coverage gap: running Podman without machine_required hides Stop button ---

  it("does NOT show Stop Machine button for Podman without machine_required", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({
        backend: "podman",
        status: "running",
        version: "5.3.1",
        machine_required: false,
      }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    expect(screen.getByText("Podman 5.3.1")).toBeInTheDocument();
    expect(screen.queryByText("Stop Machine")).not.toBeInTheDocument();
  });

  // --- Coverage gap: setup action error display ---

  it("displays action error when setup fails", async () => {
    const user = userEvent.setup();
    mockSetupContainerRuntime.mockRejectedValue(new Error("Init failed"));

    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "not_initialized" }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    render(<ContainerRuntimeStatusCard />);

    await user.click(screen.getByText("Setup Container Runtime"));

    await waitFor(() => {
      expect(screen.getByText("Init failed")).toBeInTheDocument();
    });
  });

  // --- Coverage gap: unknown status falls through to null ---

  it("returns null for unknown status values", () => {
    mockUseContainerRuntime.mockReturnValue({
      status: makeStatus({ status: "unknown_state" as any }),
      loading: false,
      error: null,
      refresh: mockRefresh,
    });

    const { container } = render(<ContainerRuntimeStatusCard />);
    expect(container.firstChild).toBeNull();
  });
});

// Need to import act for the pending-action tests
import { act } from "@testing-library/react";
