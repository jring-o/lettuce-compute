import { render, screen, waitFor } from "@testing-library/react";
import { LeafSelector } from "@/components/contribute/leaf-selector";

// Polyfill PointerEvent for jsdom (required by @base-ui/react Switch)
if (typeof globalThis.PointerEvent === "undefined") {
  (globalThis as Record<string, unknown>).PointerEvent = class PointerEvent extends MouseEvent {
    readonly pointerId: number;
    constructor(type: string, params: PointerEventInit = {}) {
      super(type, params);
      this.pointerId = params.pointerId ?? 0;
    }
  };
}

// Mock fetch globally
const mockFetch = jest.fn();
global.fetch = mockFetch;

const headResponse = {
  name: "Test Head",
  leafs: [
    {
      id: "leaf-1",
      name: "Prime Gaps",
      description: "Searching for prime gaps",
      state: "ACTIVE",
      queued_work_units: 42,
      execution_spec: { binaries: { wasm: "prime-gaps.wasm" } },
    },
    {
      id: "leaf-2",
      name: "Protein Fold",
      description: "GPU-accelerated protein folding",
      state: "ACTIVE",
      queued_work_units: 10,
      execution_spec: {
        binaries: { wasm: "protein.wasm" },
        gpu_required: true,
      },
    },
    {
      id: "leaf-3",
      name: "Inactive Leaf",
      description: "Not active",
      state: "PAUSED",
      queued_work_units: 5,
      execution_spec: { binaries: { wasm: "inactive.wasm" } },
    },
    {
      id: "leaf-4",
      name: "No WASM",
      description: "Native only",
      state: "ACTIVE",
      queued_work_units: 20,
      execution_spec: { binaries: { linux_amd64: "native" } },
    },
    {
      id: "leaf-5",
      name: "No Work",
      description: "Empty queue",
      state: "ACTIVE",
      queued_work_units: 0,
      execution_spec: { binaries: { wasm: "empty.wasm" } },
    },
  ],
};

describe("LeafSelector", () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it("shows loading state initially", () => {
    mockFetch.mockReturnValue(new Promise(() => {}));
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );
    expect(screen.getByText("Loading leafs...")).toBeInTheDocument();
  });

  it("fetches leafs from the correct URL", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(headResponse),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );
    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/head"
      );
    });
  });

  it("renders only active WASM leafs with queued work units", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(headResponse),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(screen.getByText("Prime Gaps")).toBeInTheDocument();
    });
    expect(screen.getByText("Protein Fold")).toBeInTheDocument();
    // These should NOT appear (PAUSED, no wasm, no queued WUs)
    expect(screen.queryByText("Inactive Leaf")).not.toBeInTheDocument();
    expect(screen.queryByText("No WASM")).not.toBeInTheDocument();
    expect(screen.queryByText("No Work")).not.toBeInTheDocument();
  });

  it("shows queued work unit count for each leaf", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(headResponse),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(screen.getByText("42 queued work units")).toBeInTheDocument();
    });
    expect(screen.getByText("10 queued work units")).toBeInTheDocument();
  });

  it("shows GPU badge for GPU-required leafs", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(headResponse),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(screen.getByText("GPU")).toBeInTheDocument();
    });
  });

  it("shows empty state when no WASM leafs are active", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () =>
        Promise.resolve({
          name: "Empty Head",
          leafs: [],
        }),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(
        screen.getByText(
          "No WASM leafs are currently active on this server."
        )
      ).toBeInTheDocument();
    });
  });

  it("shows error state when fetch fails", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(
        screen.getByText("Failed to fetch head info: 500")
      ).toBeInTheDocument();
    });
  });

  it("shows error state when fetch throws", async () => {
    mockFetch.mockRejectedValue(new Error("Network error"));
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(screen.getByText("Network error")).toBeInTheDocument();
    });
  });

  it("renders switch toggles for each leaf", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(headResponse),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(screen.getByText("Prime Gaps")).toBeInTheDocument();
    });

    expect(screen.getByLabelText("Enable Prime Gaps")).toBeInTheDocument();
    expect(screen.getByLabelText("Enable Protein Fold")).toBeInTheDocument();
  });

  it("reflects selected state on switch toggles", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(headResponse),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set(["leaf-1"])}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(screen.getByText("Prime Gaps")).toBeInTheDocument();
    });

    const toggle = screen.getByLabelText("Enable Prime Gaps");
    expect(toggle).toHaveAttribute("aria-checked", "true");
  });

  it("reflects unselected state on switch toggles", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(headResponse),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
      />
    );

    await waitFor(() => {
      expect(screen.getByText("Prime Gaps")).toBeInTheDocument();
    });

    const toggle = screen.getByLabelText("Enable Prime Gaps");
    expect(toggle).toHaveAttribute("aria-checked", "false");
  });

  it("marks toggles as aria-disabled when disabled prop is true", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      json: () => Promise.resolve(headResponse),
    });
    render(
      <LeafSelector
        serverUrl="http://localhost:8080"
        selectedIds={new Set()}
        onSelectionChange={jest.fn()}
        disabled={true}
      />
    );

    await waitFor(() => {
      expect(screen.getByText("Prime Gaps")).toBeInTheDocument();
    });

    const toggle = screen.getByLabelText("Enable Prime Gaps");
    expect(toggle).toHaveAttribute("aria-disabled", "true");
  });
});
