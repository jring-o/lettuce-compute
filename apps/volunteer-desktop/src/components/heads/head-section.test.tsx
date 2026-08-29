import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HeadSection } from "./head-section";
import type { HeadInfo, LeafInfo, MachineCapabilities } from "@/api/client";

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
    active_hosts: 4,
    enabled: true,
    effective_weight: 50,
    ...overrides,
  };
}

function makeHead(overrides: Partial<HeadInfo> = {}): HeadInfo {
  return {
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
    ],
    ...overrides,
  };
}

function makeMachine(overrides: Partial<MachineCapabilities> = {}): MachineCapabilities {
  return {
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
    ...overrides,
  };
}

describe("HeadSection", () => {
  const defaultProps = {
    showHeadWeight: true,
    containerStatus: null as import("@/api/client").ContainerRuntimeStatus | null,
    machine: makeMachine(),
    trustedRuntimes: ["CONTAINER"] as string[] | null,
    onHeadWeightChange: vi.fn(),
    onLeafToggle: vi.fn(),
    onLeafWeightChange: vi.fn(),
    onResetDefaults: vi.fn(),
    onDetach: vi.fn(),
    onTrustChange: vi.fn().mockResolvedValue(undefined),
  };

  it("renders head name and connected status indicator", () => {
    const { container } = render(
      <HeadSection head={makeHead()} {...defaultProps} />
    );

    expect(screen.getByText("lettuce.science")).toBeInTheDocument();
    const greenDot = container.querySelector(".bg-green-500");
    expect(greenDot).toBeInTheDocument();
  });

  it("renders disconnected status indicator", () => {
    const { container } = render(
      <HeadSection head={makeHead({ status: "disconnected" })} {...defaultProps} />
    );

    const redDot = container.querySelector(".bg-red-500");
    expect(redDot).toBeInTheDocument();
  });

  it("shows the head version when the daemon reports one", () => {
    const { rerender } = render(
      <HeadSection head={makeHead({ head_version: "v1.4.0" })} {...defaultProps} />
    );
    expect(screen.getByText("head v1.4.0")).toBeInTheDocument();

    rerender(<HeadSection head={makeHead()} {...defaultProps} />);
    expect(screen.queryByText(/^head v/)).not.toBeInTheDocument();
  });

  it("shows a prominent notice when the head requires a newer app", () => {
    const { rerender } = render(
      <HeadSection head={makeHead({ update_required: true })} {...defaultProps} />
    );
    expect(screen.getByRole("alert")).toHaveTextContent(
      "This app is too old for this head. Update Lettuce Compute to keep contributing here."
    );

    rerender(<HeadSection head={makeHead({ update_required: false })} {...defaultProps} />);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("collapses and expands on click", async () => {
    const user = userEvent.setup();

    render(<HeadSection head={makeHead()} {...defaultProps} />);

    // Expanded by default — leafs visible
    expect(screen.getByText("Prime Gap Study")).toBeInTheDocument();

    // Click header to collapse
    await user.click(screen.getByText("lettuce.science"));
    expect(screen.queryByText("Prime Gap Study")).not.toBeInTheDocument();

    // Click again to expand
    await user.click(screen.getByText("lettuce.science"));
    expect(screen.getByText("Prime Gap Study")).toBeInTheDocument();
  });

  it("head weight slider onChange fires callback", () => {
    const onHeadWeightChange = vi.fn();

    const { container } = render(
      <HeadSection
        head={makeHead()}
        {...defaultProps}
        onHeadWeightChange={onHeadWeightChange}
      />
    );

    // The first range input is the head weight slider
    const sliders = container.querySelectorAll("input[type='range']");
    const headSlider = sliders[0] as HTMLInputElement;

    // Simulate native change event on range input
    Object.getOwnPropertyDescriptor(
      window.HTMLInputElement.prototype,
      "value"
    )?.set?.call(headSlider, "50");
    headSlider.dispatchEvent(new Event("change", { bubbles: true }));

    expect(onHeadWeightChange).toHaveBeenCalled();
  });

  it("detach button with confirmation", async () => {
    const user = userEvent.setup();
    const onDetach = vi.fn();

    render(
      <HeadSection head={makeHead()} {...defaultProps} onDetach={onDetach} />
    );

    // Click Detach
    await user.click(screen.getByText("Detach"));

    // Confirm dialog appears
    expect(screen.getByText("Confirm")).toBeInTheDocument();
    expect(screen.getByText("Cancel")).toBeInTheDocument();

    // Click Confirm
    await user.click(screen.getByText("Confirm"));
    expect(onDetach).toHaveBeenCalledOnce();
  });

  it("detach cancel dismisses confirmation", async () => {
    const user = userEvent.setup();
    const onDetach = vi.fn();

    render(
      <HeadSection head={makeHead()} {...defaultProps} onDetach={onDetach} />
    );

    await user.click(screen.getByText("Detach"));
    await user.click(screen.getByText("Cancel"));

    expect(onDetach).not.toHaveBeenCalled();
    // Back to showing Detach button
    expect(screen.getByText("Detach")).toBeInTheDocument();
  });

  it("hides head weight slider when showHeadWeight is false", () => {
    render(
      <HeadSection
        head={makeHead()}
        {...defaultProps}
        showHeadWeight={false}
      />
    );

    expect(screen.queryByText("Head weight")).not.toBeInTheDocument();
  });

  it("shows server URL", () => {
    render(<HeadSection head={makeHead()} {...defaultProps} />);
    expect(screen.getByText("https://lettuce.science")).toBeInTheDocument();
  });

  it("Use Defaults button calls onResetDefaults", async () => {
    const user = userEvent.setup();
    const onResetDefaults = vi.fn();

    render(
      <HeadSection
        head={makeHead()}
        {...defaultProps}
        onResetDefaults={onResetDefaults}
      />
    );

    await user.click(screen.getByText("Use Defaults"));
    expect(onResetDefaults).toHaveBeenCalledOnce();
  });

  it("shows leaf weight sliders when 2+ enabled leafs", () => {
    render(<HeadSection head={makeHead()} {...defaultProps} />);

    // Both leafs are enabled, so "Weight" labels should appear
    const weightLabels = screen.getAllByText("Weight");
    expect(weightLabels.length).toBe(2);
  });

  it("hides leaf weight sliders when only 1 enabled leaf", () => {
    const head = makeHead({
      leafs: [
        makeLeaf({ slug: "prime", name: "Prime Study" }),
        makeLeaf({ id: "leaf-2", slug: "mandel", name: "Mandelbrot", enabled: false }),
      ],
    });

    render(<HeadSection head={head} {...defaultProps} />);

    // Only 1 enabled leaf, so no "Weight" labels
    expect(screen.queryByText("Weight")).not.toBeInTheDocument();
  });

  it("calls onLeafToggle with slug and enabled value when toggling", async () => {
    const user = userEvent.setup();
    const onLeafToggle = vi.fn();

    render(
      <HeadSection
        head={makeHead()}
        {...defaultProps}
        onLeafToggle={onLeafToggle}
      />
    );

    // Both leaf checkboxes are checked; click the first to disable
    const checkboxes = screen.getAllByRole("checkbox");
    await user.click(checkboxes[0]);

    expect(onLeafToggle).toHaveBeenCalledWith("prime-gap-study", false);
  });

  it("passes containerStatus to LeafCard children", () => {
    const containerStatus = {
      backend: "podman" as const,
      status: "stopped" as const,
      version: "5.3.1",
      socket_path: "/run/podman/podman.sock",
      machine_required: true,
      machine_name: "default",
      machine_cpus: 4,
      machine_memory_mb: 4096,
      machine_disk_gb: 50,
      error: null,
    };

    const headWithContainerLeaf = makeHead({
      leafs: [
        makeLeaf({
          id: "leaf-c",
          slug: "container-leaf",
          name: "Container Leaf",
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        }),
      ],
    });

    render(
      <HeadSection
        head={headWithContainerLeaf}
        {...defaultProps}
        containerStatus={containerStatus}
      />
    );

    // The container leaf should show the warning since runtime is stopped
    expect(screen.getByText("Container runtime required")).toBeInTheDocument();
  });

  it("does not show container warning when containerStatus has running status", () => {
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

    const headWithContainerLeaf = makeHead({
      leafs: [
        makeLeaf({
          id: "leaf-c",
          slug: "container-leaf",
          name: "Container Leaf",
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        }),
      ],
    });

    render(
      <HeadSection
        head={headWithContainerLeaf}
        {...defaultProps}
        containerStatus={containerStatus}
      />
    );

    expect(
      screen.queryByText("Container runtime required")
    ).not.toBeInTheDocument();
  });

  // --- Freshness ---

  it("shows how old the leaf figures are", () => {
    const twoHoursAgo = new Date(Date.now() - 2 * 3600 * 1000).toISOString();
    render(<HeadSection head={makeHead({ leafs_refreshed_at: twoHoursAgo })} {...defaultProps} />);
    expect(screen.getByText(/Leaf figures as of 2h ago/)).toBeInTheDocument();
    expect(screen.getByText(/refresh only when Lettuce last tried to fetch work/)).toBeInTheDocument();
  });

  it("says the leaf figures have not been fetched when there is no timestamp", () => {
    render(<HeadSection head={makeHead()} {...defaultProps} />);
    expect(screen.getByText(/Leaf figures not fetched yet/)).toBeInTheDocument();
  });

  // --- Runtime trust ---

  it("summarises what the head may run from its trusted runtimes", () => {
    render(<HeadSection head={makeHead()} {...defaultProps} trustedRuntimes={["CONTAINER"]} />);

    expect(screen.getByText("May run:")).toBeInTheDocument();
    expect(screen.getByText("WASM")).toHaveTextContent("WASM ✓");
    expect(screen.getByText("Container")).toHaveTextContent("Container ✓");
    expect(screen.getByText("Native")).toHaveTextContent("Native ✗");
    expect(screen.getByText("Change...")).toBeInTheDocument();
  });

  it("shows WASM only when the trust list is empty and flags unknown trust", () => {
    const { rerender } = render(
      <HeadSection head={makeHead()} {...defaultProps} trustedRuntimes={[]} />
    );
    expect(screen.getByText("Container")).toHaveTextContent("Container ✗");
    expect(screen.getByText("Native")).toHaveTextContent("Native ✗");
    expect(screen.queryByText("(trust settings not loaded)")).not.toBeInTheDocument();

    rerender(<HeadSection head={makeHead()} {...defaultProps} trustedRuntimes={null} />);
    expect(screen.getByText("(trust settings not loaded)")).toBeInTheDocument();
  });

  it("opens the trust editor with the current trust and saves the new list", async () => {
    const user = userEvent.setup();
    const onTrustChange = vi.fn().mockResolvedValue(undefined);

    render(
      <HeadSection
        head={makeHead()}
        {...defaultProps}
        trustedRuntimes={["CONTAINER"]}
        onTrustChange={onTrustChange}
      />
    );

    await user.click(screen.getByText("Change..."));

    const container = screen.getByLabelText("Allow container tasks from this head");
    const native = screen.getByLabelText("Allow native tasks from this head");
    expect(container).toBeChecked();
    expect(native).not.toBeChecked();
    expect(screen.getByText(/Allow this only for an operator you fully trust/)).toBeInTheDocument();

    await user.click(native);
    await user.click(screen.getByText("Save trust settings"));

    await waitFor(() => {
      expect(onTrustChange).toHaveBeenCalledWith(["CONTAINER", "NATIVE"]);
    });
    // Editor closes on success
    await waitFor(() => {
      expect(screen.queryByText("Save trust settings")).not.toBeInTheDocument();
    });
  });

  it("saves an empty list when both runtimes are unticked", async () => {
    const user = userEvent.setup();
    const onTrustChange = vi.fn().mockResolvedValue(undefined);

    render(
      <HeadSection
        head={makeHead()}
        {...defaultProps}
        trustedRuntimes={["CONTAINER", "NATIVE"]}
        onTrustChange={onTrustChange}
      />
    );

    await user.click(screen.getByText("Change..."));
    await user.click(screen.getByLabelText("Allow container tasks from this head"));
    await user.click(screen.getByLabelText("Allow native tasks from this head"));
    await user.click(screen.getByText("Save trust settings"));

    await waitFor(() => {
      expect(onTrustChange).toHaveBeenCalledWith([]);
    });
  });

  it("disables the container checkbox when this machine has no container backend", async () => {
    const user = userEvent.setup();

    render(
      <HeadSection
        head={makeHead()}
        {...defaultProps}
        machine={makeMachine({ runtimes: ["wasm"] })}
        trustedRuntimes={[]}
      />
    );

    await user.click(screen.getByText("Change..."));
    expect(screen.getByLabelText("Allow container tasks from this head")).toBeDisabled();
    expect(screen.getByText(/No Docker or Podman backend was detected/)).toBeInTheDocument();
  });

  it("shows the error and keeps the editor open when saving fails", async () => {
    const user = userEvent.setup();
    const onTrustChange = vi.fn().mockRejectedValue(new Error("daemon unreachable"));

    render(
      <HeadSection head={makeHead()} {...defaultProps} onTrustChange={onTrustChange} />
    );

    await user.click(screen.getByText("Change..."));
    await user.click(screen.getByText("Save trust settings"));

    await waitFor(() => {
      expect(screen.getByText("daemon unreachable")).toBeInTheDocument();
    });
    expect(screen.getByText("Save trust settings")).toBeInTheDocument();
  });

  it("cancel closes the editor without saving", async () => {
    const user = userEvent.setup();
    const onTrustChange = vi.fn();

    render(
      <HeadSection head={makeHead()} {...defaultProps} onTrustChange={onTrustChange} />
    );

    await user.click(screen.getByText("Change..."));
    await user.click(screen.getByLabelText("Allow native tasks from this head"));
    // The editor's Cancel is the one next to "Save trust settings"
    await user.click(screen.getAllByText("Cancel")[0]);

    expect(onTrustChange).not.toHaveBeenCalled();
    expect(screen.queryByText("Save trust settings")).not.toBeInTheDocument();
  });

  it("passes the raise-disk handler through to leaf cards", async () => {
    const user = userEvent.setup();
    const onRaiseDisk = vi.fn().mockResolvedValue(undefined);
    const head = makeHead({
      leafs: [makeLeaf({ disk_gate: { blocked: true, reason: "too big", raise_to_gb: 30 } })],
    });

    render(<HeadSection head={head} {...defaultProps} onRaiseDisk={onRaiseDisk} />);

    await user.click(screen.getByText("Raise disk allowance to 30 GB"));
    expect(onRaiseDisk).toHaveBeenCalledWith(30);
  });
});
