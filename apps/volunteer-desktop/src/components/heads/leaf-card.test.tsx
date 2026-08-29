import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { LeafCard } from "./leaf-card";
import type { LeafInfo, ContainerRuntimeStatus } from "@/api/client";

// Mock @tauri-apps/api/event
const mockEmit = vi.fn();
vi.mock("@tauri-apps/api/event", () => ({
  emit: (...args: unknown[]) => mockEmit(...args),
  listen: vi.fn(() => Promise.resolve(() => {})),
}));

// Mock @tauri-apps/plugin-opener
const mockOpenUrl = vi.fn();
vi.mock("@tauri-apps/plugin-opener", () => ({
  openUrl: (...args: unknown[]) => mockOpenUrl(...args),
}));

function makeLeaf(overrides: Partial<LeafInfo> = {}): LeafInfo {
  return {
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
    ...overrides,
  };
}

function makeContainerStatus(
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

const defaultProps = {
  showWeightSlider: false,
  containerStatus: null as ContainerRuntimeStatus | null,
  onToggle: vi.fn(),
  onWeightChange: vi.fn(),
};

describe("LeafCard", () => {
  it("renders leaf name, research area, and stats", () => {
    render(
      <LeafCard leaf={makeLeaf()} {...defaultProps} />
    );

    expect(screen.getByText("Prime Gap Study")).toBeInTheDocument();
    expect(screen.getByText("mathematics")).toBeInTheDocument();
    expect(screen.getByText("PARAMETER_SWEEP")).toBeInTheDocument();
    expect(screen.getByText(/450 queued/)).toBeInTheDocument();
    expect(screen.getByText(/3 volunteers/)).toBeInTheDocument();
  });

  it("checkbox reflects enabled state when true", () => {
    render(
      <LeafCard leaf={makeLeaf({ enabled: true })} {...defaultProps} />
    );

    const checkbox = screen.getByRole("checkbox");
    expect(checkbox).toBeChecked();
  });

  it("checkbox reflects enabled state when false", () => {
    render(
      <LeafCard leaf={makeLeaf({ enabled: false })} {...defaultProps} />
    );

    const checkbox = screen.getByRole("checkbox");
    expect(checkbox).not.toBeChecked();
  });

  it("toggle calls onToggle with opposite boolean", async () => {
    const user = userEvent.setup();
    const onToggle = vi.fn();

    render(
      <LeafCard
        leaf={makeLeaf({ enabled: true })}
        {...defaultProps}
        onToggle={onToggle}
      />
    );

    await user.click(screen.getByRole("checkbox"));
    expect(onToggle).toHaveBeenCalledWith(false);
  });

  it("weight slider shown only when showWeightSlider is true", () => {
    const { container, rerender } = render(
      <LeafCard leaf={makeLeaf()} {...defaultProps} showWeightSlider={false} />
    );

    expect(container.querySelector("input[type='range']")).not.toBeInTheDocument();

    rerender(
      <LeafCard leaf={makeLeaf()} {...defaultProps} showWeightSlider={true} />
    );

    expect(container.querySelector("input[type='range']")).toBeInTheDocument();
  });

  it("disabled leaf shows reduced opacity", () => {
    const { container } = render(
      <LeafCard leaf={makeLeaf({ enabled: false })} {...defaultProps} />
    );

    const card = container.firstElementChild;
    expect(card?.className).toContain("opacity-50");
  });

  it("disabled leaf shows (disabled) label", () => {
    render(
      <LeafCard leaf={makeLeaf({ enabled: false })} {...defaultProps} />
    );

    expect(screen.getByText("(disabled)")).toBeInTheDocument();
  });

  it("enabled leaf does NOT show (disabled) label", () => {
    render(
      <LeafCard leaf={makeLeaf({ enabled: true })} {...defaultProps} />
    );

    expect(screen.queryByText("(disabled)")).not.toBeInTheDocument();
  });

  it("weight slider hidden when showWeightSlider=true but leaf is disabled", () => {
    const { container } = render(
      <LeafCard
        leaf={makeLeaf({ enabled: false })}
        {...defaultProps}
        showWeightSlider={true}
      />
    );

    // Slider should NOT appear because leaf is disabled
    expect(container.querySelector("input[type='range']")).not.toBeInTheDocument();
  });

  it("toggle from unchecked to checked calls onToggle(true)", async () => {
    const user = userEvent.setup();
    const onToggle = vi.fn();

    render(
      <LeafCard
        leaf={makeLeaf({ enabled: false })}
        {...defaultProps}
        onToggle={onToggle}
      />
    );

    await user.click(screen.getByRole("checkbox"));
    expect(onToggle).toHaveBeenCalledWith(true);
  });

  it("displays effective_weight value in weight slider label", () => {
    render(
      <LeafCard
        leaf={makeLeaf({ effective_weight: 75 })}
        {...defaultProps}
        showWeightSlider={true}
      />
    );

    expect(screen.getByText("75")).toBeInTheDocument();
  });

  it("enabled leaf does NOT have opacity-50 class", () => {
    const { container } = render(
      <LeafCard leaf={makeLeaf({ enabled: true })} {...defaultProps} />
    );

    const card = container.firstElementChild;
    expect(card?.className).not.toContain("opacity-50");
  });

  // --- Runtime badge tests (S89) ---

  it("shows Container badge when execution_spec has image", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("Container")).toBeInTheDocument();
  });

  it("does not show Container badge when no execution_spec", () => {
    render(
      <LeafCard leaf={makeLeaf()} {...defaultProps} />
    );

    expect(screen.queryByText("Container")).not.toBeInTheDocument();
  });

  it("shows Native badge when execution_spec has binaries", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: {
            binaries: { "linux-amd64": "/path/to/bin" },
          },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("Native")).toBeInTheDocument();
  });

  it("does not show Native badge when binaries is empty", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: { binaries: {} },
        })}
        {...defaultProps}
      />
    );

    expect(screen.queryByText("Native")).not.toBeInTheDocument();
  });

  it("shows GPU badge when gpu_required is true", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: { gpu_required: true, gpu_type: "CUDA" },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("CUDA")).toBeInTheDocument();
  });

  it("shows GPU fallback text when gpu_type is not set", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: { gpu_required: true },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("GPU")).toBeInTheDocument();
  });

  it("does not show GPU badge when gpu_required is false", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: { gpu_required: false },
        })}
        {...defaultProps}
      />
    );

    expect(screen.queryByText("GPU")).not.toBeInTheDocument();
  });

  it("shows all badges together for container+GPU leaf", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: {
            image: "ghcr.io/lettuce/nbody:latest",
            gpu_required: true,
            gpu_type: "CUDA",
          },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("Container")).toBeInTheDocument();
    expect(screen.getByText("CUDA")).toBeInTheDocument();
  });

  it("shows WASM badge when execution_spec has binaries.wasm", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: {
            binaries: { wasm: "https://example.com/module.wasm" },
          },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("WASM")).toBeInTheDocument();
  });

  it("does not show WASM badge when no wasm binary", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: {
            binaries: { "linux-amd64": "/path/to/bin" },
          },
        })}
        {...defaultProps}
      />
    );

    expect(screen.queryByText("WASM")).not.toBeInTheDocument();
  });

  it("shows WASM badge but not Native badge for wasm-only leaf", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: {
            binaries: { wasm: "https://example.com/module.wasm" },
          },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("WASM")).toBeInTheDocument();
    expect(screen.queryByText("Native")).not.toBeInTheDocument();
  });

  it("shows both Native and WASM badges for multi-runtime leaf", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: {
            binaries: {
              wasm: "https://example.com/module.wasm",
              "linux-amd64": "/path/to/bin",
            },
          },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("WASM")).toBeInTheDocument();
    expect(screen.getByText("Native")).toBeInTheDocument();
  });

  // --- Container unavailability warning tests (S89) ---

  it("shows 'Container runtime required' when container leaf and runtime not running", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          enabled: true,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        })}
        {...defaultProps}
        containerStatus={makeContainerStatus({ status: "stopped" })}
      />
    );

    expect(screen.getByText("Container runtime required")).toBeInTheDocument();
  });

  it("does NOT show warning when container leaf and runtime is running", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          enabled: true,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        })}
        {...defaultProps}
        containerStatus={makeContainerStatus({ status: "running" })}
      />
    );

    expect(
      screen.queryByText("Container runtime required")
    ).not.toBeInTheDocument();
  });

  it("does NOT show warning for native-only leaf when runtime is stopped", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          enabled: true,
          execution_spec: {
            binaries: { "linux-amd64": "/bin" },
          },
        })}
        {...defaultProps}
        containerStatus={makeContainerStatus({ status: "stopped" })}
      />
    );

    expect(
      screen.queryByText("Container runtime required")
    ).not.toBeInTheDocument();
  });

  it("container unavailable leaf has opacity-60 when enabled", () => {
    const { container } = render(
      <LeafCard
        leaf={makeLeaf({
          enabled: true,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        })}
        {...defaultProps}
        containerStatus={makeContainerStatus({ status: "not_installed" })}
      />
    );

    const card = container.firstElementChild;
    expect(card?.className).toContain("opacity-60");
  });

  it("does NOT show container warning when containerStatus is null", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          enabled: true,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        })}
        {...defaultProps}
        containerStatus={null}
      />
    );

    expect(screen.getByText("Container runtime required")).toBeInTheDocument();
  });

  // --- Coverage gap: Container + Native combined leaf shows both badges ---

  it("shows both Container and Native badges when leaf has image and binaries", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: {
            image: "ghcr.io/lettuce/nbody:latest",
            binaries: { "linux-amd64": "/path/to/bin" },
          },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("Container")).toBeInTheDocument();
    expect(screen.getByText("Native")).toBeInTheDocument();
  });

  // --- Coverage gap: "Container runtime required" click emits navigate:settings ---

  it("clicking 'Container runtime required' emits navigate:settings", async () => {
    const user = userEvent.setup();
    mockEmit.mockClear();

    render(
      <LeafCard
        leaf={makeLeaf({
          enabled: true,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        })}
        {...defaultProps}
        containerStatus={makeContainerStatus({ status: "stopped" })}
      />
    );

    const warningLink = screen.getByText("Container runtime required");
    await user.click(warningLink);

    expect(mockEmit).toHaveBeenCalledWith("navigate:settings");
  });

  // --- Coverage gap: container unavailable with not_initialized status ---

  it("shows warning when container leaf and runtime not_initialized", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          enabled: true,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        })}
        {...defaultProps}
        containerStatus={makeContainerStatus({ status: "not_initialized" })}
      />
    );

    expect(screen.getByText("Container runtime required")).toBeInTheDocument();
  });

  // --- Coverage gap: container unavailable does NOT apply opacity when disabled ---

  it("container unavailable + disabled leaf uses opacity-50 not opacity-60", () => {
    const { container } = render(
      <LeafCard
        leaf={makeLeaf({
          enabled: false,
          execution_spec: { image: "ghcr.io/lettuce/nbody:latest" },
        })}
        {...defaultProps}
        containerStatus={makeContainerStatus({ status: "stopped" })}
      />
    );

    const card = container.firstElementChild;
    // Disabled takes precedence (opacity-50 from !leaf.enabled)
    expect(card?.className).toContain("opacity-50");
  });

  // --- S109: "View My Results" link tests ---

  it("renders 'View My Results' button and opens URL via opener plugin", async () => {
    const user = userEvent.setup();
    render(
      <LeafCard
        leaf={makeLeaf({ slug: "prime-gap-study" })}
        {...defaultProps}
        dashboardUrl="https://lettuce.science"
        volunteerId="vol-abc123"
      />
    );

    const button = screen.getByText("View My Results");
    expect(button).toBeInTheDocument();
    expect(button.tagName).toBe("BUTTON");

    await user.click(button);
    expect(mockOpenUrl).toHaveBeenCalledWith(
      "https://lettuce.science/leafs/prime-gap-study/visualize?volunteer=vol-abc123"
    );
  });

  it("does not render 'View My Results' link when dashboardUrl is missing", () => {
    render(
      <LeafCard
        leaf={makeLeaf()}
        {...defaultProps}
        volunteerId="vol-abc123"
      />
    );

    expect(screen.queryByText("View My Results")).not.toBeInTheDocument();
  });

  it("does not render 'View My Results' link when volunteerId is missing", () => {
    render(
      <LeafCard
        leaf={makeLeaf()}
        {...defaultProps}
        dashboardUrl="https://lettuce.science"
      />
    );

    expect(screen.queryByText("View My Results")).not.toBeInTheDocument();
  });

  it("does not render 'View My Results' link when both dashboardUrl and volunteerId are missing", () => {
    render(
      <LeafCard
        leaf={makeLeaf()}
        {...defaultProps}
      />
    );

    expect(screen.queryByText("View My Results")).not.toBeInTheDocument();
  });
});
