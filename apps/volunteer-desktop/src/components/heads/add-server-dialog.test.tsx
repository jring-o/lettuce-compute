import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { invoke } from "@tauri-apps/api/core";
import { AddServerDialog } from "./add-server-dialog";
import type { MachineCapabilities } from "@/api/client";

const mockClient = {
  attachHead: vi.fn(),
};

vi.mock("@/hooks/use-api", () => ({
  useClient: () => ({ client: mockClient, error: null }),
}));

const mockInvoke = vi.mocked(invoke);

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

/** Route the two host commands the dialog uses; everything else resolves to undefined. */
function mockHost(opts: {
  health?: unknown | Error;
  head?: unknown | Error;
}) {
  mockInvoke.mockImplementation(async (cmd: string) => {
    if (cmd === "test_server_connection") {
      if (opts.health instanceof Error) throw opts.health.message;
      return opts.health ?? { status: "healthy" };
    }
    if (cmd === "fetch_head_info") {
      if (opts.head instanceof Error) throw opts.head.message;
      return opts.head;
    }
    return undefined;
  });
}

/** The preview's title line (the name also appears in the trust consent wording). */
const previewTitle = (name: string) => screen.getByText(name, { selector: "p" });

const defaultProps = {
  onOpenChange: vi.fn(),
  onServerAdded: vi.fn(),
  machine: makeMachine(),
};

