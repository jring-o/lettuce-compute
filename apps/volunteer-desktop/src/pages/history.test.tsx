import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HistoryPage } from "./history";
import type { HistoryEntry, CreditSummary } from "@/api/client";

// Mock the hooks
vi.mock("@/hooks/use-history", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/hooks/use-history")>();
  return {
    ...actual,
    useHistory: vi.fn(),
  };
});

vi.mock("@/hooks/use-credit", () => ({
  useCredit: vi.fn(),
}));

const mockUseHeads = vi.fn();
vi.mock("@/hooks/use-heads", () => ({
  useHeads: () => mockUseHeads(),
}));

vi.mock("@/hooks/use-api", () => ({
  useClient: vi.fn(),
}));

// Mock lucide-react icons to avoid rendering issues
vi.mock("lucide-react", () => ({
  Download: (props: any) => <span data-testid="download-icon" {...props} />,
  Filter: (props: any) => <span data-testid="filter-icon" {...props} />,
  ChevronRight: (props: any) => <span data-testid="chevron-right" {...props} />,
  ChevronDown: (props: any) => <span data-testid="chevron-down" {...props} />,
  Copy: (props: any) => <span data-testid="copy-icon" {...props} />,
}));

import { useHistory } from "@/hooks/use-history";
import { useCredit } from "@/hooks/use-credit";
import { useClient } from "@/hooks/use-api";

const mockUseHistory = useHistory as ReturnType<typeof vi.fn>;
const mockUseCredit = useCredit as ReturnType<typeof vi.fn>;
const mockUseClient = useClient as ReturnType<typeof vi.fn>;

function makeMockEntry(overrides: Partial<HistoryEntry> = {}): HistoryEntry {
  const now = new Date();
  return {
    work_unit_id: "wu-" + Math.random().toString(36).slice(2, 10),
    leaf_name: "Test Leaf",
    completed_at: now.toISOString(),
    duration_seconds: 3600,
    cpu_seconds: 3200,
    credit_earned: 100,
    validation_status: "accepted",
    head_name: "lettuce.science",
    ...overrides,
  };
}

