import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HistoryPage, HISTORY_CSV_HEADER, HEAD_ACCEPTED_TOOLTIP, historyToCsv } from "./history";
import type { HistoryEntry, CreditSummary, ResultEntry } from "@/api/client";

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

// The replay frame talks to the Rust host; stand in for it and record its props.
const mockVizFrame = vi.fn();
vi.mock("@/components/viz/VizFrame", () => ({
  VizFrame: (props: Record<string, unknown>) => {
    mockVizFrame(props);
    return <div data-testid="viz-frame" />;
  },
}));

// Mock lucide-react icons to avoid rendering issues
vi.mock("lucide-react", () => ({
  Download: (props: any) => <span data-testid="download-icon" {...props} />,
  Filter: (props: any) => <span data-testid="filter-icon" {...props} />,
  ChevronRight: (props: any) => <span data-testid="chevron-right" {...props} />,
  ChevronDown: (props: any) => <span data-testid="chevron-down" {...props} />,
  Copy: (props: any) => <span data-testid="copy-icon" {...props} />,
  Eye: (props: any) => <span data-testid="eye-icon" {...props} />,
  X: (props: any) => <span data-testid="x-icon" {...props} />,
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
    credit_earned: 0,
    validation_status: "accepted",
    head_name: "lettuce.science",
    ...overrides,
  };
}

/** What `useHistory` returns; every field the page reads has a default. */
function historyState(overrides: Partial<ReturnType<typeof useHistory>> = {}) {
  const entries = overrides.entries ?? [];
  return {
    entries,
    loadedCount: entries.length,
    leafNames: Array.from(new Set(entries.map((e) => e.leaf_name))).sort(),
    hasMore: false,
    isLoading: false,
    loadMore: vi.fn(),
    error: null,
    ...overrides,
  };
}

/** A management client stub with every method the page calls. */
function makeClient(overrides: Record<string, unknown> = {}) {
  return {
    history: vi.fn().mockResolvedValue({
      entries: [],
      pagination: { next_cursor: "", has_more: false },
    }),
    results: vi.fn().mockResolvedValue({ results: [] }),
    resultData: vi.fn().mockResolvedValue({}),
    ...overrides,
  };
}

/** Capture the blob and anchor of a download; returns a restore function. */
function captureDownload() {
  const state: { blob: Blob | null; anchor: { href: string; download: string; click: ReturnType<typeof vi.fn> } } = {
    blob: null,
    anchor: { href: "", download: "", click: vi.fn() },
  };
  const origCreateObjectURL = URL.createObjectURL;
  const origRevokeObjectURL = URL.revokeObjectURL;
  URL.createObjectURL = (blob: Blob) => {
    state.blob = blob;
    return "blob:mock";
  };
  URL.revokeObjectURL = () => {};
  const origCreateElement = document.createElement;
  document.createElement = ((tag: string, ...args: any[]) => {
    if (tag === "a") return state.anchor as unknown as HTMLAnchorElement;
    return origCreateElement.call(document, tag, ...args);
  }) as typeof document.createElement;
  const restore = () => {
    URL.createObjectURL = origCreateObjectURL;
    URL.revokeObjectURL = origRevokeObjectURL;
    document.createElement = origCreateElement;
  };
  return { state, restore };
}

/** The leaf-name span inside a history row (the name also appears in the filter). */
function rowLabel(name: string): HTMLElement {
  return screen.getAllByText(name).find(
    (el) => el.tagName === "SPAN" && el.classList.contains("font-medium")
  )!;
}

describe("HistoryPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseClient.mockReturnValue({ client: makeClient(), error: null });
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
    mockUseHistory.mockReturnValue(historyState());

    render(<HistoryPage />);
    expect(
      screen.getByText("No completed work units yet.")
    ).toBeInTheDocument();
    expect(
      screen.getByText("Start contributing to see your history here.")
    ).toBeInTheDocument();
    expect(screen.queryByTestId("history-count")).not.toBeInTheDocument();
    expect(screen.queryByTestId("history-end")).not.toBeInTheDocument();
  });

  it("renders loading skeleton when loading", () => {
    mockUseHistory.mockReturnValue(historyState({ isLoading: true }));

    const { container } = render(<HistoryPage />);
    // Skeleton rows have animate-pulse class
    const skeletons = container.querySelectorAll(".animate-pulse");
    expect(skeletons.length).toBeGreaterThan(0);
  });

  it("renders error message when error occurs", () => {
    mockUseHistory.mockReturnValue(historyState({ error: new Error("Connection failed") }));

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

    mockUseHistory.mockReturnValue(historyState({ entries }));

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

    mockUseHistory.mockReturnValue(historyState({ entries }));

    render(<HistoryPage />);
    // Displays first 8 chars of work unit id
    expect(screen.getByText("wu-abcde")).toBeInTheDocument();
  });

  // --- Head accepted: the daemon's validation_status records acceptance on submission ---

  it("labels an accepted submission 'Head accepted' with an explanatory tooltip", () => {
    mockUseHistory.mockReturnValue(
      historyState({ entries: [makeMockEntry({ validation_status: "accepted" })] })
    );

    render(<HistoryPage />);
    const row = screen.getByTestId("history-row");
    const badge = within(row).getByText("Head accepted");
    expect(badge).toHaveAttribute("title", HEAD_ACCEPTED_TOOLTIP);
    expect(HEAD_ACCEPTED_TOOLTIP).toMatch(/accepted this submission/);
    expect(HEAD_ACCEPTED_TOOLTIP).toMatch(/decided later on the head/);
    // The old, misleading label is gone
    expect(screen.queryByText("Validated")).not.toBeInTheDocument();
  });

  it("labels a rejected submission 'Head rejected'", () => {
    mockUseHistory.mockReturnValue(
      historyState({ entries: [makeMockEntry({ validation_status: "rejected" })] })
    );

    render(<HistoryPage />);
    const row = screen.getByTestId("history-row");
    expect(within(row).getByText("Head rejected")).toBeInTheDocument();
  });

  it("offers the head-accepted filter with honest option labels", () => {
    mockUseHistory.mockReturnValue(historyState());

    render(<HistoryPage />);
    const select = screen.getByLabelText("Head accepted") as HTMLSelectElement;
    const labels = Array.from(select.options).map((o) => o.text);
    expect(labels).toEqual(["All submissions", "Head accepted", "Head rejected"]);
    expect(select).toHaveAttribute("title", HEAD_ACCEPTED_TOOLTIP);
  });

  it("passes the head-accepted filter to useHistory", async () => {
    const user = userEvent.setup();
    mockUseHistory.mockReturnValue(historyState());

    render(<HistoryPage />);
    await user.selectOptions(screen.getByLabelText("Head accepted"), "rejected");

    const lastCall = mockUseHistory.mock.calls[mockUseHistory.mock.calls.length - 1][0];
    expect(lastCall.headAccepted).toBe("rejected");
  });

  // --- Leaf filter: by name, applied client-side ---

  it("lists every leaf name the hook has seen and passes the chosen one as leafName", async () => {
    const user = userEvent.setup();
    mockUseHistory.mockReturnValue(
      historyState({
        entries: [makeMockEntry({ leaf_name: "Alpha Leaf" })],
        leafNames: ["Alpha Leaf", "Beta Leaf"],
      })
    );

    render(<HistoryPage />);
    const select = screen.getByLabelText("Leaf") as HTMLSelectElement;
    expect(Array.from(select.options).map((o) => o.text)).toEqual([
      "All Leafs",
      "Alpha Leaf",
      "Beta Leaf",
    ]);

    await user.selectOptions(select, "Beta Leaf");

    const lastCall = mockUseHistory.mock.calls[mockUseHistory.mock.calls.length - 1][0];
    expect(lastCall.leafName).toBe("Beta Leaf");
    expect(lastCall.leafId).toBeUndefined();
  });

  it("keeps the selected leaf in the dropdown even when no loaded page names it", async () => {
    const user = userEvent.setup();
    // Once a leaf is chosen the hook reloads; simulate pages that never mention it.
    mockUseHistory.mockImplementation((filters: { leafName?: string }) =>
      historyState({
        entries: [],
        loadedCount: 50,
        leafNames: filters.leafName ? ["Alpha Leaf"] : ["Alpha Leaf", "Beta Leaf"],
      })
    );

    render(<HistoryPage />);
    const select = screen.getByLabelText("Leaf") as HTMLSelectElement;
    await user.selectOptions(select, "Beta Leaf");

    expect(select.value).toBe("Beta Leaf");
    expect(Array.from(select.options).map((o) => o.text)).toEqual([
      "All Leafs",
      "Alpha Leaf",
      "Beta Leaf",
    ]);
  });

  // --- Count and end-of-history marker ---

  it("shows how many entries are loaded and an end-of-history marker", () => {
    mockUseHistory.mockReturnValue(
      historyState({
        entries: [makeMockEntry(), makeMockEntry()],
        hasMore: false,
      })
    );

    render(<HistoryPage />);
    expect(screen.getByTestId("history-count")).toHaveTextContent("2 entries loaded");
    expect(screen.getByTestId("history-end")).toHaveTextContent("End of history — 2 entries");
  });

  it("shows the matching count separately when a client-side filter is active", async () => {
    const user = userEvent.setup();
    mockUseHistory.mockImplementation(() =>
      historyState({
        entries: [makeMockEntry({ validation_status: "rejected" })],
        loadedCount: 40,
      })
    );

    render(<HistoryPage />);
    // Default filters -> plain count of what the daemon returned
    expect(screen.getByTestId("history-count")).toHaveTextContent("40 entries loaded");
    expect(screen.getByTestId("history-end")).toHaveTextContent("End of history — 1 entry");

    await user.selectOptions(screen.getByLabelText("Head accepted"), "rejected");
    expect(screen.getByTestId("history-count")).toHaveTextContent("1 matching of 40 loaded");
    expect(screen.getByTestId("history-end")).toHaveTextContent(
      "End of history — 1 entry match the current filters"
    );
  });

  it("hides the end marker and keeps the scroll sentinel while more pages exist", () => {
    mockUseHistory.mockReturnValue(
      historyState({ entries: [makeMockEntry()], hasMore: true })
    );

    const { container } = render(<HistoryPage />);
    expect(screen.queryByTestId("history-end")).not.toBeInTheDocument();
    expect(container.querySelector("[data-sentinel]")).toBeTruthy();
  });

  it("says when filters match nothing in a non-empty history", () => {
    mockUseHistory.mockReturnValue(historyState({ entries: [], loadedCount: 30 }));

    render(<HistoryPage />);
    expect(screen.getByText("No entries match the current filters.")).toBeInTheDocument();
    expect(screen.queryByText("No completed work units yet.")).not.toBeInTheDocument();
  });

  // --- Credit ---

  it("formats a decimal per-unit credit and hides a zero", () => {
    mockUseHistory.mockReturnValue(
      historyState({
        entries: [
          makeMockEntry({ work_unit_id: "wu-credit-1", leaf_name: "Credited", credit_earned: 12.3456 }),
          makeMockEntry({ work_unit_id: "wu-credit-0", leaf_name: "Uncredited", credit_earned: 0 }),
        ],
      })
    );

    render(<HistoryPage />);
    expect(screen.getByText("+12.35")).toBeInTheDocument();
    expect(screen.queryByText("+0")).not.toBeInTheDocument();
  });

  it("renders filter controls", () => {
    mockUseHistory.mockReturnValue(historyState());

    render(<HistoryPage />);

    // Leaf filter select
    expect(screen.getByText("All Leafs")).toBeInTheDocument();

    // Date range filter
    expect(screen.getByText("Last 7 days")).toBeInTheDocument();
    expect(screen.getByText("Last 30 days")).toBeInTheDocument();

    // Head accepted filter
    expect(screen.getByText("All submissions")).toBeInTheDocument();
  });

  it("renders export buttons", () => {
    mockUseHistory.mockReturnValue(
      historyState({ entries: [makeMockEntry({ completed_at: new Date().toISOString() })] })
    );

    render(<HistoryPage />);
    expect(screen.getByText("CSV")).toBeInTheDocument();
    expect(screen.getByText("JSON")).toBeInTheDocument();
  });

  it("disables export buttons when no entries", () => {
    mockUseHistory.mockReturnValue(historyState());

    render(<HistoryPage />);
    const csvBtn = screen.getByText("CSV").closest("button");
    const jsonBtn = screen.getByText("JSON").closest("button");
    expect(csvBtn).toBeDisabled();
    expect(jsonBtn).toBeDisabled();
  });

  it("renders credit breakdown with by_leaf fallback and decimal credit", () => {
    mockUseHistory.mockReturnValue(
      historyState({ entries: [makeMockEntry({ completed_at: new Date().toISOString() })] })
    );

    mockUseCredit.mockReturnValue({
      credit: {
        total_credit: 500.5,
        today: 50,
        this_week: 200,
        this_month: 500,
        by_leaf: [
          { leaf_id: "p1", leaf_name: "Alpha", credit: 300.25 },
          { leaf_id: "p2", leaf_name: "Beta", credit: 200.25 },
        ],
        by_head: [],
        source: "head",
      } as CreditSummary,
      isLoading: false,
      error: null,
    });

    render(<HistoryPage />);
    // Falls back to "Credit by Leaf" when by_head is empty
    expect(screen.getAllByText("Credit by Leaf").length).toBeGreaterThan(0);
    expect(screen.getAllByText("300.25 (60%)").length).toBeGreaterThan(0);
  });

  it("renders Credit by Head when by_head data is present", () => {
    mockUseHistory.mockReturnValue(
      historyState({ entries: [makeMockEntry({ completed_at: new Date().toISOString() })] })
    );

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
            volunteer_id: "v1",
            total_credit: 1234.5,
            available: true,
          },
        ],
        source: "head",
      } as CreditSummary,
      isLoading: false,
      error: null,
    });

    render(<HistoryPage />);
    expect(screen.getAllByText("Credit by Head").length).toBeGreaterThan(0);
    expect(screen.getAllByText("1,234.5 (100%)").length).toBeGreaterThan(0);
  });

  // --- Export ---

  it("CSV export uses the documented header and fills head_name from heads data", async () => {
    const user = userEvent.setup();
    const client = makeClient({
      history: vi.fn().mockResolvedValue({
        entries: [
          {
            work_unit_id: "wu-export-test1",
            leaf_name: "Prime Study",
            completed_at: "2026-03-20T12:00:00Z",
            duration_seconds: 3600,
            cpu_seconds: 3000,
            credit_earned: 0,
            validation_status: "accepted",
            head_name: "",
          },
        ],
        pagination: { next_cursor: "", has_more: false },
      }),
    });
    mockUseClient.mockReturnValue({ client, error: null });

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

    mockUseHistory.mockReturnValue(
      historyState({
        entries: [
          makeMockEntry({
            work_unit_id: "wu-export-test1",
            leaf_name: "Prime Study",
            completed_at: new Date().toISOString(),
          }),
        ],
      })
    );

    const { state, restore } = captureDownload();
    try {
      render(<HistoryPage />);
      await user.click(screen.getByText("CSV"));
      await waitFor(() => {
        expect(state.anchor.click).toHaveBeenCalled();
      });

      expect(state.blob).not.toBeNull();
      const csvText = await state.blob!.text();
      const [header, row] = csvText.split("\n");
      expect(header).toBe(HISTORY_CSV_HEADER);
      expect(header).toBe(
        "work_unit_id,leaf_name,head_name,completed_at,duration_seconds,cpu_seconds,credit_earned,head_accepted"
      );
      expect(row).toBe(
        'wu-export-test1,"Prime Study","lettuce.science",2026-03-20T12:00:00Z,3600,3000,0,true'
      );
      expect(state.anchor.download).toBe("lettuce-history.csv");
    } finally {
      restore();
    }
  });

  it("CSV export applies the client-side filters", async () => {
    const user = userEvent.setup();
    const client = makeClient({
      history: vi.fn().mockResolvedValue({
        entries: [
          makeMockEntry({ work_unit_id: "wu-acc", validation_status: "accepted" }),
          makeMockEntry({ work_unit_id: "wu-rej", validation_status: "rejected" }),
        ],
        pagination: { next_cursor: "", has_more: false },
      }),
    });
    mockUseClient.mockReturnValue({ client, error: null });
    mockUseHistory.mockReturnValue(
      historyState({ entries: [makeMockEntry({ validation_status: "rejected" })] })
    );

    const { state, restore } = captureDownload();
    try {
      render(<HistoryPage />);
      await user.selectOptions(screen.getByLabelText("Head accepted"), "rejected");
      await user.click(screen.getByText("CSV"));
      await waitFor(() => {
        expect(state.anchor.click).toHaveBeenCalled();
      });

      const csvText = await state.blob!.text();
      expect(csvText).toContain("wu-rej");
      expect(csvText).not.toContain("wu-acc");
      expect(csvText.trim().split("\n")).toHaveLength(2);
    } finally {
      restore();
    }
  });

  it("JSON export produces valid JSON with entries", async () => {
    const user = userEvent.setup();
    const client = makeClient({
      history: vi.fn().mockResolvedValue({
        entries: [
          {
            work_unit_id: "wu-json-test1",
            leaf_name: "Test Leaf",
            completed_at: "2026-03-20T12:00:00Z",
            duration_seconds: 1800,
            cpu_seconds: 1700,
            credit_earned: 0,
            validation_status: "accepted",
            head_name: "lettuce.science",
          },
        ],
        pagination: { next_cursor: "", has_more: false },
      }),
    });
    mockUseClient.mockReturnValue({ client, error: null });

    mockUseHistory.mockReturnValue(
      historyState({
        entries: [
          makeMockEntry({
            work_unit_id: "wu-json-test1",
            leaf_name: "Test Leaf",
            completed_at: new Date().toISOString(),
          }),
        ],
      })
    );

    const { state, restore } = captureDownload();
    try {
      render(<HistoryPage />);
      await user.click(screen.getByText("JSON"));
      await waitFor(() => {
        expect(state.anchor.click).toHaveBeenCalled();
      });

      expect(state.blob).not.toBeNull();
      const parsed = JSON.parse(await state.blob!.text());
      expect(Array.isArray(parsed)).toBe(true);
      expect(parsed[0].work_unit_id).toBe("wu-json-test1");
      expect(parsed[0].validation_status).toBe("accepted");
      expect(state.anchor.download).toBe("lettuce-history.json");
    } finally {
      restore();
    }
  });

  // --- Row expand/collapse and detail section ---

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

    mockUseHistory.mockReturnValue(historyState({ entries }));

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

    mockUseHistory.mockReturnValue(historyState({ entries }));

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

    mockUseHistory.mockReturnValue(historyState({ entries }));

    render(<HistoryPage />);

    // Before expanding, the detail labels should not be in the document
    expect(screen.queryByText("CPU Time")).not.toBeInTheDocument();
    expect(screen.queryByText("Wall Clock")).not.toBeInTheDocument();

    await user.click(rowLabel("Expandable Leaf"));

    // After expanding, detail section should appear
    expect(screen.getByText("CPU Time")).toBeInTheDocument();
    expect(screen.getByText("Wall Clock")).toBeInTheDocument();
    expect(screen.getByText("Time Paused")).toBeInTheDocument();
    expect(screen.getByText("Head")).toBeInTheDocument();
    expect(screen.getByText("Work Unit ID")).toBeInTheDocument();
    // The detail restates what "head accepted" means
    expect(screen.getByText("Yes")).toBeInTheDocument();
    expect(
      screen.getByText(/on submission; validation and credit are decided later on the head/)
    ).toBeInTheDocument();
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

    mockUseHistory.mockReturnValue(historyState({ entries }));

    render(<HistoryPage />);
    await user.click(rowLabel("Detail Leaf"));

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

    mockUseHistory.mockReturnValue(historyState({ entries }));

    render(<HistoryPage />);

    const label = rowLabel("Collapsible Leaf");
    await user.click(label);
    expect(screen.getByText("CPU Time")).toBeInTheDocument();

    await user.click(label);
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

    mockUseHistory.mockReturnValue(historyState({ entries }));

    render(<HistoryPage />);

    // Collapsed state: chevron-right
    expect(screen.getByTestId("chevron-right")).toBeInTheDocument();
    expect(screen.queryByTestId("chevron-down")).not.toBeInTheDocument();

    await user.click(rowLabel("Chevron Leaf"));

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

    mockUseHistory.mockReturnValue(historyState({ entries }));

    render(<HistoryPage />);

    await user.click(rowLabel("Copy Test Leaf"));

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

    mockUseHistory.mockReturnValue(historyState({ entries }));

    render(<HistoryPage />);
    await user.click(rowLabel("No Head Leaf"));

    // Should show "—" for head
    const headLabel = screen.getByText("Head");
    const headValue = headLabel.parentElement?.querySelector(".font-medium");
    expect(headValue?.textContent).toBe("—");
  });

  // --- Results replay ---

  it("offers 'View Visualization' only for units with a locally persisted result", async () => {
    const user = userEvent.setup();
    const result: ResultEntry = {
      work_unit_id: "wu-with-result",
      leaf_name: "Beyblade Arena",
      leaf_slug: "beyblade-arena",
      head_name: "lbry.science",
      completed_at: new Date().toISOString(),
      viz_bundle_path: "/home/u/.lettuce/container-work/wu-with-result/.lettuce-viz",
      size_bytes: 1234,
    };
    const client = makeClient({
      results: vi.fn().mockResolvedValue({ results: [result] }),
    });
    mockUseClient.mockReturnValue({ client, error: null });
    mockUseHistory.mockReturnValue(
      historyState({
        entries: [
          makeMockEntry({ work_unit_id: "wu-with-result", leaf_name: "Beyblade Arena" }),
          makeMockEntry({ work_unit_id: "wu-without-result", leaf_name: "Plain Leaf" }),
        ],
      })
    );

    render(<HistoryPage />);
    await waitFor(() => {
      expect(client.results).toHaveBeenCalled();
    });

    await user.click(rowLabel("Plain Leaf"));
    expect(screen.queryByText("View Visualization")).not.toBeInTheDocument();

    await user.click(rowLabel("Beyblade Arena"));
    expect(screen.getByText("View Visualization")).toBeInTheDocument();
  });

  it("replays a result in the viz frame with the persisted bundle path and slug", async () => {
    const user = userEvent.setup();
    const result: ResultEntry = {
      work_unit_id: "wu-replay",
      leaf_name: "Beyblade Arena",
      leaf_slug: "beyblade-arena",
      head_name: "lbry.science",
      completed_at: new Date().toISOString(),
      viz_bundle_path: "/home/u/.lettuce/container-work/wu-replay/.lettuce-viz",
      size_bytes: 10,
    };
    const client = makeClient({
      results: vi.fn().mockResolvedValue({ results: [result] }),
      resultData: vi.fn().mockResolvedValue({ frames: [1, 2, 3] }),
    });
    mockUseClient.mockReturnValue({ client, error: null });
    mockUseHistory.mockReturnValue(
      historyState({
        entries: [makeMockEntry({ work_unit_id: "wu-replay", leaf_name: "Beyblade Arena" })],
      })
    );

    render(<HistoryPage />);
    await waitFor(() => expect(client.results).toHaveBeenCalled());
    await user.click(rowLabel("Beyblade Arena"));
    await user.click(screen.getByText("View Visualization"));

    await waitFor(() => {
      expect(screen.getByTestId("viz-frame")).toBeInTheDocument();
    });
    expect(client.resultData).toHaveBeenCalledWith("wu-replay");
    expect(mockVizFrame).toHaveBeenCalledWith(
      expect.objectContaining({
        mode: "replay",
        vizBundlePath: result.viz_bundle_path,
        leafSlug: "beyblade-arena",
        replayData: { frames: [1, 2, 3] },
      })
    );

    // Escape closes the dialog
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("explains when the persisted result can no longer be read", async () => {
    const user = userEvent.setup();
    const client = makeClient({
      results: vi.fn().mockResolvedValue({
        results: [
          {
            work_unit_id: "wu-gone",
            leaf_name: "Gone Leaf",
            leaf_slug: "gone",
            head_name: "h",
            completed_at: new Date().toISOString(),
            viz_bundle_path: "/gone/.lettuce-viz",
            size_bytes: 1,
          },
        ],
      }),
      resultData: vi.fn().mockRejectedValue(new Error("NOT_FOUND")),
    });
    mockUseClient.mockReturnValue({ client, error: null });
    mockUseHistory.mockReturnValue(
      historyState({ entries: [makeMockEntry({ work_unit_id: "wu-gone", leaf_name: "Gone Leaf" })] })
    );

    render(<HistoryPage />);
    await waitFor(() => expect(client.results).toHaveBeenCalled());
    await user.click(rowLabel("Gone Leaf"));
    await user.click(screen.getByText("View Visualization"));

    await waitFor(() => {
      expect(
        screen.getByText("This result is no longer stored on this machine.")
      ).toBeInTheDocument();
    });
    expect(screen.queryByTestId("viz-frame")).not.toBeInTheDocument();
  });
});

describe("historyToCsv", () => {
  it("quotes names, escapes embedded quotes, and writes head_accepted as true/false", () => {
    const csv = historyToCsv(
      [
        makeMockEntry({
          work_unit_id: "wu-1",
          leaf_name: 'Say "hi"',
          head_name: "lettuce.science",
          completed_at: "2026-03-20T12:00:00Z",
          duration_seconds: 10,
          cpu_seconds: 9,
          credit_earned: 1.5,
          validation_status: "rejected",
        }),
      ],
      new Map()
    );
    expect(csv.split("\n")).toEqual([
      HISTORY_CSV_HEADER,
      'wu-1,"Say ""hi""","lettuce.science",2026-03-20T12:00:00Z,10,9,1.5,false',
    ]);
  });
});
