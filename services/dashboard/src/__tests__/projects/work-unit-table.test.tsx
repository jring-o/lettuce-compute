import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { WorkUnitTable } from "@/components/projects/work-unit-table";
import type { WorkUnitSummary, PaginatedResponse } from "@/types/infrastructure";

// --- Mocks ---

const mockListWorkUnits = jest.fn();
jest.mock("@/lib/actions/work-units", () => ({
  listWorkUnits: (...args: unknown[]) => mockListWorkUnits(...args),
}));

const mockWorkUnits: WorkUnitSummary[] = [
  {
    id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
    leaf_id: "p1",
    batch_id: "b1",
    state: "RUNNING",
    priority: "HIGH",
    assigned_to: "11111111-2222-3333-4444-555555555555",
    attempts: 2,
    flagged_for_review: false,
    created_at: "2026-03-14T10:00:00Z",
    updated_at: "2026-03-14T10:05:00Z",
  },
  {
    id: "ffffffff-1111-2222-3333-444444444444",
    leaf_id: "p1",
    batch_id: "b1",
    state: "COMPLETED",
    priority: "NORMAL",
    assigned_to: null,
    attempts: 1,
    flagged_for_review: true,
    created_at: "2026-03-14T09:00:00Z",
    updated_at: "2026-03-14T09:30:00Z",
  },
];

const mockResponse: PaginatedResponse<WorkUnitSummary> = {
  data: mockWorkUnits,
  pagination: { next_cursor: null, has_more: false },
};

beforeEach(() => {
  jest.clearAllMocks();
  mockListWorkUnits.mockResolvedValue({ data: mockResponse });
});