describe("AddServerDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockInvoke.mockReset();
    mockInvoke.mockResolvedValue(undefined);
    mockClient.attachHead.mockResolvedValue(undefined);
  });

  it("renders URL input and test button when open", () => {
    render(<AddServerDialog open={true} {...defaultProps} />);

    expect(
      screen.getByPlaceholderText("https://compute.example.org")
    ).toBeInTheDocument();
    expect(screen.getByText("Test Connection")).toBeInTheDocument();
  });

  it("does not render when closed", () => {
    render(<AddServerDialog open={false} {...defaultProps} />);

    expect(
      screen.queryByPlaceholderText("https://compute.example.org")
    ).not.toBeInTheDocument();
  });

  it("test connection button is disabled when URL is empty", () => {
    render(<AddServerDialog open={true} {...defaultProps} />);

    const testBtn = screen.getByText("Test Connection");
    expect(testBtn).toBeDisabled();
  });

  it("closes on Escape key", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();

    render(<AddServerDialog open={true} {...defaultProps} onOpenChange={onOpenChange} />);

    await user.keyboard("{Escape}");
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("closes on Cancel button click", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();

    render(<AddServerDialog open={true} {...defaultProps} onOpenChange={onOpenChange} />);

    await user.click(screen.getByText("Cancel"));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("test connection button is enabled after typing URL", async () => {
    const user = userEvent.setup();

    render(<AddServerDialog open={true} {...defaultProps} />);

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://example.com");

    const testBtn = screen.getByText("Test Connection");
    expect(testBtn).not.toBeDisabled();
  });

  it("shows head preview after successful test connection", async () => {
    const user = userEvent.setup();
    mockHost({
      head: {
        name: "Test Server",
        description: "A test server",
        leafs: [
          { slug: "leaf-a", name: "Leaf A", research_area: ["science", "physics"], state: "ACTIVE" },
          { slug: "leaf-b", name: "Leaf B", research_area: "biology", state: "ACTIVE" },
          { slug: "leaf-c", name: "Leaf C", research_area: "draft", state: "DRAFT" },
        ],
      },
    });

    render(<AddServerDialog open={true} {...defaultProps} />);

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://example.com");
    await user.click(screen.getByText("Test Connection"));

    await waitFor(() => {
      expect(previewTitle("Test Server")).toBeInTheDocument();
    });
    expect(mockInvoke).toHaveBeenCalledWith("test_server_connection", { url: "https://example.com" });
    expect(mockInvoke).toHaveBeenCalledWith("fetch_head_info", { url: "https://example.com" });
    expect(screen.getByText("A test server")).toBeInTheDocument();
    expect(screen.getByText("Leaf A")).toBeInTheDocument();
    expect(screen.getByText("science, physics")).toBeInTheDocument();
    expect(screen.getByText("biology")).toBeInTheDocument();
    // Only active leafs are previewed.
    expect(screen.queryByText("Leaf C")).not.toBeInTheDocument();
  });

  it("still offers Attach when the head answers the health check but not head info", async () => {
    const user = userEvent.setup();
    mockHost({ head: new Error("Server returned 404") });

    render(<AddServerDialog open={true} {...defaultProps} />);

    await user.type(screen.getByPlaceholderText("https://compute.example.org"), "https://old.example.com");
    await user.click(screen.getByText("Test Connection"));

    await waitFor(() => {
      expect(screen.getByText("Attach")).toBeInTheDocument();
    });
    expect(previewTitle("https://old.example.com")).toBeInTheDocument();
  });

  it("shows test error on failed connection", async () => {
    const user = userEvent.setup();
    mockHost({ health: new Error("Connection failed: refused") });

    render(<AddServerDialog open={true} {...defaultProps} />);

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://bad.example.com");
    await user.click(screen.getByText("Test Connection"));

    await waitFor(() => {
      expect(
        screen.getByText("Connection failed. Check the URL and try again.")
      ).toBeInTheDocument();
    });
    expect(screen.queryByText("Attach")).not.toBeInTheDocument();
  });

  it("attaches with container trusted by default and native off, then closes", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();
    const onServerAdded = vi.fn();
    mockHost({
      head: {
        name: "My Server",
        description: "Desc",
        leafs: [{ slug: "l1", name: "Leaf 1", research_area: "bio", state: "ACTIVE" }],
      },
    });

    render(
      <AddServerDialog
        open={true}
        {...defaultProps}
        onOpenChange={onOpenChange}
        onServerAdded={onServerAdded}
      />
    );

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://my-server.com");
    await user.click(screen.getByText("Test Connection"));

    await waitFor(() => {
      expect(previewTitle("My Server")).toBeInTheDocument();
    });

    const container = screen.getByLabelText("Allow container tasks from this head");
    const native = screen.getByLabelText("Allow native tasks from this head");
    expect(container).toBeChecked();
    expect(container).not.toBeDisabled();
    expect(native).not.toBeChecked();
    expect(screen.getByText(/no sandbox/)).toBeInTheDocument();

    await user.click(screen.getByText("Attach"));

    await waitFor(() => {
      expect(mockClient.attachHead).toHaveBeenCalledWith({
        server_address: "https://my-server.com",
        name: undefined,
        trusted_runtimes: ["CONTAINER"],
      });
    });

    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onServerAdded).toHaveBeenCalledWith("My Server");
  });

  it("sends the trust the user chose and the custom display name", async () => {
    const user = userEvent.setup();
    const onServerAdded = vi.fn();
    mockHost({ head: { name: "My Server", description: "", leafs: [] } });

    render(<AddServerDialog open={true} {...defaultProps} onServerAdded={onServerAdded} />);

    await user.type(screen.getByPlaceholderText("https://compute.example.org"), "https://my-server.com");
    await user.click(screen.getByText("Test Connection"));
    await waitFor(() => expect(screen.getByText("Attach")).toBeInTheDocument());

    await user.click(screen.getByLabelText("Allow container tasks from this head"));
    await user.click(screen.getByLabelText("Allow native tasks from this head"));
    await user.type(screen.getByPlaceholderText("Display name (optional)"), "Lab");
    await user.click(screen.getByText("Attach"));

    await waitFor(() => {
      expect(mockClient.attachHead).toHaveBeenCalledWith({
        server_address: "https://my-server.com",
        name: "Lab",
        trusted_runtimes: ["NATIVE"],
      });
    });
    expect(onServerAdded).toHaveBeenCalledWith("Lab");
  });

  it("cannot offer container trust when this machine has no container backend", async () => {
    const user = userEvent.setup();
    mockHost({ head: { name: "My Server", description: "", leafs: [] } });

    render(
      <AddServerDialog
        open={true}
        {...defaultProps}
        machine={makeMachine({ runtimes: ["wasm"] })}
      />
    );

    await user.type(screen.getByPlaceholderText("https://compute.example.org"), "https://my-server.com");
    await user.click(screen.getByText("Test Connection"));
    await waitFor(() => expect(screen.getByText("Attach")).toBeInTheDocument());

    const container = screen.getByLabelText("Allow container tasks from this head");
    expect(container).toBeDisabled();
    expect(container).not.toBeChecked();
    expect(screen.getByText(/No Docker or Podman backend was detected/)).toBeInTheDocument();

    await user.click(screen.getByText("Attach"));
    await waitFor(() => {
      expect(mockClient.attachHead).toHaveBeenCalledWith(
        expect.objectContaining({ trusted_runtimes: [] })
      );
    });
  });

  it("shows attach error when attachHead fails", async () => {
    const user = userEvent.setup();

    mockClient.attachHead.mockRejectedValue(
      new (await import("@/api/client")).ApiError("ALREADY_ATTACHED", "Already attached")
    );
    mockHost({ head: { name: "Dup Server", description: "", leafs: [] } });

    render(<AddServerDialog open={true} {...defaultProps} />);

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://dup.example.com");
    await user.click(screen.getByText("Test Connection"));

    await waitFor(() => {
      expect(previewTitle("Dup Server")).toBeInTheDocument();
    });

    await user.click(screen.getByText("Attach"));

    await waitFor(() => {
      expect(screen.getByText("Already attached")).toBeInTheDocument();
    });
    expect(defaultProps.onServerAdded).not.toHaveBeenCalled();
  });

  it("closes when clicking backdrop overlay", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();

    render(<AddServerDialog open={true} {...defaultProps} onOpenChange={onOpenChange} />);

    // The backdrop is the fixed overlay div; click it directly
    const backdrop = document.querySelector(".fixed.inset-0");
    expect(backdrop).toBeTruthy();

    // Click the backdrop (not the dialog content)
    await user.click(backdrop!);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
