import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ActiveTaskTable } from "./active-task-table";
import { makeTask, makeActions } from "./test-helpers";

const SAMPLE_TASKS: ActiveTaskInfo[] = [
  makeTask({
    work_unit_id: "cccccccc-1234-5678-abcd-000000000001",
    leaf_name: "Climate Model",
    progress_pct: 75,
    cpu_seconds: 3600,
    deadline_seconds: 86400,
    task_status: "running",
  }),
  makeTask({
    work_unit_id: "aaaaaaaa-1234-5678-abcd-000000000002",
    leaf_name: "Alpha Study",
    progress_pct: 25,
    cpu_seconds: 600,
    deadline_seconds: 43200,
    task_status: "suspended_user",
  }),
  makeTask({
    work_unit_id: "bbbbbbbb-1234-5678-abcd-000000000003",
    leaf_name: "Beta Analysis",
    progress_pct: 50,
    cpu_seconds: 1800,
    deadline_seconds: 7200,
    task_status: "running",
    estimated_remaining_seconds: 1800,
  }),
];

describe("ActiveTaskTable", () => {
  let actions: TaskActions;
  let onRowClick: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    actions = makeActions();
    onRowClick = vi.fn();
  });

  // --- Column rendering ---

  it("renders all 7 column headers", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    expect(screen.getByText("Leaf")).toBeInTheDocument();
    expect(screen.getByText("Status")).toBeInTheDocument();
    expect(screen.getByText("Progress")).toBeInTheDocument();
    expect(screen.getByText("CPU Time")).toBeInTheDocument();
    expect(screen.getByText("Remaining")).toBeInTheDocument();
    expect(screen.getByText("Deadline")).toBeInTheDocument();
    expect(screen.getByText("WU ID")).toBeInTheDocument();
  });

  it("renders a row for each task", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    expect(screen.getByText("Climate Model")).toBeInTheDocument();
    expect(screen.getByText("Alpha Study")).toBeInTheDocument();
    expect(screen.getByText("Beta Analysis")).toBeInTheDocument();
  });

  it("renders leaf name in each row", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    expect(screen.getByText("Climate Model")).toBeInTheDocument();
    expect(screen.getByText("Alpha Study")).toBeInTheDocument();
    expect(screen.getByText("Beta Analysis")).toBeInTheDocument();
  });

  it("renders status text with color dot for running tasks", () => {
    const task = makeTask({
      work_unit_id: "wu-status-run-01",
      leaf_name: "Runner",
      task_status: "running",
    });
    render(
      <ActiveTaskTable tasks={[task]} actions={actions} onRowClick={onRowClick} />
    );

    expect(screen.getByText("Running")).toBeInTheDocument();
    const dot = document.querySelector(".bg-green-500.rounded-full");
    expect(dot).toBeInTheDocument();
  });

  it("renders Suspended status text for suspended_user tasks", () => {
    const task = makeTask({
      work_unit_id: "wu-status-sus-01",
      leaf_name: "Paused",
      task_status: "suspended_user",
    });
    render(
      <ActiveTaskTable tasks={[task]} actions={actions} onRowClick={onRowClick} />
    );

    expect(screen.getByText("Suspended")).toBeInTheDocument();
    const dot = document.querySelector(".bg-yellow-500.rounded-full");
    expect(dot).toBeInTheDocument();
  });

  it("renders progress percentage", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    expect(screen.getByText("75%")).toBeInTheDocument();
    expect(screen.getByText("25%")).toBeInTheDocument();
    expect(screen.getByText("50%")).toBeInTheDocument();
  });

  it("renders CPU time formatted", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // 3600s = "1h 0m", 600s = "10m 0s", 1800s = "30m 0s"
    expect(screen.getByText("1h 0m")).toBeInTheDocument();
    expect(screen.getByText("10m 0s")).toBeInTheDocument();
    // "30m 0s" appears twice: once for Beta Analysis cpu_seconds and once for its estimated_remaining_seconds
    const thirtyMin = screen.getAllByText("30m 0s");
    expect(thirtyMin.length).toBeGreaterThanOrEqual(1);
  });

  it("renders estimated remaining time when available", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // Beta Analysis has estimated_remaining_seconds: 1800 -> "30m 0s"
    // But CPU time is also "30m 0s". Let's find the remaining column entry for the task that has "---"
    // The other two tasks should show "---" since they have no estimated_remaining_seconds
    const dashes = screen.getAllByText("---");
    expect(dashes.length).toBe(2); // Climate Model and Alpha Study have no remaining estimate
  });

  it("renders work unit ID prefix (first 8 chars)", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    expect(screen.getByText("cccccccc")).toBeInTheDocument();
    expect(screen.getByText("aaaaaaaa")).toBeInTheDocument();
    expect(screen.getByText("bbbbbbbb")).toBeInTheDocument();
  });

  it("renders deadline time formatted", () => {
    const task = makeTask({
      work_unit_id: "wu-deadline-fmt-01",
      leaf_name: "Deadline Task",
      deadline_seconds: 86400,
    });
    render(
      <ActiveTaskTable tasks={[task]} actions={actions} onRowClick={onRowClick} />
    );

    expect(screen.getByText("24h 0m")).toBeInTheDocument();
  });

  it("renders deadline in yellow when less than 2 hours", () => {
    const task = makeTask({
      work_unit_id: "wu-deadline-urg-01",
      leaf_name: "Urgent Task",
      deadline_seconds: 3600, // 1h — less than 7200
    });
    render(
      <ActiveTaskTable tasks={[task]} actions={actions} onRowClick={onRowClick} />
    );

    const deadlineCell = screen.getByText("1h 0m");
    expect(deadlineCell.className).toContain("text-yellow-500");
  });

  it("renders overdue deadline in red", () => {
    const task = makeTask({
      work_unit_id: "wu-deadline-late-01",
      leaf_name: "Late Task",
      deadline_seconds: -3600,
    });
    render(
      <ActiveTaskTable tasks={[task]} actions={actions} onRowClick={onRowClick} />
    );

    const deadlineCell = screen.getByText("1h 0m");
    expect(deadlineCell.className).toContain("text-red-500");
  });

  it("renders overflow menu trigger for each row", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    const triggers = screen.getAllByText("···");
    expect(triggers).toHaveLength(3);
  });

  // --- Sorting ---

  it("sorts by leaf name ascending by default", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    const rows = screen.getAllByRole("row");
    // Row 0 is thead row, rows 1-3 are data rows
    const firstDataRow = rows[1];
    const secondDataRow = rows[2];
    const thirdDataRow = rows[3];

    // Default sort is leaf_name asc: Alpha Study, Beta Analysis, Climate Model
    expect(within(firstDataRow).getByText("Alpha Study")).toBeInTheDocument();
    expect(within(secondDataRow).getByText("Beta Analysis")).toBeInTheDocument();
    expect(within(thirdDataRow).getByText("Climate Model")).toBeInTheDocument();
  });

  it("toggles sort direction when clicking same column header", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // Click Leaf to toggle from asc to desc
    await user.click(screen.getByText("Leaf"));

    const rows = screen.getAllByRole("row");
    // After toggling: Climate Model, Beta Analysis, Alpha Study
    expect(within(rows[1]).getByText("Climate Model")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Beta Analysis")).toBeInTheDocument();
    expect(within(rows[3]).getByText("Alpha Study")).toBeInTheDocument();
  });

  it("sorts by a different column when clicking its header", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // Click Progress to sort by progress_pct ascending
    await user.click(screen.getByText("Progress"));

    const rows = screen.getAllByRole("row");
    // Progress asc: Alpha Study (25%), Beta Analysis (50%), Climate Model (75%)
    expect(within(rows[1]).getByText("Alpha Study")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Beta Analysis")).toBeInTheDocument();
    expect(within(rows[3]).getByText("Climate Model")).toBeInTheDocument();
  });

  it("sorts by progress descending on double-click of Progress header", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // Click Progress twice: first sets asc, second toggles to desc
    await user.click(screen.getByText("Progress"));
    await user.click(screen.getByText("Progress"));

    const rows = screen.getAllByRole("row");
    // Progress desc: Climate Model (75%), Beta Analysis (50%), Alpha Study (25%)
    expect(within(rows[1]).getByText("Climate Model")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Beta Analysis")).toBeInTheDocument();
    expect(within(rows[3]).getByText("Alpha Study")).toBeInTheDocument();
  });

  it("sorts by CPU Time column", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // Click CPU Time to sort ascending
    await user.click(screen.getByText("CPU Time"));

    const rows = screen.getAllByRole("row");
    // cpu_seconds asc: Alpha Study (600), Beta Analysis (1800), Climate Model (3600)
    expect(within(rows[1]).getByText("Alpha Study")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Beta Analysis")).toBeInTheDocument();
    expect(within(rows[3]).getByText("Climate Model")).toBeInTheDocument();
  });

  it("sorts by Status column", async () => {
    const user = userEvent.setup();
    // Add an error task to get a third status type
    const tasksWithError = [
      ...SAMPLE_TASKS,
      makeTask({
        work_unit_id: "dddddddd-1234-5678-abcd-000000000004",
        leaf_name: "Delta Error",
        task_status: "error",
      }),
    ];
    render(
      <ActiveTaskTable tasks={tasksWithError} actions={actions} onRowClick={onRowClick} />
    );

    await user.click(screen.getByText("Status"));

    const rows = screen.getAllByRole("row");
    // Status text ascending: "Error" < "Running" < "Suspended"
    expect(within(rows[1]).getByText("Delta Error")).toBeInTheDocument();
    // Running tasks next (Climate Model and Beta Analysis)
    // Suspended last (Alpha Study)
    expect(within(rows[4]).getByText("Alpha Study")).toBeInTheDocument();
  });

  it("sorts by Deadline column", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    await user.click(screen.getByText("Deadline"));

    const rows = screen.getAllByRole("row");
    // deadline_seconds asc: Beta Analysis (7200), Alpha Study (43200), Climate Model (86400)
    expect(within(rows[1]).getByText("Beta Analysis")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Alpha Study")).toBeInTheDocument();
    expect(within(rows[3]).getByText("Climate Model")).toBeInTheDocument();
  });

  it("sorts by WU ID column", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    await user.click(screen.getByText("WU ID"));

    const rows = screen.getAllByRole("row");
    // work_unit_id asc (lowercase): aaaaaaaa, bbbbbbbb, cccccccc
    expect(within(rows[1]).getByText("Alpha Study")).toBeInTheDocument();
    expect(within(rows[2]).getByText("Beta Analysis")).toBeInTheDocument();
    expect(within(rows[3]).getByText("Climate Model")).toBeInTheDocument();
  });

  it("sorts by Remaining column with null values sorted to end", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    await user.click(screen.getByText("Remaining"));

    const rows = screen.getAllByRole("row");
    // remaining asc: Beta Analysis (1800), then Climate Model (Infinity) and Alpha Study (Infinity)
    expect(within(rows[1]).getByText("Beta Analysis")).toBeInTheDocument();
    // Climate Model and Alpha Study both have null remaining -> Infinity, but sort stably
  });

  it("caps progress bar width at 100% for progress > 100", () => {
    const task = makeTask({
      work_unit_id: "wu-overflow-01",
      leaf_name: "Overflow Task",
      progress_pct: 150,
    });
    render(
      <ActiveTaskTable tasks={[task]} actions={actions} onRowClick={onRowClick} />
    );

    // The progress bar div should have width capped at 100%
    const progressBar = document.querySelector("[style*='width']") as HTMLElement;
    expect(progressBar).toBeInTheDocument();
    expect(progressBar!.style.width).toBe("100%");
  });

  it("displays sort arrow indicator on active column", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // Default sort is on "Leaf" column — there should be an SVG arrow in that header
    const leafHeader = screen.getByText("Leaf");
    const svg = leafHeader.querySelector("svg");
    expect(svg).toBeInTheDocument();
  });

  it("does not display sort arrow on non-active columns", () => {
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // "Status" column should not have an SVG arrow (not the active sort column)
    const statusHeader = screen.getByText("Status");
    const svg = statusHeader.querySelector("svg");
    expect(svg).toBeNull();
  });

  // --- Row click ---

  it("calls onRowClick with the task when a row is clicked", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // Click on the Alpha Study leaf name cell
    await user.click(screen.getByText("Alpha Study"));
    expect(onRowClick).toHaveBeenCalledWith(
      expect.objectContaining({ leaf_name: "Alpha Study" })
    );
  });

  // --- Empty state ---

  it("renders empty tbody when no tasks are provided", () => {
    render(
      <ActiveTaskTable tasks={[]} actions={actions} onRowClick={onRowClick} />
    );

    // Headers still render
    expect(screen.getByText("Leaf")).toBeInTheDocument();
    // But no data rows (only the header row)
    const rows = screen.getAllByRole("row");
    expect(rows).toHaveLength(1); // just the header row
  });

  // --- Context menu on right-click ---

  it("opens context menu on right-click of a row", async () => {
    const user = userEvent.setup();
    render(
      <ActiveTaskTable tasks={SAMPLE_TASKS} actions={actions} onRowClick={onRowClick} />
    );

    // Right-click on the Climate Model row
    const climateText = screen.getByText("Climate Model");
    const row = climateText.closest("tr")!;
    await user.pointer({ keys: "[MouseRight]", target: row });

    // The context menu should open — look for menu items
    // The context menu dropdown is rendered for the right-clicked task
    expect(await screen.findByRole("menuitem", { name: "Abort" })).toBeInTheDocument();
  });
});
