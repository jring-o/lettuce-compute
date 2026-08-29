import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { LeafCard } from "./leaf-card";
import type { LeafInfo, ContainerRuntimeStatus, MachineCapabilities } from "@/api/client";

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
    research_area: ["mathematics"],
    task_pattern: "PARAMETER_SWEEP",
    state: "ACTIVE",
    queued_work_units: 450,
    active_volunteers: 3,
    active_hosts: 5,
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

function makeMachine(overrides: Partial<MachineCapabilities> = {}): MachineCapabilities {
  return {
    runtimes: ["container", "wasm"],
    has_gpu: true,
    max_memory_mb: 8192,
    max_disk_mb: 10240,
    max_cpu_cores: 4,
    max_gpu_vram_mb: 2048,
    gpu_card_vram_mb: 4096,
    gpu_vram_pct: 50,
    gpu_vendors: ["NVIDIA"],
    gpu_compute_capabilities: ["8.6"],
    ...overrides,
  };
}

const defaultProps = {
  showWeightSlider: false,
  containerStatus: null as ContainerRuntimeStatus | null,
  machine: null as MachineCapabilities | null,
  trustedRuntimes: null as string[] | null,
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
    expect(screen.getByText(/5 hosts/)).toBeInTheDocument();
  });

  it("joins several research areas and hides the chip when there are none", () => {
    const { rerender } = render(
      <LeafCard leaf={makeLeaf({ research_area: ["physics", "astronomy"] })} {...defaultProps} />
    );
    expect(screen.getByText("physics, astronomy")).toBeInTheDocument();

    rerender(<LeafCard leaf={makeLeaf({ research_area: [] })} {...defaultProps} />);
    expect(screen.queryByText("physics, astronomy")).not.toBeInTheDocument();
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

  it("shows the GPU badge when only the resource requirements ask for a GPU", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          resource_requirements: { gpu_required: true, gpu_type: "NVIDIA" },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText("NVIDIA")).toBeInTheDocument();
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

  // --- Runtime trust ---

  it("greys the Container badge and says so when the head is not trusted with containers", () => {
    render(
      <LeafCard
        leaf={makeLeaf({ execution_spec: { image: "ghcr.io/lettuce/nbody:latest" } })}
        {...defaultProps}
        trustedRuntimes={[]}
      />
    );

    const badge = screen.getByTestId("runtime-badge-container");
    expect(badge).toHaveAttribute("data-trusted", "false");
    expect(badge).toHaveAttribute("title", "Container is not allowed by your trust settings for this head");
    expect(
      screen.getByText("Container is not allowed by your trust settings for this head.")
    ).toBeInTheDocument();
  });

  it("greys only the untrusted runtime and never WASM", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: { binaries: { wasm: "m.wasm", "linux-amd64": "/bin" } },
        })}
        {...defaultProps}
        trustedRuntimes={["CONTAINER"]}
      />
    );

    expect(screen.getByTestId("runtime-badge-native")).toHaveAttribute("data-trusted", "false");
    expect(screen.getByTestId("runtime-badge-wasm")).toHaveAttribute("data-trusted", "true");
    expect(
      screen.getByText("Native is not allowed by your trust settings for this head.")
    ).toBeInTheDocument();
  });

  it("does not grey anything when the runtime is trusted or trust is unknown", () => {
    const leaf = makeLeaf({ execution_spec: { image: "ghcr.io/lettuce/nbody:latest" } });
    const { rerender } = render(
      <LeafCard leaf={leaf} {...defaultProps} trustedRuntimes={["CONTAINER"]} />
    );
    expect(screen.getByTestId("runtime-badge-container")).toHaveAttribute("data-trusted", "true");
    expect(screen.queryByText(/not allowed by your trust settings/)).not.toBeInTheDocument();

    rerender(<LeafCard leaf={leaf} {...defaultProps} trustedRuntimes={null} />);
    expect(screen.getByTestId("runtime-badge-container")).toHaveAttribute("data-trusted", "true");
  });

  // --- Requirements ---

  it("renders the requirements line with no marks on a capable machine", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: { max_memory_mb: 7168, gpu_required: true },
          resource_requirements: {
            min_disk_mb: 5120,
            min_cpu_cores: 1,
            min_gpu_vram_mb: 1024,
            gpu_type: "NVIDIA",
          },
        })}
        {...defaultProps}
        machine={makeMachine()}
      />
    );

    const line = screen.getByText(/^Needs:/);
    expect(line).toHaveTextContent("Needs: 5 GB disk · 7 GB RAM · 1 core · NVIDIA GPU, 1 GB VRAM");
    expect(screen.getByTestId("requirement-disk")).toHaveAttribute("data-short", "false");
    expect(screen.getByTestId("requirement-gpu")).toHaveAttribute("data-short", "false");
  });

  it("marks the items this machine falls short on and explains the VRAM allowance", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          execution_spec: { max_memory_mb: 2048, gpu_required: true },
          resource_requirements: { min_disk_mb: 15360, min_gpu_vram_mb: 3072, gpu_type: "NVIDIA" },
        })}
        {...defaultProps}
        machine={makeMachine()}
      />
    );

    const disk = screen.getByTestId("requirement-disk");
    expect(disk).toHaveAttribute("data-short", "true");
    expect(disk).toHaveTextContent("15 GB disk (you allow 10 GB)");
    expect(screen.getByTestId("requirement-memory")).toHaveAttribute("data-short", "false");
    const gpu = screen.getByTestId("requirement-gpu");
    expect(gpu).toHaveAttribute("data-short", "true");
    expect(gpu).toHaveTextContent(
      "NVIDIA GPU, 3 GB VRAM (your allowance is 2 GB (50% of a 4 GB card))"
    );
  });

  it("renders no requirements line for a leaf without requirements", () => {
    render(<LeafCard leaf={makeLeaf()} {...defaultProps} machine={makeMachine()} />);
    expect(screen.queryByText(/^Needs:/)).not.toBeInTheDocument();
  });

  // --- Disk gate ---

  it("shows the daemon's disk-gate verdict and a button to raise the allowance", async () => {
    const user = userEvent.setup();
    const onRaiseDisk = vi.fn().mockResolvedValue(undefined);

    render(
      <LeafCard
        leaf={makeLeaf({
          disk_gate: {
            blocked: true,
            reason: "needs 15 GB of free allowance; 4 GB left of your 10 GB",
            raise_to_gb: 21,
          },
        })}
        {...defaultProps}
        onRaiseDisk={onRaiseDisk}
      />
    );

    expect(
      screen.getByText("Will not fetch: needs 15 GB of free allowance; 4 GB left of your 10 GB")
    ).toBeInTheDocument();

    await user.click(screen.getByText("Raise disk allowance to 21 GB"));
    expect(onRaiseDisk).toHaveBeenCalledWith(21);
  });

  it("shows the verdict without a button when the daemon gives no figure to raise to", () => {
    render(
      <LeafCard
        leaf={makeLeaf({ disk_gate: { blocked: true, reason: "image not cached yet" } })}
        {...defaultProps}
        onRaiseDisk={vi.fn()}
      />
    );

    expect(screen.getByText("Will not fetch: image not cached yet")).toBeInTheDocument();
    expect(screen.queryByText(/Raise disk allowance/)).not.toBeInTheDocument();
  });

  it("shows the error when raising the allowance fails", async () => {
    const user = userEvent.setup();
    const onRaiseDisk = vi.fn().mockRejectedValue(new Error("VALIDATION_ERROR: max_disk_gb too large"));

    render(
      <LeafCard
        leaf={makeLeaf({ disk_gate: { blocked: true, reason: "r", raise_to_gb: 21 } })}
        {...defaultProps}
        onRaiseDisk={onRaiseDisk}
      />
    );

    await user.click(screen.getByText("Raise disk allowance to 21 GB"));
    await waitFor(() => {
      expect(screen.getByText("VALIDATION_ERROR: max_disk_gb too large")).toBeInTheDocument();
    });
  });

  it("shows nothing about the disk gate when the leaf is not blocked", () => {
    render(
      <LeafCard
        leaf={makeLeaf({ disk_gate: { blocked: false } })}
        {...defaultProps}
        onRaiseDisk={vi.fn()}
      />
    );
    expect(screen.queryByText(/Will not fetch/)).not.toBeInTheDocument();
  });

  // --- Failures ---

  it("shows the failure record with the pause time", () => {
    const pausedUntil = new Date("2026-08-29T14:32:00Z");
    render(
      <LeafCard
        leaf={makeLeaf({
          failures: {
            leaf_id: "leaf-1",
            leaf_name: "Prime Gap Study",
            consecutive_failures: 3,
            total_failures: 5,
            last_reason: "exit status 1",
            last_failed_at: "2026-08-29T14:22:00Z",
            paused: true,
            paused_until: pausedUntil.toISOString(),
          },
        })}
        {...defaultProps}
      />
    );

    const expectedClock = pausedUntil.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    expect(screen.getByText(/Failing on this machine/)).toHaveTextContent(
      `Failing on this machine: 3 in a row, 5 total (last: exit status 1). Paused here until ${expectedClock}`
    );
  });

  it("shows failures that have not paused the leaf without a pause time", () => {
    render(
      <LeafCard
        leaf={makeLeaf({
          failures: {
            leaf_id: "leaf-1",
            leaf_name: "Prime Gap Study",
            consecutive_failures: 1,
            total_failures: 1,
            paused: false,
          },
        })}
        {...defaultProps}
      />
    );

    expect(screen.getByText(/Failing on this machine/)).toHaveTextContent(
      "Failing on this machine: 1 in a row, 1 total"
    );
    expect(screen.queryByText(/Paused here/)).not.toBeInTheDocument();
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

  it("shows container warning when containerStatus is null", () => {
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

  // --- Results link ---

  it("renders the results link and opens the head's visualization page", async () => {
    const user = userEvent.setup();
    render(
      <LeafCard
        leaf={makeLeaf({ slug: "prime-gap-study" })}
        {...defaultProps}
        dashboardUrl="https://lettuce.science"
        volunteerId="vol-abc123"
      />
    );

    const button = screen.getByText("View results on the head's website");
    expect(button).toBeInTheDocument();
    expect(button.tagName).toBe("BUTTON");

    await user.click(button);
    expect(mockOpenUrl).toHaveBeenCalledWith(
      "https://lettuce.science/leafs/prime-gap-study/visualize?volunteer=vol-abc123"
    );
  });

  it("does not render the results link when dashboardUrl is missing", () => {
    render(
      <LeafCard
        leaf={makeLeaf()}
        {...defaultProps}
        volunteerId="vol-abc123"
      />
    );

    expect(screen.queryByText("View results on the head's website")).not.toBeInTheDocument();
  });

  it("does not render the results link when volunteerId is missing", () => {
    render(
      <LeafCard
        leaf={makeLeaf()}
        {...defaultProps}
        dashboardUrl="https://lettuce.science"
      />
    );

    expect(screen.queryByText("View results on the head's website")).not.toBeInTheDocument();
  });
});
