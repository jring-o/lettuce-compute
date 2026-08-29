import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ProjectsPage } from "./projects";
import type { HeadInfo } from "@/api/client";

const mockRefetch = vi.fn();
const mockSetHeads = vi.fn();
const mockWriteLeafPrefs = vi.fn().mockResolvedValue(undefined);
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

vi.mock("@/hooks/use-heads", () => ({
  useHeads: () => mockUseHeads(),
  useWriteLeafPreferences: () => ({
    write: mockWriteLeafPrefs,
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

const mockHeads: HeadInfo[] = [
  {
    name: "lettuce.science",
    description: "Open science compute",
    url: "https://lettuce.science",
    grpc_address: "lettuce.science:443",
    status: "connected",
    weight: 70,
    leafs: [
      {
        id: "leaf-1",
        slug: "prime-gap-study",
        name: "Prime Gap Study",
        description: "Finding prime gaps",
        research_area: "mathematics",
        task_pattern: "PARAMETER_SWEEP",
        state: "ACTIVE",
        queued_work_units: 450,
        active_volunteers: 3,
        enabled: true,
        effective_weight: 50,
      },
      {
        id: "leaf-2",
        slug: "mandelbrot",
        name: "Mandelbrot Analysis",
        description: "Fractal analysis",
        research_area: "mathematics",
        task_pattern: "PARAMETER_SWEEP",
        state: "ACTIVE",
        queued_work_units: 200,
        active_volunteers: 2,
        enabled: true,
        effective_weight: 30,
      },
      {
        id: "leaf-3",
        slug: "monte-carlo-pi",
        name: "Monte Carlo Pi",
        description: "Pi estimation",
        research_area: "mathematics",
        task_pattern: "PARAMETER_SWEEP",
        state: "ACTIVE",
        queued_work_units: 100,
        active_volunteers: 1,
        enabled: false,
        effective_weight: 20,
      },
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
      {
        id: "leaf-4",
        slug: "gw-search",
        name: "Gravitational Wave Search",
        description: "Searching for gravitational waves",
        research_area: "physics",
        task_pattern: "PARAMETER_SWEEP",
        state: "ACTIVE",
        queued_work_units: 1000,
        active_volunteers: 50,
        enabled: true,
        effective_weight: 100,
      },
    ],
  },
];

describe("ProjectsPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseClient.mockReturnValue({ client: mockClient, error: null });
    mockClient.detachHead.mockResolvedValue(undefined);
    mockClient.attachHead.mockResolvedValue(undefined);
    mockUseContainerRuntime.mockReturnValue({
      status: null,
      loading: false,
      error: null,
      refresh: vi.fn(),
    });
  });

  it("renders head sections for each connected server", () => {
    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);
    expect(screen.getByText("lettuce.science")).toBeInTheDocument();
    expect(screen.getByText("einstein@home")).toBeInTheDocument();
  });

  it("renders leaf cards within each head", () => {
    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);
    expect(screen.getByText("Prime Gap Study")).toBeInTheDocument();
    expect(screen.getByText("Mandelbrot Analysis")).toBeInTheDocument();
    expect(screen.getByText("Monte Carlo Pi")).toBeInTheDocument();
    expect(screen.getByText("Gravitational Wave Search")).toBeInTheDocument();
  });

  it("shows disabled state for disabled leafs", () => {
    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);
    expect(screen.getByText("(disabled)")).toBeInTheDocument();
  });

  it("toggle leaf calls update function", async () => {
    const user = userEvent.setup();

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);

    // Monte Carlo Pi is the unchecked checkbox (disabled leaf)
    const checkboxes = screen.getAllByRole("checkbox");
    // Find the unchecked one
    const unchecked = checkboxes.find(
      (cb) => !(cb as HTMLInputElement).checked
    );
    expect(unchecked).toBeDefined();

    await user.click(unchecked!);

    expect(mockWriteLeafPrefs).toHaveBeenCalled();
  });

  it("head weight slider is visible when 2+ heads exist", () => {
    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);
    // Both heads should show "Head weight" label
    expect(screen.getAllByText("Head weight")).toHaveLength(2);
  });

  it("leaf weight slider visibility", () => {
    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    const { container } = render(<ProjectsPage />);

    // lettuce.science has 2 enabled leafs -> weight sliders visible
    // look for "Weight" labels (leaf weight labels)
    const weightLabels = screen.getAllByText("Weight");
    expect(weightLabels.length).toBe(2); // 2 enabled leafs on lettuce.science

    // einstein@home has 1 enabled leaf -> no leaf weight slider
    // (only head weight and the 2 leaf weights = verified above)
  });

  it("Use Defaults button resets leaf preferences", async () => {
    const user = userEvent.setup();

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);

    const defaultBtns = screen.getAllByText("Use Defaults");
    await user.click(defaultBtns[0]);

    expect(mockWriteLeafPrefs).toHaveBeenCalledWith(
      "lettuce.science",
      { mode: "ALL" }
    );
  });

  it("Add Server button opens dialog", async () => {
    const user = userEvent.setup();

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);

    await user.click(screen.getByText("+ Add Server"));
    expect(
      screen.getByPlaceholderText("https://compute.example.org")
    ).toBeInTheDocument();
  });

  it("empty state when no heads", () => {
    mockUseHeads.mockReturnValue({
      heads: [],
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);
    expect(
      screen.getByText("No servers connected. Add a server to start contributing.")
    ).toBeInTheDocument();
  });

  it("loading state shows skeletons", () => {
    mockUseHeads.mockReturnValue({
      heads: [],
      isLoading: true,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    const { container } = render(<ProjectsPage />);
    const skeletons = container.querySelectorAll("[class*='animate-pulse']");
    expect(skeletons.length).toBeGreaterThan(0);
  });

  it("renders error message when useHeads returns error", () => {
    mockUseHeads.mockReturnValue({
      heads: [],
      isLoading: false,
      error: new Error("Network timeout"),
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);
    expect(
      screen.getByText("Failed to load servers: Network timeout")
    ).toBeInTheDocument();
  });

  it("detach calls client.detachHead and refetches heads", async () => {
    const user = userEvent.setup();

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);

    // Click Detach on the first head
    const detachButtons = screen.getAllByText("Detach");
    await user.click(detachButtons[0]);

    // Confirm
    await user.click(screen.getAllByText("Confirm")[0]);

    expect(mockClient.detachHead).toHaveBeenCalledWith({
      server_name: "lettuce.science",
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

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);

    const detachButtons = screen.getAllByText("Detach");
    await user.click(detachButtons[0]);
    await user.click(screen.getAllByText("Confirm")[0]);

    await waitFor(() => {
      expect(screen.getByText("Server busy")).toBeInTheDocument();
    });
  });

  it("head weight slider hidden when only 1 head", () => {
    mockUseHeads.mockReturnValue({
      heads: [mockHeads[0]],
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);
    expect(screen.queryByText("Head weight")).not.toBeInTheDocument();
  });

  it("leaf toggle sets mode ALL when all leafs enabled", async () => {
    const user = userEvent.setup();

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);

    // Monte Carlo Pi is the disabled leaf; enable it to make all leafs enabled
    const checkboxes = screen.getAllByRole("checkbox");
    const unchecked = checkboxes.find(
      (cb) => !(cb as HTMLInputElement).checked
    );
    await user.click(unchecked!);

    // When all 3 leafs become enabled, mode should be ALL
    expect(mockWriteLeafPrefs).toHaveBeenCalledWith(
      "lettuce.science",
      { mode: "ALL" }
    );
  });

  it("leaf toggle sets mode SPECIFIC when disabling a leaf", async () => {
    const user = userEvent.setup();

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    render(<ProjectsPage />);

    // Disable a currently enabled leaf (first checked checkbox under lettuce.science)
    const checkboxes = screen.getAllByRole("checkbox");
    const firstChecked = checkboxes.find(
      (cb) => (cb as HTMLInputElement).checked
    );
    await user.click(firstChecked!);

    expect(mockWriteLeafPrefs).toHaveBeenCalledWith(
      "lettuce.science",
      expect.objectContaining({ mode: "SPECIFIC" })
    );
  });

  // --- Container warning toast tests (S89) ---

  it("shows warning toast when enabling a container leaf without running container runtime", async () => {
    const user = userEvent.setup();

    const headsWithContainerLeaf: HeadInfo[] = [
      {
        name: "lettuce.science",
        description: "Open science compute",
        url: "https://lettuce.science",
        grpc_address: "lettuce.science:443",
        status: "connected",
        weight: 100,
        leafs: [
          {
            id: "leaf-c1",
            slug: "nbody-sim",
            name: "N-Body Simulation",
            description: "Gravitational N-body simulation",
            research_area: "physics",
            task_pattern: "PARAMETER_SWEEP",
            state: "ACTIVE",
            queued_work_units: 200,
            active_volunteers: 5,
            enabled: false,
            effective_weight: 50,
            execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
          },
        ],
      },
    ];

    mockUseHeads.mockReturnValue({
      heads: headsWithContainerLeaf,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

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

    const headsWithContainerLeaf: HeadInfo[] = [
      {
        name: "lettuce.science",
        description: "Open science compute",
        url: "https://lettuce.science",
        grpc_address: "lettuce.science:443",
        status: "connected",
        weight: 100,
        leafs: [
          {
            id: "leaf-c1",
            slug: "nbody-sim",
            name: "N-Body Simulation",
            description: "Gravitational N-body simulation",
            research_area: "physics",
            task_pattern: "PARAMETER_SWEEP",
            state: "ACTIVE",
            queued_work_units: 200,
            active_volunteers: 5,
            enabled: false,
            effective_weight: 50,
            execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
          },
        ],
      },
    ];

    mockUseHeads.mockReturnValue({
      heads: headsWithContainerLeaf,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

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

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

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

  it("passes containerStatus to HeadSection components", () => {
    const containerStatus = {
      backend: "podman" as const,
      status: "running" as const,
      version: "5.3.1",
      socket_path: "/run/podman/podman.sock",
      machine_required: false,
      machine_name: "",
      machine_cpus: 0,
      machine_memory_mb: 0,
      machine_disk_gb: 0,
      error: null,
    };

    mockUseContainerRuntime.mockReturnValue({
      status: containerStatus,
      loading: false,
      error: null,
      refresh: vi.fn(),
    });

    mockUseHeads.mockReturnValue({
      heads: mockHeads,
      isLoading: false,
      error: null,
      refetch: mockRefetch,
      setHeads: mockSetHeads,
    });

    // Just verify it renders without errors — the prop passthrough
    // is validated by the HeadSection/LeafCard tests
    render(<ProjectsPage />);
    expect(screen.getByText("lettuce.science")).toBeInTheDocument();
  });
});