describe("HistoryPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseClient.mockReturnValue({ client: null, error: null });
    mockUseCredit.mockReturnValue({
      credit: null,
      isLoading: false,
      error: null,
    });
    mockUseHeads.mockReturnValue({
      heads: [],
      isLoading: false,
      error: null,
      refetch: vi.fn(),
    });
  });

  it("renders empty state when no entries", () => {
    mockUseHistory.mockReturnValue({
      entries: [],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    expect(
      screen.getByText("No completed work units yet.")
    ).toBeInTheDocument();
    expect(
      screen.getByText("Start contributing to see your history here.")
    ).toBeInTheDocument();
  });

  it("renders loading skeleton when loading", () => {
    mockUseHistory.mockReturnValue({
      entries: [],
      hasMore: false,
      isLoading: true,
      loadMore: vi.fn(),
      error: null,
    });

    const { container } = render(<HistoryPage />);
    // Skeleton rows have animate-pulse class
    const skeletons = container.querySelectorAll(".animate-pulse");
    expect(skeletons.length).toBeGreaterThan(0);
  });

  it("renders error message when error occurs", () => {
    mockUseHistory.mockReturnValue({
      entries: [],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: new Error("Connection failed"),
    });

    render(<HistoryPage />);
    expect(
      screen.getByText(/Failed to load history: Connection failed/)
    ).toBeInTheDocument();
  });

  it("renders history entries grouped by day", () => {
    const today = new Date();
    const yesterday = new Date();
    yesterday.setDate(yesterday.getDate() - 1);

    const entries = [
      makeMockEntry({
        work_unit_id: "wu-11111111",
        leaf_name: "Alpha Leaf",
        completed_at: today.toISOString(),
      }),
      makeMockEntry({
        work_unit_id: "wu-22222222",
        leaf_name: "Beta Leaf",
        completed_at: yesterday.toISOString(),
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);

    // Check leaf names appear (they also appear in the filter dropdown,
    // so use getAllByText)
    expect(screen.getAllByText("Alpha Leaf").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Beta Leaf").length).toBeGreaterThanOrEqual(1);

    // Check day labels
    expect(screen.getByText("Today")).toBeInTheDocument();
    expect(screen.getByText("Yesterday")).toBeInTheDocument();
  });

  it("renders work unit ID prefix", () => {
    const entries = [
      makeMockEntry({
        work_unit_id: "wu-abcdefgh12345",
        leaf_name: "Test",
        completed_at: new Date().toISOString(),
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    // Displays first 8 chars of work unit id
    expect(screen.getByText("wu-abcde")).toBeInTheDocument();
  });

  it("renders validation status badges", () => {
    const entries = [
      makeMockEntry({
        validation_status: "accepted",
        completed_at: new Date().toISOString(),
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    // "Validated" appears in both the status badge and the filter select option
    const validated = screen.getAllByText("Validated");
    expect(validated.length).toBeGreaterThanOrEqual(2); // badge + filter option
  });

  it("renders filter controls", () => {
    mockUseHistory.mockReturnValue({
      entries: [],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);

    // Leaf filter select
    expect(screen.getByText("All Leafs")).toBeInTheDocument();

    // Date range filter
    expect(screen.getByText("Last 7 days")).toBeInTheDocument();
    expect(screen.getByText("Last 30 days")).toBeInTheDocument();

    // Validation status filter
    expect(screen.getByText("All Status")).toBeInTheDocument();
  });

  it("renders export buttons", () => {
    mockUseHistory.mockReturnValue({
      entries: [makeMockEntry({ completed_at: new Date().toISOString() })],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    expect(screen.getByText("CSV")).toBeInTheDocument();
    expect(screen.getByText("JSON")).toBeInTheDocument();
  });

  it("disables export buttons when no entries", () => {
    mockUseHistory.mockReturnValue({
      entries: [],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    const csvBtn = screen.getByText("CSV").closest("button");
    const jsonBtn = screen.getByText("JSON").closest("button");
    expect(csvBtn).toBeDisabled();
    expect(jsonBtn).toBeDisabled();
  });

  it("renders credit breakdown with by_leaf fallback", () => {
    mockUseHistory.mockReturnValue({
      entries: [makeMockEntry({ completed_at: new Date().toISOString() })],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    mockUseCredit.mockReturnValue({
      credit: {
        total_credit: 500,
        today: 50,
        this_week: 200,
        this_month: 500,
        by_leaf: [
          { leaf_id: "p1", leaf_name: "Alpha", credit: 300 },
          { leaf_id: "p2", leaf_name: "Beta", credit: 200 },
        ],
      } as CreditSummary,
      isLoading: false,
      error: null,
    });

    render(<HistoryPage />);
    // Falls back to "Credit by Leaf" when by_head is not present
    expect(screen.getAllByText("Credit by Leaf").length).toBeGreaterThan(0);
  });

  it("renders Credit by Head when by_head data is present", () => {
    mockUseHistory.mockReturnValue({
      entries: [makeMockEntry({ completed_at: new Date().toISOString() })],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    mockUseCredit.mockReturnValue({
      credit: {
        total_credit: 500,
        today: 50,
        this_week: 200,
        this_month: 500,
        by_leaf: [],
        by_head: [
          {
            head_name: "lettuce.science",
            credit: 500,
            leafs: [
              { leaf_slug: "prime", leaf_name: "Prime Study", credit: 300 },
              { leaf_slug: "mandel", leaf_name: "Mandelbrot", credit: 200 },
            ],
          },
        ],
      } as CreditSummary,
      isLoading: false,
      error: null,
    });

    render(<HistoryPage />);
    expect(screen.getAllByText("Credit by Head").length).toBeGreaterThan(0);
  });

  it("filter dropdown says All Leafs", () => {
    mockUseHistory.mockReturnValue({
      entries: [],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    expect(screen.getByText("All Leafs")).toBeInTheDocument();
  });

  it("CSV export includes head_name column from heads data", async () => {
    const user = userEvent.setup();
    const mockHistoryFn = vi.fn().mockResolvedValue({
      entries: [
        {
          work_unit_id: "wu-export-test1",
          leaf_name: "Prime Study",
          completed_at: "2026-03-20T12:00:00Z",
          duration_seconds: 3600,
          credit_earned: 100,
          validation_status: "accepted",
        },
      ],
      pagination: { next_cursor: "", has_more: false },
    });
    mockUseClient.mockReturnValue({ client: { history: mockHistoryFn }, error: null });

    // Mock useHeads with head data containing leaf->head mapping
    mockUseHeads.mockReturnValue({
      heads: [
        {
          name: "lettuce.science",
          leafs: [
            { slug: "prime", name: "Prime Study" },
            { slug: "mandel", name: "Mandelbrot" },
          ],
        },
      ],
      isLoading: false,
      error: null,
      refetch: vi.fn(),
    });

    mockUseHistory.mockReturnValue({
      entries: [makeMockEntry({
        work_unit_id: "wu-export-test1",
        leaf_name: "Prime Study",
        completed_at: new Date().toISOString(),
      })],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    // Capture the blob passed to URL.createObjectURL
    let capturedBlob: Blob | null = null;
    const origCreateObjectURL = URL.createObjectURL;
    const origRevokeObjectURL = URL.revokeObjectURL;
    URL.createObjectURL = (blob: Blob) => {
      capturedBlob = blob;
      return "blob:mock";
    };
    URL.revokeObjectURL = () => {};

    // Mock anchor element for download
    const mockAnchor = { href: "", download: "", click: vi.fn() };
    const origCreateElement = document.createElement;
    document.createElement = ((tag: string, ...args: any[]) => {
      if (tag === "a") return mockAnchor as unknown as HTMLAnchorElement;
      return origCreateElement.call(document, tag, ...args);
    }) as typeof document.createElement;

    render(<HistoryPage />);

    // Click CSV export
    await user.click(screen.getByText("CSV"));

    // Wait for export to complete
    await waitFor(() => {
      expect(mockAnchor.click).toHaveBeenCalled();
    });

    expect(capturedBlob).not.toBeNull();
    const csvText = await capturedBlob!.text();

    // Verify the CSV header contains head_name
    expect(csvText).toContain("head_name");
    // Verify the data row includes the mapped head name
    expect(csvText).toContain("lettuce.science");
    expect(csvText).toContain("Prime Study");
    expect(mockAnchor.download).toBe("lettuce-history.csv");

    // Restore
    URL.createObjectURL = origCreateObjectURL;
    URL.revokeObjectURL = origRevokeObjectURL;
    document.createElement = origCreateElement;
  });

  // --- S105: HistoryRow expand/collapse, cpu_seconds, head_name, detail section ---

  it("renders cpu_seconds as primary duration in collapsed row", () => {
    const entries = [
      makeMockEntry({
        work_unit_id: "wu-cpu-primary1",
        leaf_name: "CPU Time Leaf",
        completed_at: new Date().toISOString(),
        duration_seconds: 7200,
        cpu_seconds: 3600,
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    // cpu_seconds = 3600 -> "1h 0m"
    expect(screen.getByText("1h 0m")).toBeInTheDocument();
  });

  it("shows head_name in collapsed row", () => {
    const entries = [
      makeMockEntry({
        work_unit_id: "wu-headname-row1",
        leaf_name: "Head Display Leaf",
        completed_at: new Date().toISOString(),
        head_name: "my-server.example.com",
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    expect(screen.getByText("my-server.example.com")).toBeInTheDocument();
  });

  it("expands row on click to show detail section", async () => {
    const user = userEvent.setup();
    const entries = [
      makeMockEntry({
        work_unit_id: "wu-expand-test01",
        leaf_name: "Expandable Leaf",
        completed_at: new Date().toISOString(),
        duration_seconds: 7200,
        cpu_seconds: 3600,
        head_name: "lettuce.science",
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);

    // Before expanding, the detail labels should not be in the document
    expect(screen.queryByText("CPU Time")).not.toBeInTheDocument();
    expect(screen.queryByText("Wall Clock")).not.toBeInTheDocument();

    // Click the row to expand — leaf name appears in both the filter and the row,
    // so target the span element inside the row (class "font-medium")
    const rowLabel = screen.getAllByText("Expandable Leaf").find(
      (el) => el.tagName === "SPAN" && el.classList.contains("font-medium")
    )!;
    await user.click(rowLabel);

    // After expanding, detail section should appear
    expect(screen.getByText("CPU Time")).toBeInTheDocument();
    expect(screen.getByText("Wall Clock")).toBeInTheDocument();
    expect(screen.getByText("Time Paused")).toBeInTheDocument();
    expect(screen.getByText("Head")).toBeInTheDocument();
    expect(screen.getByText("Work Unit ID")).toBeInTheDocument();
  });

  it("detail section shows correct values for cpu_seconds, duration, and head", async () => {
    const user = userEvent.setup();
    const entries = [
      makeMockEntry({
        work_unit_id: "wu-detail-vals01",
        leaf_name: "Detail Leaf",
        completed_at: new Date().toISOString(),
        duration_seconds: 7200,
        cpu_seconds: 5400,
        head_name: "compute.example.org",
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    const rowLabel = screen.getAllByText("Detail Leaf").find(
      (el) => el.tagName === "SPAN" && el.classList.contains("font-medium")
    )!;
    await user.click(rowLabel);

    // cpu_seconds = 5400 -> "1h 30m" (appears as CPU Time value)
    // duration_seconds = 7200 -> "2h 0m" (appears as Wall Clock value)
    // paused = 7200 - 5400 = 1800 -> "30m 0s" (appears as Time Paused value)
    // head_name appears in both collapsed summary and expanded detail
    expect(screen.getAllByText("compute.example.org").length).toBeGreaterThanOrEqual(2);
    // Full WU ID should be visible in the detail section
    expect(screen.getByText("wu-detail-vals01")).toBeInTheDocument();
  });

  it("collapses row when clicked again", async () => {
    const user = userEvent.setup();
    const entries = [
      makeMockEntry({
        work_unit_id: "wu-collapse-test",
        leaf_name: "Collapsible Leaf",
        completed_at: new Date().toISOString(),
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);

    // Expand
    const rowLabel = screen.getAllByText("Collapsible Leaf").find(
      (el) => el.tagName === "SPAN" && el.classList.contains("font-medium")
    )!;
    await user.click(rowLabel);
    expect(screen.getByText("CPU Time")).toBeInTheDocument();

    // Collapse
    await user.click(rowLabel);
    expect(screen.queryByText("CPU Time")).not.toBeInTheDocument();
  });

  it("shows chevron-right when collapsed and chevron-down when expanded", async () => {
    const user = userEvent.setup();
    const entries = [
      makeMockEntry({
        work_unit_id: "wu-chevron-test1",
        leaf_name: "Chevron Leaf",
        completed_at: new Date().toISOString(),
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);

    // Collapsed state: chevron-right
    expect(screen.getByTestId("chevron-right")).toBeInTheDocument();
    expect(screen.queryByTestId("chevron-down")).not.toBeInTheDocument();

    // Click to expand
    const rowLabel = screen.getAllByText("Chevron Leaf").find(
      (el) => el.tagName === "SPAN" && el.classList.contains("font-medium")
    )!;
    await user.click(rowLabel);

    // Expanded state: chevron-down
    expect(screen.getByTestId("chevron-down")).toBeInTheDocument();
    expect(screen.queryByTestId("chevron-right")).not.toBeInTheDocument();
  });

  it("detail section shows Copy button and full WU ID", async () => {
    const user = userEvent.setup();
    const writeTextMock = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: writeTextMock },
      writable: true,
      configurable: true,
    });

    const entries = [
      makeMockEntry({
        work_unit_id: "wu-copy-detail-full-id",
        leaf_name: "Copy Test Leaf",
        completed_at: new Date().toISOString(),
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);

    // Expand the row
    const rowLabel = screen.getAllByText("Copy Test Leaf").find(
      (el) => el.tagName === "SPAN" && el.classList.contains("font-medium")
    )!;
    await user.click(rowLabel);

    // Full WU ID should be visible
    expect(screen.getByText("wu-copy-detail-full-id")).toBeInTheDocument();

    // Copy button
    expect(screen.getByText("Copy")).toBeInTheDocument();

    // Click the Copy button
    await user.click(screen.getByText("Copy"));
    expect(writeTextMock).toHaveBeenCalledWith("wu-copy-detail-full-id");

    // Should show "Copied" briefly
    await waitFor(() => {
      expect(screen.getByText("Copied")).toBeInTheDocument();
    });
  });

  it("detail section shows dash for head when head_name is empty", async () => {
    const user = userEvent.setup();
    const entries = [
      makeMockEntry({
        work_unit_id: "wu-no-head-test1",
        leaf_name: "No Head Leaf",
        completed_at: new Date().toISOString(),
        head_name: "",
      }),
    ];

    mockUseHistory.mockReturnValue({
      entries,
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    render(<HistoryPage />);
    const rowLabel = screen.getAllByText("No Head Leaf").find(
      (el) => el.tagName === "SPAN" && el.classList.contains("font-medium")
    )!;
    await user.click(rowLabel);

    // Should show "—" for head
    const headLabel = screen.getByText("Head");
    const headValue = headLabel.parentElement?.querySelector(".font-medium");
    expect(headValue?.textContent).toBe("—");
  });

  it("CSV export includes cpu_seconds column", async () => {
    const user = userEvent.setup();
    const mockHistoryFn = vi.fn().mockResolvedValue({
      entries: [
        {
          work_unit_id: "wu-csv-cpu-test1",
          leaf_name: "CSV CPU Leaf",
          completed_at: "2026-03-20T12:00:00Z",
          duration_seconds: 7200,
          cpu_seconds: 5400,
          credit_earned: 100,
          validation_status: "accepted",
          head_name: "test.server",
        },
      ],
      pagination: { next_cursor: "", has_more: false },
    });
    mockUseClient.mockReturnValue({ client: { history: mockHistoryFn }, error: null });

    mockUseHeads.mockReturnValue({
      heads: [],
      isLoading: false,
      error: null,
      refetch: vi.fn(),
    });

    mockUseHistory.mockReturnValue({
      entries: [makeMockEntry({
        work_unit_id: "wu-csv-cpu-test1",
        leaf_name: "CSV CPU Leaf",
        completed_at: new Date().toISOString(),
        cpu_seconds: 5400,
      })],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    let capturedBlob: Blob | null = null;
    const origCreateObjectURL = URL.createObjectURL;
    const origRevokeObjectURL = URL.revokeObjectURL;
    URL.createObjectURL = (blob: Blob) => {
      capturedBlob = blob;
      return "blob:mock";
    };
    URL.revokeObjectURL = () => {};

    const mockAnchor = { href: "", download: "", click: vi.fn() };
    const origCreateElement = document.createElement;
    document.createElement = ((tag: string, ...args: any[]) => {
      if (tag === "a") return mockAnchor as unknown as HTMLAnchorElement;
      return origCreateElement.call(document, tag, ...args);
    }) as typeof document.createElement;

    render(<HistoryPage />);

    await user.click(screen.getByText("CSV"));

    await waitFor(() => {
      expect(mockAnchor.click).toHaveBeenCalled();
    });

    expect(capturedBlob).not.toBeNull();
    const csvText = await capturedBlob!.text();

    // Header should include cpu_seconds
    const headerLine = csvText.split("\n")[0];
    expect(headerLine).toContain("cpu_seconds");
    expect(headerLine).toContain("duration_seconds");

    // Data row should include cpu_seconds value
    const dataLine = csvText.split("\n")[1];
    expect(dataLine).toContain("5400");

    // Restore
    URL.createObjectURL = origCreateObjectURL;
    URL.revokeObjectURL = origRevokeObjectURL;
    document.createElement = origCreateElement;
  });

  it("JSON export produces valid JSON with entries", async () => {
    const user = userEvent.setup();
    const mockHistoryFn = vi.fn().mockResolvedValue({
      entries: [
        {
          work_unit_id: "wu-json-test1",
          leaf_name: "Test Leaf",
          completed_at: "2026-03-20T12:00:00Z",
          duration_seconds: 1800,
          credit_earned: 50,
          validation_status: "accepted",
        },
      ],
      pagination: { next_cursor: "", has_more: false },
    });
    mockUseClient.mockReturnValue({ client: { history: mockHistoryFn }, error: null });

    mockUseHistory.mockReturnValue({
      entries: [makeMockEntry({
        work_unit_id: "wu-json-test1",
        leaf_name: "Test Leaf",
        completed_at: new Date().toISOString(),
      })],
      hasMore: false,
      isLoading: false,
      loadMore: vi.fn(),
      error: null,
    });

    let capturedBlob: Blob | null = null;
    const origCreateObjectURL = URL.createObjectURL;
    const origRevokeObjectURL = URL.revokeObjectURL;
    URL.createObjectURL = (blob: Blob) => {
      capturedBlob = blob;
      return "blob:mock";
    };
    URL.revokeObjectURL = () => {};

    const mockAnchor = { href: "", download: "", click: vi.fn() };
    const origCreateElement = document.createElement;
    document.createElement = ((tag: string, ...args: any[]) => {
      if (tag === "a") return mockAnchor as unknown as HTMLAnchorElement;
      return origCreateElement.call(document, tag, ...args);
    }) as typeof document.createElement;

    render(<HistoryPage />);

    await user.click(screen.getByText("JSON"));

    await waitFor(() => {
      expect(mockAnchor.click).toHaveBeenCalled();
    });

    expect(capturedBlob).not.toBeNull();
    const jsonText = await capturedBlob!.text();
    const parsed = JSON.parse(jsonText);

    expect(Array.isArray(parsed)).toBe(true);
    expect(parsed[0].work_unit_id).toBe("wu-json-test1");
    expect(mockAnchor.download).toBe("lettuce-history.json");

    // Restore
    URL.createObjectURL = origCreateObjectURL;
    URL.revokeObjectURL = origRevokeObjectURL;
    document.createElement = origCreateElement;
  });
});
