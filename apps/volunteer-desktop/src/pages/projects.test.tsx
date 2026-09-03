import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { invoke } from "@tauri-apps/api/core";
import { ProjectsPage } from "./projects";
import { RestartRequiredBanner } from "@/components/restart-required-banner";
import { resetRestartRequiredForTest, useRestartRequired } from "@/hooks/use-restart-required";
import { renderHook } from "@testing-library/react";
import type { HeadInfo, LeafInfo, MachineCapabilities } from "@/api/client";

const mockRefetch = vi.fn();
const mockSetHeads = vi.fn();
const mockSetTrustByHead = vi.fn();
const mockWriteLeafPrefs = vi.fn().mockResolvedValue(undefined);
const mockWriteHeadTrust = vi.fn();
const mockRaiseDisk = vi.fn();
const mockWriteHeadWeight = vi.fn();
const mockWriteLeafWeight = vi.fn();
const mockClient = {
  detachHead: vi.fn(),
  attachHead: vi.fn(),
  config: vi.fn(),
  updateConfig: vi.fn(),
};

const mockUseHeads = vi.fn();
const mockUseClient = vi.fn();
const mockInvoke = vi.mocked(invoke);

vi.mock("@/hooks/use-heads", () => ({
  useHeads: () => mockUseHeads(),
  useWriteLeafPreferences: () => ({
    write: mockWriteLeafPrefs,
  }),
  useWriteHeadTrust: () => ({
    write: mockWriteHeadTrust,
  }),
  useRaiseDiskAllowance: () => ({
    raise: mockRaiseDisk,
  }),
  useDebouncedHeadWeight: () => ({
    write: mockWriteHeadWeight,
  }),
  useDebouncedLeafWeight: () => ({
    write: mockWriteLeafWeight,
  }),
}));

vi.mock("@/hooks/use-api", () => ({
  useClient: () => mockUseClient(),
}));

const mockUseContainerRuntime = vi.fn();

vi.mock("@/hooks/use-container-runtime", () => ({
  useContainerRuntime: () => mockUseContainerRuntime(),
}));

function makeLeaf(overrides: Partial<LeafInfo> = {}): LeafInfo {
  return {
    id: "leaf-1",
    slug: "prime-gap-study",
    name: "Prime Gap Study",
    description: "Finding prime gaps",
    research_area: ["mathematics"],
    task_pattern: "PARAMETER_SWEEP",
    state: "ACTIVE",
    queued_work_units: 450,
    active_volunteers: 3,
    active_hosts: 3,
    enabled: true,
    effective_weight: 50,
    ...overrides,
  };
}

const mockMachine: MachineCapabilities = {
  runtimes: ["container", "wasm"],
  has_gpu: false,
  max_memory_mb: 4096,
  max_disk_mb: 10240,
  max_cpu_cores: 2,
  max_gpu_vram_mb: 0,
  gpu_card_vram_mb: 0,
  gpu_vram_pct: 0,
  gpu_vendors: [],
  gpu_compute_capabilities: [],
};

const mockHeads: HeadInfo[] = [
  {
    name: "lettuce.science",
    description: "Open science compute",
    url: "https://lettuce.science",
    grpc_address: "lettuce.science:443",
    status: "connected",
    weight: 70,
    leafs: [
      makeLeaf(),
      makeLeaf({
        id: "leaf-2",
        slug: "mandelbrot",
        name: "Mandelbrot Analysis",
        description: "Fractal analysis",
        queued_work_units: 200,
        active_volunteers: 2,
        effective_weight: 30,
      }),
      makeLeaf({
        id: "leaf-3",
        slug: "monte-carlo-pi",
        name: "Monte Carlo Pi",
        description: "Pi estimation",
        queued_work_units: 100,
        active_volunteers: 1,
        enabled: false,
        effective_weight: 20,
      }),
    ],
  },
  {
    name: "einstein@home",
    description: "Gravitational wave search",
    url: "https://einstein.phys.uwm.edu",
    grpc_address: "einstein.phys.uwm.edu:443",
    status: "connected",
    weight: 30,
    leafs: [
      makeLeaf({
        id: "leaf-4",
        slug: "gw-search",
        name: "Gravitational Wave Search",
        description: "Searching for gravitational waves",
        research_area: ["physics"],
        queued_work_units: 1000,
        active_volunteers: 50,
        effective_weight: 100,
      }),
    ],
  },
];