describe("WorkUnitTable", () => {
  it("renders work units with correct columns", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    });

    // State and priority text appears in both filter options and table badges,
    // so use getAllByText and check at least 2 exist (option + badge)
    expect(screen.getAllByText("RUNNING").length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText("HIGH").length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText("COMPLETED").length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText("NORMAL").length).toBeGreaterThanOrEqual(2);
  });

  it("truncates UUID to 8 chars", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
      expect(screen.getByText("ffffffff")).toBeInTheDocument();
    });

    // Full UUID should not appear
    expect(
      screen.queryByText("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
    ).not.toBeInTheDocument();
  });

  it("shows state and priority badges", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("RUNNING")).toBeInTheDocument();
    });

    expect(screen.getByText("HIGH")).toBeInTheDocument();
    expect(screen.getByText("COMPLETED")).toBeInTheDocument();
    expect(screen.getByText("NORMAL")).toBeInTheDocument();
  });

  it("shows truncated volunteer ID or dash", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("11111111")).toBeInTheDocument();
    });

    // The unassigned one should show a dash
    const dashes = screen.getAllByText("\u2014");
    expect(dashes.length).toBeGreaterThanOrEqual(1);
  });

  it("shows empty state when no work units", async () => {
    mockListWorkUnits.mockResolvedValue({
      data: { data: [], pagination: { next_cursor: null, has_more: false } },
    });

    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("No work units yet")).toBeInTheDocument();
    });
  });

  it("shows filter empty state message", async () => {
    // First render with empty results and a filter active
    mockListWorkUnits.mockResolvedValue({
      data: { data: [], pagination: { next_cursor: null, has_more: false } },
    });

    render(<WorkUnitTable leafId="p1" />);

    // Wait for initial load to complete
    await waitFor(() => {
      expect(screen.queryByText("No work units yet")).toBeInTheDocument();
    });

    // Change state filter — component re-fetches with filter active
    const selects = screen.getAllByRole("combobox");
    fireEvent.change(selects[0], { target: { value: "FAILED" } });

    await waitFor(() => {
      expect(
        screen.getByText("No work units match filters"),
      ).toBeInTheDocument();
    });
  });

  it("shows loading skeletons during initial fetch", () => {
    // Don't resolve the mock — keep the component in loading state
    mockListWorkUnits.mockReturnValue(new Promise(() => {}));
    render(<WorkUnitTable leafId="p1" />);

    // Should show skeleton placeholders while loading
    const skeletons = screen.getByTestId("work-unit-table").querySelectorAll("[data-slot='skeleton']");
    expect(skeletons.length).toBe(5);
  });

  it("shows Load More button when hasMore is true and loads additional data", async () => {
    const page1: PaginatedResponse<WorkUnitSummary> = {
      data: [mockWorkUnits[0]],
      pagination: { next_cursor: "cursor-page2", has_more: true },
    };
    const page2: PaginatedResponse<WorkUnitSummary> = {
      data: [mockWorkUnits[1]],
      pagination: { next_cursor: null, has_more: false },
    };

    mockListWorkUnits
      .mockResolvedValueOnce({ data: page1 })
      .mockResolvedValueOnce({ data: page2 });

    render(<WorkUnitTable leafId="p1" />);

    // Wait for first page to render
    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    });

    // Load More button should be present
    const loadMoreBtn = screen.getByRole("button", { name: /Load More/i });
    expect(loadMoreBtn).toBeInTheDocument();

    // Click Load More
    fireEvent.click(loadMoreBtn);

    // Second page data should appear
    await waitFor(() => {
      expect(screen.getByText("ffffffff")).toBeInTheDocument();
    });

    // Load More should disappear (no more pages)
    expect(screen.queryByRole("button", { name: /Load More/i })).not.toBeInTheDocument();

    // Second call should include cursor
    expect(mockListWorkUnits).toHaveBeenCalledTimes(2);
    expect(mockListWorkUnits).toHaveBeenNthCalledWith(2, "p1", expect.objectContaining({
      cursor: "cursor-page2",
    }));
  });

  it("sorts by state when clicking the State column header", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    });

    // Default sort is created_at desc — aaaaaaaa (10:00) first, ffffffff (09:00) second
    const rows = screen.getAllByRole("row");
    // rows[0] is header, rows[1] is first data row
    expect(rows[1]).toHaveTextContent("aaaaaaaa");
    expect(rows[2]).toHaveTextContent("ffffffff");

    // Click State header to sort by state asc
    const stateHeader = screen.getByText("State", { selector: "th" });
    fireEvent.click(stateHeader);

    // COMPLETED comes before RUNNING alphabetically
    const sortedRows = screen.getAllByRole("row");
    expect(sortedRows[1]).toHaveTextContent("ffffffff"); // COMPLETED
    expect(sortedRows[2]).toHaveTextContent("aaaaaaaa"); // RUNNING
  });

  it("toggles sort order when clicking the same column header twice", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    });

    // Click State header to sort asc
    const stateHeader = screen.getByText("State", { selector: "th" });
    fireEvent.click(stateHeader);

    // Click again to toggle to desc
    fireEvent.click(stateHeader);

    // RUNNING comes after COMPLETED alphabetically — desc puts RUNNING first
    const rows = screen.getAllByRole("row");
    expect(rows[1]).toHaveTextContent("aaaaaaaa"); // RUNNING
    expect(rows[2]).toHaveTextContent("ffffffff"); // COMPLETED
  });

  it("sorts by priority when clicking the Priority column header", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    });

    // Click Priority header
    const priorityHeader = screen.getByText("Priority", { selector: "th" });
    fireEvent.click(priorityHeader);

    // HIGH vs NORMAL — asc: HIGH before NORMAL
    const rows = screen.getAllByRole("row");
    expect(rows[1]).toHaveTextContent("aaaaaaaa"); // HIGH
    expect(rows[2]).toHaveTextContent("ffffffff"); // NORMAL
  });

  it("re-fetches when priority filter changes", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    });

    // Clear the call count from initial fetch
    mockListWorkUnits.mockClear();
    mockListWorkUnits.mockResolvedValue({ data: mockResponse });

    // Change priority filter
    const selects = screen.getAllByRole("combobox");
    // selects[0] is State, selects[1] is Priority
    fireEvent.change(selects[1], { target: { value: "HIGH" } });

    await waitFor(() => {
      expect(mockListWorkUnits).toHaveBeenCalledWith(
        "p1",
        expect.objectContaining({ priority: "HIGH" }),
      );
    });
  });

  it("re-fetches when flagged-only checkbox is toggled", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    });

    mockListWorkUnits.mockClear();
    mockListWorkUnits.mockResolvedValue({ data: mockResponse });

    // Toggle flagged checkbox
    const checkbox = screen.getByRole("checkbox");
    fireEvent.click(checkbox);

    await waitFor(() => {
      expect(mockListWorkUnits).toHaveBeenCalledWith(
        "p1",
        expect.objectContaining({ flagged_for_review: true }),
      );
    });
  });

  it("handles error response from listWorkUnits gracefully", async () => {
    // Return a result without "data" key (error shape)
    mockListWorkUnits.mockResolvedValue({
      error: { code: "INTERNAL", message: "Server error" },
    });

    render(<WorkUnitTable leafId="p1" />);

    // Should finish loading and show empty state (no crash)
    await waitFor(() => {
      expect(screen.getByText("No work units yet")).toBeInTheDocument();
    });
  });

  it("shows sort indicator on the active sort column", async () => {
    render(<WorkUnitTable leafId="p1" />);

    await waitFor(() => {
      expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    });

    // Default sort is created_at desc — Created header should have down arrow
    const createdHeader = screen.getByText(/Created/, { selector: "th" });
    expect(createdHeader.textContent).toContain("\u2193");

    // Click State header — State should get up arrow (asc), Created should lose indicator
    const stateHeader = screen.getByText("State", { selector: "th" });
    fireEvent.click(stateHeader);
    expect(stateHeader.textContent).toContain("\u2191");
  });
});
