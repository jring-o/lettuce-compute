import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HeadSection } from "./head-section";
import type { HeadInfo } from "@/api/client";

function makeHead(overrides: Partial<HeadInfo> = {}): HeadInfo {
  return {
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
    ],
    ...overrides,
  };
}

describe("HeadSection", () => {
  const defaultProps = {
    showHeadWeight: true,
    containerStatus: null as import("@/api/client").ContainerRuntimeStatus | null,
    onHeadWeightChange: vi.fn(),
    onLeafToggle: vi.fn(),
    onLeafWeightChange: vi.fn(),
    onResetDefaults: vi.fn(),
    onDetach: vi.fn(),
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
        {
          id: "leaf-1",
          slug: "prime",
          name: "Prime Study",
          description: "Primes",
          research_area: "math",
          task_pattern: "PARAMETER_SWEEP",
          state: "ACTIVE",
          queued_work_units: 100,
          active_volunteers: 3,
          enabled: true,
          effective_weight: 50,
        },
        {
          id: "leaf-2",
          slug: "mandel",
          name: "Mandelbrot",
          description: "Fractals",
          research_area: "math",
          task_pattern: "PARAMETER_SWEEP",
          state: "ACTIVE",
          queued_work_units: 50,
          active_volunteers: 1,
          enabled: false,
          effective_weight: 30,
        },
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

    // Both checkboxes are checked; click first to disable
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
        {
          id: "leaf-c",
          slug: "container-leaf",
          name: "Container Leaf",
          description: "Needs container",
          research_area: "physics",
          task_pattern: "PARAMETER_SWEEP",
          state: "ACTIVE",
          queued_work_units: 100,
          active_volunteers: 1,
          enabled: true,
          effective_weight: 50,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        },
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
        {
          id: "leaf-c",
          slug: "container-leaf",
          name: "Container Leaf",
          description: "Needs container",
          research_area: "physics",
          task_pattern: "PARAMETER_SWEEP",
          state: "ACTIVE",
          queued_work_units: 100,
          active_volunteers: 1,
          enabled: true,
          effective_weight: 50,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        },
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
});