function headsState(overrides: Partial<ReturnType<typeof mockUseHeads>> = {}) {
  return {
    heads: mockHeads,
    machine: mockMachine,
    trustByHead: { "lettuce.science:443": ["CONTAINER"], "einstein.phys.uwm.edu:443": [] },
    isLoading: false,
    error: null,
    refetch: mockRefetch,
    setHeads: mockSetHeads,
    setTrustByHead: mockSetTrustByHead,
    ...overrides,
  };
}

describe("ProjectsPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetRestartRequiredForTest();
    mockInvoke.mockReset();
    mockInvoke.mockResolvedValue(undefined);
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockClient.detachHead.mockResolvedValue(undefined);
    mockClient.attachHead.mockResolvedValue(undefined);
    mockWriteHeadTrust.mockResolvedValue({ status: "ok", restart_required: true });
    mockRaiseDisk.mockResolvedValue({ status: "ok", restart_required: false });
    mockUseHeads.mockReturnValue(headsState());
    mockUseContainerRuntime.mockReturnValue({
      status: null,
      loading: false,
      error: null,
      refresh: vi.fn(),
    });
  });

  it("renders head sections for each connected server", () => {
    render(<ProjectsPage />);
    expect(screen.getByText("lettuce.science")).toBeInTheDocument();
    expect(screen.getByText("einstein@home")).toBeInTheDocument();
  });

  it("renders leaf cards within each head", () => {
    render(<ProjectsPage />);
    expect(screen.getByText("Prime Gap Study")).toBeInTheDocument();
    expect(screen.getByText("Mandelbrot Analysis")).toBeInTheDocument();
    expect(screen.getByText("Monte Carlo Pi")).toBeInTheDocument();
    expect(screen.getByText("Gravitational Wave Search")).toBeInTheDocument();
  });

  it("shows disabled state for disabled leafs", () => {
    render(<ProjectsPage />);
    expect(screen.getByText("(disabled)")).toBeInTheDocument();
  });

  it("toggle leaf calls update function", async () => {
    const user = userEvent.setup();

    render(<ProjectsPage />);

    // Monte Carlo Pi is the unchecked leaf checkbox (disabled leaf)
    const checkboxes = screen.getAllByRole("checkbox");
    const unchecked = checkboxes.find(
      (cb) => !(cb as HTMLInputElement).checked
    );
    expect(unchecked).toBeDefined();

    await user.click(unchecked!);

    expect(mockWriteLeafPrefs).toHaveBeenCalled();
  });

  it("head weight slider is visible when 2+ heads exist", () => {
    render(<ProjectsPage />);
    // Both heads should show "Head weight" label
    expect(screen.getAllByText("Head weight")).toHaveLength(2);
  });

  it("leaf weight slider visibility", () => {
    render(<ProjectsPage />);

    // lettuce.science has 2 enabled leafs -> weight sliders visible;
    // einstein@home has 1 enabled leaf -> none.
    const weightLabels = screen.getAllByText("Weight");
    expect(weightLabels.length).toBe(2);
  });

  it("Use Defaults button resets leaf preferences", async () => {
    const user = userEvent.setup();

    render(<ProjectsPage />);

    const defaultBtns = screen.getAllByText("Use Defaults");
    await user.click(defaultBtns[0]);

    expect(mockWriteLeafPrefs).toHaveBeenCalledWith(
      expect.objectContaining({ name: "lettuce.science", grpc_address: "lettuce.science:443" }),
      { mode: "ALL" }
    );
  });

  it("Add Server button opens dialog", async () => {
    const user = userEvent.setup();

    render(<ProjectsPage />);

    await user.click(screen.getByText("+ Add Server"));
    expect(
      screen.getByPlaceholderText("compute.example.org")
    ).toBeInTheDocument();
  });

  it("empty state when no heads", () => {
    mockUseHeads.mockReturnValue(headsState({ heads: [], trustByHead: {} }));

    render(<ProjectsPage />);
    expect(
      screen.getByText("No servers connected. Add a server to start contributing.")
    ).toBeInTheDocument();
  });

  it("loading state shows skeletons", () => {
    mockUseHeads.mockReturnValue(
      headsState({ heads: [], machine: null, trustByHead: {}, isLoading: true })
    );

    const { container } = render(<ProjectsPage />);
    const skeletons = container.querySelectorAll("[class*='animate-pulse']");
    expect(skeletons.length).toBeGreaterThan(0);
  });

  it("renders error message when useHeads returns error", () => {
    mockUseHeads.mockReturnValue(
      headsState({ heads: [], trustByHead: {}, error: new Error("Network timeout") })
    );

    render(<ProjectsPage />);
    expect(
      screen.getByText("Failed to load servers: Network timeout")
    ).toBeInTheDocument();
  });

  it("detach calls client.detachHead and refetches heads", async () => {
    const user = userEvent.setup();

    render(<ProjectsPage />);

    // Click Detach on the first head
    const detachButtons = screen.getAllByText("Detach");
    await user.click(detachButtons[0]);

    // Confirm
    await user.click(screen.getAllByText("Confirm")[0]);

    expect(mockClient.detachHead).toHaveBeenCalledWith({
      server_address: "lettuce.science:443",
    });

    await waitFor(() => {
      expect(mockRefetch).toHaveBeenCalled();
    });
  });

  it("shows error toast when detach fails", async () => {
    const user = userEvent.setup();

    mockClient.detachHead.mockRejectedValue(
      new (await import("@/api/client")).ApiError("DETACH_FAILED", "Server busy")
    );

    render(<ProjectsPage />);

    const detachButtons = screen.getAllByText("Detach");
    await user.click(detachButtons[0]);
    await user.click(screen.getAllByText("Confirm")[0]);

    await waitFor(() => {
      expect(screen.getByText("Server busy")).toBeInTheDocument();
    });
  });

  it("head weight slider hidden when only 1 head", () => {
    mockUseHeads.mockReturnValue(headsState({ heads: [mockHeads[0]] }));

    render(<ProjectsPage />);
    expect(screen.queryByText("Head weight")).not.toBeInTheDocument();
  });

  it("leaf toggle sets mode ALL when all leafs enabled", async () => {
    const user = userEvent.setup();

    render(<ProjectsPage />);

    // Monte Carlo Pi is the disabled leaf; enable it to make all leafs enabled
    const checkboxes = screen.getAllByRole("checkbox");
    const unchecked = checkboxes.find(
      (cb) => !(cb as HTMLInputElement).checked
    );
    await user.click(unchecked!);

    // When all 3 leafs become enabled, mode should be ALL
    expect(mockWriteLeafPrefs).toHaveBeenCalledWith(
      expect.objectContaining({ grpc_address: "lettuce.science:443" }),
      { mode: "ALL" }
    );
  });

  it("leaf toggle sets mode SPECIFIC when disabling a leaf", async () => {
    const user = userEvent.setup();

    render(<ProjectsPage />);

    // Disable a currently enabled leaf (first checked checkbox under lettuce.science)
    const checkboxes = screen.getAllByRole("checkbox");
    const firstChecked = checkboxes.find(
      (cb) => (cb as HTMLInputElement).checked
    );
    await user.click(firstChecked!);

    expect(mockWriteLeafPrefs).toHaveBeenCalledWith(
      expect.objectContaining({ grpc_address: "lettuce.science:443" }),
      expect.objectContaining({ mode: "SPECIFIC" })
    );
  });

  // --- Container warning toast tests (S89) ---

  function containerHeads(): HeadInfo[] {
    return [
      {
        name: "lettuce.science",
        description: "Open science compute",
        url: "https://lettuce.science",
        grpc_address: "lettuce.science:443",
        status: "connected",
        weight: 100,
        leafs: [
          makeLeaf({
            id: "leaf-c1",
            slug: "nbody-sim",
            name: "N-Body Simulation",
            description: "Gravitational N-body simulation",
            research_area: ["physics"],
            queued_work_units: 200,
            active_volunteers: 5,
            enabled: false,
            execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
          }),
        ],
      },
    ];
  }

  it("shows warning toast when enabling a container leaf without running container runtime", async () => {
    const user = userEvent.setup();

    mockUseHeads.mockReturnValue(headsState({ heads: containerHeads() }));

    // Container runtime is stopped
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

    render(<ProjectsPage />);

    // The leaf is disabled (unchecked), click to enable
    const checkbox = screen.getByRole("checkbox");
    await user.click(checkbox);

    // Should see the warning toast
    await waitFor(() => {
      expect(
        screen.getByText(
          "This leaf requires a container runtime. Go to Settings to set up Podman."
        )
      ).toBeInTheDocument();
    });
  });

  it("does NOT show warning toast when enabling a container leaf with running container runtime", async () => {
    const user = userEvent.setup();

    mockUseHeads.mockReturnValue(headsState({ heads: containerHeads() }));

    // Container runtime is running
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

    render(<ProjectsPage />);

    // The leaf is disabled (unchecked), click to enable
    const checkbox = screen.getByRole("checkbox");
    await user.click(checkbox);

    // Should NOT see the warning toast
    expect(
      screen.queryByText(
        "This leaf requires a container runtime. Go to Settings to set up Podman."
      )
    ).not.toBeInTheDocument();
  });

  it("does NOT show warning toast when enabling a non-container leaf", async () => {
    const user = userEvent.setup();

    // Container runtime is stopped
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

    render(<ProjectsPage />);

    // Monte Carlo Pi (non-container leaf) is disabled — enable it
    const checkboxes = screen.getAllByRole("checkbox");
    const unchecked = checkboxes.find(
      (cb) => !(cb as HTMLInputElement).checked
    );
    await user.click(unchecked!);

    // Should NOT show container warning for non-container leaf
    expect(
      screen.queryByText(
        "This leaf requires a container runtime. Go to Settings to set up Podman."
      )
    ).not.toBeInTheDocument();
  });

  // --- Runtime trust ---

  it("shows each head's trust summary from trustByHead", () => {
    render(<ProjectsPage />);

    const containerChips = screen.getAllByText("Container");
    expect(containerChips[0]).toHaveTextContent("Container ✓");
    expect(containerChips[1]).toHaveTextContent("Container ✗");
  });

  it("saves trust for the right head and mirrors it locally", async () => {
    const user = userEvent.setup();
    mockUseHeads.mockReturnValue(headsState({ heads: [mockHeads[0]] }));

    render(<ProjectsPage />);

    await user.click(screen.getByText("Change..."));
    await user.click(screen.getByLabelText("Allow native tasks from this head"));
    await user.click(screen.getByText("Save trust settings"));

    await waitFor(() => {
      expect(mockWriteHeadTrust).toHaveBeenCalledWith(
        expect.objectContaining({ name: "lettuce.science", grpc_address: "lettuce.science:443" }),
        ["CONTAINER", "NATIVE"]
      );
    });
    expect(mockSetTrustByHead).toHaveBeenCalled();
    const updater = mockSetTrustByHead.mock.calls[0][0];
    expect(updater({ other: [] })).toEqual({ other: [], "lettuce.science:443": ["CONTAINER", "NATIVE"] });
  });

  it("identifies a head by gRPC address in every write when its display title differs from the config alias", async () => {
    const user = userEvent.setup();
    const titled: HeadInfo = {
      ...mockHeads[0],
      name: "LBRY.Science - Lettuce Rip",
      grpc_address: "lbry.science:50051",
      leafs: [makeLeaf(), makeLeaf({ id: "leaf-2", slug: "mandelbrot", name: "Mandelbrot Analysis" })],
    };
    mockUseHeads.mockReturnValue(
      headsState({ heads: [titled], trustByHead: { "lbry.science:50051": ["CONTAINER"] } })
    );
    const ref = expect.objectContaining({
      name: "LBRY.Science - Lettuce Rip",
      grpc_address: "lbry.science:50051",
    });

    render(<ProjectsPage />);

    // Trust is looked up by address, so the chips reflect the config entry.
    expect(screen.getByText("Container")).toHaveTextContent("Container ✓");

    await user.click(screen.getByText("Change..."));
    await user.click(screen.getByText("Save trust settings"));
    await waitFor(() => expect(mockWriteHeadTrust).toHaveBeenCalledWith(ref, ["CONTAINER"]));

    await user.click(screen.getAllByRole("checkbox")[0]);
    expect(mockWriteLeafPrefs).toHaveBeenLastCalledWith(ref, { mode: "SPECIFIC", enabled: ["mandelbrot"] });

    await user.click(screen.getByText("Use Defaults"));
    expect(mockWriteLeafPrefs).toHaveBeenLastCalledWith(ref, { mode: "ALL" });

    await user.click(screen.getByText("Detach"));
    await user.click(screen.getByText("Confirm"));
    expect(mockClient.detachHead).toHaveBeenCalledWith({ server_address: "lbry.science:50051" });
  });

  it("shows the daemon's error in the trust editor when the save fails", async () => {
    const user = userEvent.setup();
    mockUseHeads.mockReturnValue(headsState({ heads: [mockHeads[0]] }));
    mockWriteHeadTrust.mockRejectedValue(
      new (await import("@/api/client")).ApiError("VALIDATION_ERROR", "unknown runtime")
    );

    render(<ProjectsPage />);

    await user.click(screen.getByText("Change..."));
    await user.click(screen.getByText("Save trust settings"));

    await waitFor(() => {
      expect(screen.getByText("unknown runtime")).toBeInTheDocument();
    });
    expect(mockSetTrustByHead).not.toHaveBeenCalled();
  });

  // --- Disk gate ---

  function gatedHeads(): HeadInfo[] {
    return [
      {
        ...mockHeads[0],
        leafs: [
          makeLeaf({
            disk_gate: { blocked: true, reason: "needs 15 GB; 4 GB left of your 10 GB", raise_to_gb: 21 },
          }),
        ],
      },
    ];
  }

  it("raises the disk allowance from a gated leaf and refreshes the verdict", async () => {
    const user = userEvent.setup();
    mockUseHeads.mockReturnValue(headsState({ heads: gatedHeads() }));

    render(<ProjectsPage />);

    expect(screen.getByText("Will not fetch: needs 15 GB; 4 GB left of your 10 GB")).toBeInTheDocument();
    await user.click(screen.getByText("Raise disk allowance to 21 GB"));

    await waitFor(() => {
      expect(mockRaiseDisk).toHaveBeenCalledWith(21);
    });
    await waitFor(() => {
      expect(mockRefetch).toHaveBeenCalled();
    });
  });

  // --- Add server ---

  /** Attach a head through the dialog; the page records the pending restart. */
  async function attachNewHead(user: ReturnType<typeof userEvent.setup>) {
    mockInvoke.mockImplementation(async (cmd: string) => {
      if (cmd === "test_server_connection") return { status: "healthy" };
      if (cmd === "fetch_head_info") return { name: "New Head", description: "", leafs: [] };
      return undefined;
    });
    await user.click(screen.getByText("+ Add Server"));
    await user.type(screen.getByPlaceholderText("compute.example.org"), "https://new.example.org");
    await user.click(screen.getByText("Test Connection"));
    await waitFor(() => expect(screen.getByText("Attach")).toBeInTheDocument());
    await user.click(screen.getByText("Attach"));
    await waitFor(() => expect(mockClient.attachHead).toHaveBeenCalled());
  }

  it("attaching a head refreshes the list and records a pending restart", async () => {
    const user = userEvent.setup();
    const { result: restart } = renderHook(() => useRestartRequired());

    render(<ProjectsPage />);
    await attachNewHead(user);

    expect(mockClient.attachHead).toHaveBeenCalledWith({
      server_address: "new.example.org",
      name: undefined,
      trusted_runtimes: ["CONTAINER"],
    });
    expect(mockRefetch).toHaveBeenCalled();
    expect(restart.current.reasons).toEqual([
      "New Head is attached. Lettuce starts fetching work from it the next time it starts.",
    ]);
  });

  // --- Restart banner (mounted once in the tab layout, above every page) ---

  it("the app-wide banner shows the attach reason and restarting from it hides it", async () => {
    const user = userEvent.setup();

    render(
      <>
        <RestartRequiredBanner />
        <ProjectsPage />
      </>
    );
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    await attachNewHead(user);

    expect(screen.getByRole("status")).toHaveTextContent(
      "New Head is attached. Lettuce starts fetching work from it the next time it starts."
    );

    await user.click(screen.getByText("Restart Lettuce now"));
    await waitFor(() => {
      expect(mockInvoke).toHaveBeenCalledWith("restart_daemon");
    });
    await waitFor(() => {
      expect(screen.queryByRole("status")).not.toBeInTheDocument();
    });
  });

  it("the banner can be dismissed without restarting", async () => {
    const user = userEvent.setup();

    render(
      <>
        <RestartRequiredBanner />
        <ProjectsPage />
      </>
    );
    await attachNewHead(user);
    await waitFor(() => expect(screen.getByText("Restart Lettuce now")).toBeInTheDocument());

    await user.click(screen.getByLabelText("Dismiss restart notice"));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(mockInvoke).not.toHaveBeenCalledWith("restart_daemon");
  });
});
