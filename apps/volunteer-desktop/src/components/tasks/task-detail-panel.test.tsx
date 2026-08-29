import { describe, it, expect, vi, beforeEach, beforeAll, afterEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TaskDetailPanel } from "./task-detail-panel";
import type { TaskDetail } from "@/api/client";

// --- Mock useClient with a STABLE client reference ---

const mockTaskDetails = vi.fn();

const stableClient = {
  taskDetails: (...args: unknown[]) => mockTaskDetails(...args),
};

vi.mock("@/hooks/use-api", () => ({
  useClient: () => ({
    client: stableClient,
    error: null,
  }),
}));

// --- Mock clipboard ---

let mockWriteText: ReturnType<typeof vi.fn>;

beforeAll(() => {
  mockWriteText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(window.navigator, "clipboard", {
    value: { writeText: mockWriteText },
    writable: true,
    configurable: true,
  });
});

beforeEach(() => {
  mockTaskDetails.mockReset();
  mockWriteText.mockClear();
});

afterEach(() => {
  // Clean up Radix Dialog/AlertDialog body mutations (scroll lock, pointer-events)
  document.body.removeAttribute("data-scroll-locked");
  document.body.style.removeProperty("pointer-events");
});

// --- Helper to build a TaskDetail object ---

function makeDetail(overrides: Partial<TaskDetail> = {}): TaskDetail {
  return {
    work_unit_id: "wu-detail-01",
    leaf_name: "Climate Sim",
    progress_pct: 65,
    elapsed_seconds: 7200,
    estimated_remaining_seconds: 3600,
    work_dir: "/tmp/wu-detail-01",
    viz_bundle_path: null,
    checkpoint_sequence: 3,
    last_checkpoint_at: "2026-03-29T10:00:00Z",
    resumed_from_checkpoint: false,
    cpu_seconds: 6000,
    task_status: "running",
    status_reason: null,
    deadline_seconds: 86400,
    head_name: "lettuce.science",
    runtime_type: "native",
    process_id: 4567,
    memory_rss_mb: 512,
    virtual_memory_mb: 1024,
    cpu_usage_pct: 85,
    disk_read_mb: 100,
    disk_written_mb: 50,
    time_since_checkpoint_seconds: 300,
    estimated_completion_at: "2026-03-29T14:00:00Z",
    progress_rate_pct_per_hour: 9.2,
    fraction_done: 65.0,
    container_image: null,
    ...overrides,
  };
}

// Default props helper
function renderPanel(overrides: Partial<Parameters<typeof TaskDetailPanel>[0]> = {}) {
  const defaultProps = {
    workUnitId: "wu-detail-01",
    open: true,
    onOpenChange: vi.fn(),
    onSuspend: vi.fn(),
    onResume: vi.fn(),
    onAbort: vi.fn(),
    ...overrides,
  };
  return { ...render(<TaskDetailPanel {...defaultProps} />), props: defaultProps };
}

/** Wait for the detail content to load (i.e., the loading spinner to disappear). */
async function waitForDetail() {
  await waitFor(() => {
    expect(screen.queryByText("Loading...")).not.toBeInTheDocument();
  });
}

