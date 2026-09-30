import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RunPreviewCard } from "./run-preview-card";
import type { PreviewTask, RunPreview } from "@/api/client";

const mockQuery = vi.fn();
vi.mock("@/hooks/use-api", () => ({
  useApiQuery: () => mockQuery(),
}));

const preview: RunPreview = {
  cpu_limit: 4,
  together: [
    { leaf_id: "g", leaf_name: "GREP", head: "h", cores: 2 },
    { leaf_id: "b", leaf_name: "Beyblade", head: "h", cores: 1 },
  ],
  together_cores: 3,
  waiting_for_cores: { leaf_id: "g", leaf_name: "GREP", head: "h", cores: 2 },
  alone: [
    { leaf_id: "g", leaf_name: "GREP", head: "h", tasks: [2, 2], cores_used: 4 },
    { leaf_id: "b", leaf_name: "Beyblade", head: "h", tasks: [1, 1, 1, 1], cores_used: 4 },
    { leaf_id: "x", leaf_name: "Wide", head: "h", tasks: [], cores_used: 0, cannot_start: "needs 8 cores" },
  ],
};

function tasks(n: number, task: Omit<PreviewTask, "head">): PreviewTask[] {
  return Array.from({ length: n }, () => ({ ...task, head: "h" }));
}

// A 256-thread machine with five enabled CPU leaves and a GPU leaf that cannot
// start: 151 tasks would start together, and one leaf on its own runs 66 tasks
// of two different widths.
const big: RunPreview = {
  cpu_limit: 256,
  together: [
    ...tasks(30, { leaf_id: "native", leaf_name: "Beyblade Arena (native)", cores: 1 }),
    ...tasks(30, { leaf_id: "f13", leaf_name: "GREP f13 (CPU)", cores: 2 }),
    ...tasks(30, { leaf_id: "native", leaf_name: "Beyblade Arena (native)", cores: 1 }),
    ...tasks(30, { leaf_id: "f14", leaf_name: "GREP f14 (CPU)", cores: 2 }),
    ...tasks(15, { leaf_id: "v1", leaf_name: "GREP V1 (CPU)", cores: 4 }),
    ...tasks(16, { leaf_id: "box", leaf_name: "Beyblade Arena", cores: 1 }),
  ],
  together_cores: 256,
  alone: [
    { leaf_id: "native", leaf_name: "Beyblade Arena (native)", head: "h", tasks: Array(256).fill(1), cores_used: 256 },
    { leaf_id: "box", leaf_name: "Beyblade Arena", head: "h", tasks: Array(16).fill(1), cores_used: 16 },
    { leaf_id: "f13", leaf_name: "GREP f13 (CPU)", head: "h", tasks: Array(128).fill(2), cores_used: 256 },
    { leaf_id: "f14", leaf_name: "GREP f14 (CPU)", head: "h", tasks: Array(128).fill(2), cores_used: 256 },
    { leaf_id: "v1", leaf_name: "GREP V1 (CPU)", head: "h", tasks: [...Array(62).fill(4), ...Array(4).fill(2)], cores_used: 256 },
    { leaf_id: "gpu", leaf_name: "GREP V1 (GPU)", head: "h", tasks: [], cores_used: 0, cannot_start: "needs a GPU" },
  ],
};

describe("RunPreviewCard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
  });

  it("says it is a preview first, then what runs together, the task that waits, and each leaf alone", () => {
    mockQuery.mockReturnValue({ data: preview, isLoading: false, error: null, refetch: vi.fn() });
    render(<RunPreviewCard />);
    expect(screen.getByTestId("run-preview-summary").textContent).toMatch(
      /^If your queue held work of every enabled leaf: 2 tasks would run at once, using 3 of your 4 allowed cores\./
    );
    expect(screen.getByText(/The next GREP task needs 2 cores and waits for them/)).toBeInTheDocument();
    const chips = screen.getAllByTestId("run-preview-task").map((el) => el.textContent);
    expect(chips).toEqual(["GREP × 1 · 2 cores", "Beyblade × 1 · 1 core"]);
    expect(screen.getByText("GREP on its own: 2 at once, 2 cores each")).toBeInTheDocument();
    expect(screen.getByText("Beyblade on its own: 4 at once, 1 core each")).toBeInTheDocument();
    expect(screen.getByText("Wide on its own: cannot start here")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /details/ })).not.toBeInTheDocument();
  });

  // The field report: ~130 chips and 66-number lines on 256 threads, headed by a
  // bare "256 of 256 cores" read as load while nothing ran.
  it("folds a large machine's preview to one line, and groups it by leaf and cores when opened", async () => {
    const user = userEvent.setup();
    mockQuery.mockReturnValue({ data: big, isLoading: false, error: null, refetch: vi.fn() });
    render(<RunPreviewCard page="projects" />);

    expect(screen.getByTestId("run-preview-summary").textContent).toBe(
      "If your queue held work of every enabled leaf: 151 tasks would run at once, using 256 of your 256 allowed cores."
    );
    expect(screen.queryAllByTestId("run-preview-task")).toHaveLength(0);
    expect(screen.queryByText(/on its own/)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Show details" }));
    const chips = screen.getAllByTestId("run-preview-task").map((el) => el.textContent);
    expect(chips).toEqual([
      "Beyblade Arena (native) × 60 · 1 core each",
      "GREP f13 (CPU) × 30 · 2 cores each",
      "GREP f14 (CPU) × 30 · 2 cores each",
      "GREP V1 (CPU) × 15 · 4 cores each",
      "Beyblade Arena × 16 · 1 core each",
    ]);
    expect(screen.getByText("GREP V1 (CPU) on its own: 66 at once: 62 × 4 cores, 4 × 2 cores")).toBeInTheDocument();
    expect(screen.getByText("Beyblade Arena (native) on its own: 256 at once, 1 core each")).toBeInTheDocument();
    expect(screen.getByText("GREP V1 (GPU) on its own: cannot start here")).toBeInTheDocument();
  });

  it("keeps the opened state per page", async () => {
    const user = userEvent.setup();
    mockQuery.mockReturnValue({ data: big, isLoading: false, error: null, refetch: vi.fn() });
    const { unmount } = render(<RunPreviewCard page="projects" />);
    await user.click(screen.getByRole("button", { name: "Show details" }));
    unmount();

    const { unmount: unmountAgain } = render(<RunPreviewCard page="projects" />);
    expect(screen.getByRole("button", { name: "Hide details" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getAllByTestId("run-preview-task")).toHaveLength(5);
    unmountAgain();

    render(<RunPreviewCard page="settings" />);
    expect(screen.getByRole("button", { name: "Show details" })).toHaveAttribute("aria-expanded", "false");
  });

  it("asks again when a setting is saved", () => {
    const refetch = vi.fn();
    mockQuery.mockReturnValue({ data: preview, isLoading: false, error: null, refetch });
    const { rerender } = render(<RunPreviewCard refreshKey={0} />);
    refetch.mockClear();
    rerender(<RunPreviewCard refreshKey={1} />);
    expect(refetch).toHaveBeenCalledTimes(1);
  });

  it("shows nothing for a daemon that does not offer the preview", () => {
    mockQuery.mockReturnValue({ data: null, isLoading: false, error: new Error("404"), refetch: vi.fn() });
    const { container } = render(<RunPreviewCard />);
    expect(container.firstChild).toBeNull();
  });
});
