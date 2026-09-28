import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { RunPreviewCard } from "./run-preview-card";
import type { RunPreview } from "@/api/client";

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

describe("RunPreviewCard", () => {
  beforeEach(() => vi.clearAllMocks());

  it("shows what runs together, the cores used, the task that waits, and each leaf alone", () => {
    mockQuery.mockReturnValue({ data: preview, isLoading: false, error: null, refetch: vi.fn() });
    render(<RunPreviewCard />);
    const tasks = screen.getAllByTestId("run-preview-task").map((el) => el.textContent);
    expect(tasks).toEqual(["GREP · 2 cores", "Beyblade · 1 core"]);
    expect(screen.getByText(/3 of 4 cores\./)).toBeInTheDocument();
    expect(screen.getByText(/The next GREP task needs 2 cores and waits for them/)).toBeInTheDocument();
    expect(screen.getByText("GREP on its own: 2 at once, 2 cores each")).toBeInTheDocument();
    expect(screen.getByText("Beyblade on its own: 4 at once, 1 core each")).toBeInTheDocument();
    expect(screen.getByText("Wide on its own: cannot start here")).toBeInTheDocument();
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