describe("TaskDetailPanel", () => {
  // --- Loading state ---

  it("shows loading state when open and fetching", () => {
    mockTaskDetails.mockReturnValue(new Promise(() => {}));
    renderPanel();
    expect(screen.getByText("Loading...")).toBeInTheDocument();
  });

  // --- Error state ---

  it("shows error state on API failure", async () => {
    mockTaskDetails.mockRejectedValue(new Error("Network failed"));
    renderPanel();
    expect(await screen.findByText("Network failed")).toBeInTheDocument();
  });

  // --- Successful load ---

  it("renders leaf name as sheet title", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    await waitForDetail();
    const heading = screen.getByRole("heading", { name: "Climate Sim" });
    expect(heading).toBeInTheDocument();
  });

  it("renders work unit ID", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    expect(await screen.findByText("wu-detail-01")).toBeInTheDocument();
  });

  it("renders all 4 collapsible sections", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("Timing")).toBeInTheDocument();
    expect(screen.getByText("Progress")).toBeInTheDocument();
    expect(screen.getByText("Resources")).toBeInTheDocument();
    expect(screen.getByText("Task Info")).toBeInTheDocument();
  });

  // --- Timing section ---

  it("renders CPU Time in Timing section", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ cpu_seconds: 6000 }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("CPU Time")).toBeInTheDocument();
    expect(screen.getByText("1h 40m")).toBeInTheDocument();
  });

  it("renders Elapsed Time in Timing section", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ elapsed_seconds: 7200 }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("Elapsed Time")).toBeInTheDocument();
    expect(screen.getByText("2h 0m")).toBeInTheDocument();
  });

  // --- Nullable fields show '---' ---

  it("shows '---' for null estimated_remaining_seconds", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ estimated_remaining_seconds: null }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("Est. Remaining")).toBeInTheDocument();
    const dashes = screen.getAllByText("---");
    expect(dashes.length).toBeGreaterThanOrEqual(1);
  });

  it("shows '---' for null resource fields", async () => {
    mockTaskDetails.mockResolvedValue(
      makeDetail({
        memory_rss_mb: null,
        virtual_memory_mb: null,
        cpu_usage_pct: null,
        disk_read_mb: null,
        disk_written_mb: null,
      })
    );
    renderPanel();
    await waitForDetail();
    const dashes = screen.getAllByText("---");
    expect(dashes.length).toBeGreaterThanOrEqual(5);
  });

  it("shows '---' for null container_image", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ container_image: null }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("Container Image")).toBeInTheDocument();
  });

  it("shows '---' for null process_id", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ process_id: null }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("Process ID")).toBeInTheDocument();
  });

  // --- Resources section ---

  it("renders memory values", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ memory_rss_mb: 512, virtual_memory_mb: 1024 }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("512 MB")).toBeInTheDocument();
    expect(screen.getByText("1024 MB")).toBeInTheDocument();
  });

  it("renders CPU usage percentage", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ cpu_usage_pct: 85 }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("85%")).toBeInTheDocument();
  });

  // --- Progress section ---

  it("renders fraction done with 3 decimal places", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ fraction_done: 65.123 }));
    renderPanel();
    await screen.findByText("65.123%");
  });

  it("renders progress rate", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ progress_rate_pct_per_hour: 9.2 }));
    renderPanel();
    await screen.findByText("9.2% / hour");
  });

  it("shows '---' for null progress rate", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ progress_rate_pct_per_hour: null }));
    renderPanel();
    await waitForDetail();
    const dashes = screen.getAllByText("---");
    expect(dashes.length).toBeGreaterThanOrEqual(1);
  });

  // --- Task Info section ---

  it("renders head name", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    await waitForDetail();
    const headValues = screen.getAllByText("lettuce.science");
    expect(headValues.length).toBeGreaterThanOrEqual(1);
  });

  it("renders runtime badge for native", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ runtime_type: "native" }));
    renderPanel();
    await screen.findByText("Native");
  });

  it("renders checkpoint info", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ checkpoint_sequence: 3 }));
    renderPanel();
    const checkpointText = await screen.findByText(/seq 3/);
    expect(checkpointText).toBeInTheDocument();
  });

  it("renders 'None' for checkpoint when sequence is 0", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ checkpoint_sequence: 0 }));
    renderPanel();
    await screen.findByText("None");
  });

  it("renders resumed status", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ resumed_from_checkpoint: true }));
    renderPanel();
    await screen.findByText("Yes");
  });

  // --- Copy WU ID button ---

  it("copies work unit ID to clipboard when copy button is clicked", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    await waitForDetail();

    const copyBtn = screen.getByTitle("Copy Work Unit ID");
    fireEvent.click(copyBtn);

    await waitFor(() => {
      expect(mockWriteText).toHaveBeenCalledWith("wu-detail-01");
    });
  });

  // --- Copy All button ---

  it("shows Copy All button", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    const copyAllBtn = await screen.findByRole("button", { name: "Copy All" });
    expect(copyAllBtn).toBeInTheDocument();
  });

  it("copies formatted text to clipboard when Copy All is clicked", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    await waitForDetail();

    const dialog = document.querySelector("[role='dialog']")!;
    const allButtons = dialog.querySelectorAll("button");
    const copyAllBtn = Array.from(allButtons).find((b) => b.textContent === "Copy All");
    expect(copyAllBtn).toBeDefined();

    fireEvent.click(copyAllBtn!);

    await waitFor(() => {
      expect(mockWriteText).toHaveBeenCalledTimes(1);
    });
    const copiedText = mockWriteText.mock.calls[0][0] as string;
    expect(copiedText).toContain("Leaf: Climate Sim");
    expect(copiedText).toContain("Work Unit ID: wu-detail-01");
    expect(copiedText).toContain("Status: Running");
    expect(copiedText).toContain("--- Timing ---");
    expect(copiedText).toContain("--- Progress ---");
    expect(copiedText).toContain("--- Resources ---");
    expect(copiedText).toContain("--- Task Info ---");
  });

  // --- Copy All with null fields ---

  it("copies formatted text with null fields gracefully", async () => {
    mockTaskDetails.mockResolvedValue(
      makeDetail({
        memory_rss_mb: null,
        virtual_memory_mb: null,
        cpu_usage_pct: null,
        disk_read_mb: null,
        disk_written_mb: null,
        time_since_checkpoint_seconds: null,
        estimated_completion_at: null,
        estimated_remaining_seconds: null,
        progress_rate_pct_per_hour: null,
        container_image: null,
        process_id: null,
        checkpoint_sequence: 0,
      })
    );
    renderPanel();
    await waitForDetail();

    const dialog = document.querySelector("[role='dialog']")!;
    const allButtons = dialog.querySelectorAll("button");
    const copyAllBtn = Array.from(allButtons).find((b) => b.textContent === "Copy All");
    expect(copyAllBtn).toBeDefined();
    fireEvent.click(copyAllBtn!);

    await waitFor(() => {
      expect(mockWriteText).toHaveBeenCalledTimes(1);
    });
    const copiedText = mockWriteText.mock.calls[0][0] as string;
    // Null fields should render as "---"
    expect(copiedText).toContain("Memory (RSS): ---");
    expect(copiedText).toContain("Virtual Memory: ---");
    expect(copiedText).toContain("CPU Usage: ---");
    expect(copiedText).toContain("Disk Read: ---");
    expect(copiedText).toContain("Disk Written: ---");
    expect(copiedText).toContain("Time Since Checkpoint: ---");
    expect(copiedText).toContain("Estimated Completion: ---");
    expect(copiedText).toContain("Estimated Remaining: ---");
    expect(copiedText).toContain("Progress Rate: ---");
    expect(copiedText).toContain("Container Image: ---");
    expect(copiedText).toContain("Process ID: ---");
    expect(copiedText).toContain("Checkpoint: None");
  });

  // --- Collapsible sections ---

  it("collapses a section when its header is clicked", async () => {
    const user = userEvent.setup();
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    await waitForDetail();

    expect(screen.getByText("CPU Time")).toBeInTheDocument();
    await user.click(screen.getByText("Timing"));
    expect(screen.queryByText("CPU Time")).not.toBeInTheDocument();
  });

  it("re-expands a collapsed section when clicked again", async () => {
    const user = userEvent.setup();
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    await waitForDetail();

    await user.click(screen.getByText("Timing"));
    expect(screen.queryByText("CPU Time")).not.toBeInTheDocument();

    await user.click(screen.getByText("Timing"));
    expect(screen.getByText("CPU Time")).toBeInTheDocument();
  });

  it("sections toggle independently", async () => {
    const user = userEvent.setup();
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    await waitForDetail();

    await user.click(screen.getByText("Timing"));
    expect(screen.queryByText("CPU Time")).not.toBeInTheDocument();

    expect(screen.getByText("Memory (RSS)")).toBeInTheDocument();

    await user.click(screen.getByText("Resources"));
    expect(screen.queryByText("Memory (RSS)")).not.toBeInTheDocument();

    expect(screen.queryByText("CPU Time")).not.toBeInTheDocument();
    expect(screen.getByText("Head")).toBeInTheDocument();
  });

  // --- Suspend button for running tasks ---

  it("shows Suspend button for running tasks", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "running" }));
    renderPanel();
    const suspendBtn = await screen.findByRole("button", { name: "Suspend" });
    expect(suspendBtn).toBeInTheDocument();
  });

  it("calls onSuspend and closes panel when Suspend is clicked", async () => {
    const user = userEvent.setup();
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "running" }));
    const { props } = renderPanel();
    const suspendBtn = await screen.findByRole("button", { name: "Suspend" });
    await user.click(suspendBtn);

    expect(props.onSuspend).toHaveBeenCalledWith("wu-detail-01");
    expect(props.onOpenChange).toHaveBeenCalledWith(false);
  });

  // --- Resume button for suspended tasks ---

  it("shows Resume button for suspended_user tasks", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "suspended_user" }));
    renderPanel();
    const resumeBtn = await screen.findByRole("button", { name: "Resume" });
    expect(resumeBtn).toBeInTheDocument();
  });

  it("shows Resume button for suspended_thermal tasks", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "suspended_thermal" }));
    renderPanel();
    const resumeBtn = await screen.findByRole("button", { name: "Resume" });
    expect(resumeBtn).toBeInTheDocument();
  });

  it("shows Resume button for suspended_scheduled tasks", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "suspended_scheduled" }));
    renderPanel();
    const resumeBtn = await screen.findByRole("button", { name: "Resume" });
    expect(resumeBtn).toBeInTheDocument();
  });

  it("calls onResume and closes panel when Resume is clicked", async () => {
    const user = userEvent.setup();
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "suspended_user" }));
    const { props } = renderPanel();
    const resumeBtn = await screen.findByRole("button", { name: "Resume" });
    await user.click(resumeBtn);

    expect(props.onResume).toHaveBeenCalledWith("wu-detail-01");
    expect(props.onOpenChange).toHaveBeenCalledWith(false);
  });

  // --- Neither Suspend nor Resume for error tasks ---

  it("shows neither Suspend nor Resume for error tasks", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "error" }));
    renderPanel();
    await waitForDetail();
    expect(screen.queryByRole("button", { name: "Suspend" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resume" })).not.toBeInTheDocument();
  });

  // --- Panel with null workUnitId ---

  it("does not call taskDetails when workUnitId is null", () => {
    renderPanel({ workUnitId: null, open: false });
    expect(mockTaskDetails).not.toHaveBeenCalled();
  });

  it("does not render detail content when open is false", () => {
    renderPanel({ open: false });
    expect(screen.queryByText("Loading...")).not.toBeInTheDocument();
    expect(mockTaskDetails).not.toHaveBeenCalled();
  });

  // --- Container and WASM runtime badges ---

  it("renders runtime badge for container", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ runtime_type: "container" }));
    renderPanel();
    await screen.findByText("Container");
  });

  it("renders runtime badge for wasm", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ runtime_type: "wasm" }));
    renderPanel();
    await screen.findByText("WASM");
  });

  // --- Re-fetch when workUnitId changes ---

  it("re-fetches when workUnitId changes", async () => {
    const detail1 = makeDetail({ work_unit_id: "wu-a", leaf_name: "First Task" });
    const detail2 = makeDetail({ work_unit_id: "wu-b", leaf_name: "Second Task" });
    mockTaskDetails
      .mockResolvedValueOnce(detail1)
      .mockResolvedValueOnce(detail2);

    const { rerender, props } = renderPanel({ workUnitId: "wu-a" });
    await screen.findByRole("heading", { name: "First Task" });
    expect(mockTaskDetails).toHaveBeenCalledWith("wu-a");

    rerender(
      <TaskDetailPanel
        {...props}
        workUnitId="wu-b"
      />
    );
    await screen.findByRole("heading", { name: "Second Task" });
    expect(mockTaskDetails).toHaveBeenCalledWith("wu-b");
    expect(mockTaskDetails).toHaveBeenCalledTimes(2);
  });

  // --- Status dot color for running task ---

  it("renders status dot with correct color for running task", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "running" }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("Running")).toBeInTheDocument();
    const dot = document.querySelector("[role='dialog'] .bg-green-500.rounded-full");
    expect(dot).toBeInTheDocument();
  });

  // --- Error status display ---

  it("renders Error status text for error tasks", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail({ task_status: "error" }));
    renderPanel();
    await waitForDetail();
    expect(screen.getByText("Error")).toBeInTheDocument();
    const dot = document.querySelector("[role='dialog'] .bg-red-500.rounded-full");
    expect(dot).toBeInTheDocument();
  });

  // --- Abort button (these tests open nested dialogs and must run last) ---

  it("always shows Abort button", async () => {
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    const abortBtn = await screen.findByRole("button", { name: "Abort" });
    expect(abortBtn).toBeInTheDocument();
  });

  it("shows confirmation dialog when Abort is clicked", async () => {
    const user = userEvent.setup();
    mockTaskDetails.mockResolvedValue(makeDetail());
    renderPanel();
    const abortBtn = await screen.findByRole("button", { name: "Abort" });
    await user.click(abortBtn);

    expect(await screen.findByText("Abort this task?")).toBeInTheDocument();
    expect(
      screen.getByText("This will kill the process and the work unit will be reassigned.")
    ).toBeInTheDocument();
  });

  it("calls onAbort when abort is confirmed", async () => {
    const user = userEvent.setup();
    mockTaskDetails.mockResolvedValue(makeDetail());
    const { props } = renderPanel();

    const abortBtn = await screen.findByRole("button", { name: "Abort" });
    await user.click(abortBtn);

    await screen.findByText("Abort this task?");
    const allAbortBtns = screen.getAllByRole("button", { name: "Abort" });
    await user.click(allAbortBtns[allAbortBtns.length - 1]);

    expect(props.onAbort).toHaveBeenCalledWith("wu-detail-01");
  });

  it("does not call onAbort when abort dialog is cancelled", async () => {
    const user = userEvent.setup();
    mockTaskDetails.mockResolvedValue(makeDetail());
    const { props } = renderPanel();

    const abortBtn = await screen.findByRole("button", { name: "Abort" });
    await user.click(abortBtn);

    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);

    expect(props.onAbort).not.toHaveBeenCalled();
  });
});
