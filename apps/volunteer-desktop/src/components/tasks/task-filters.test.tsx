import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  applyTaskFilters,
  isQueuedTask,
  TaskFilters,
  type TaskFilterState,
} from "./task-filters";
import { makeTask } from "./test-helpers";
import type { ActiveTaskInfo, QueuedTaskInfo } from "@/api/client";

// --- Helper: build a QueuedTaskInfo ---

function makeQueued(
  overrides: Partial<QueuedTaskInfo> & { work_unit_id: string; leaf_name: string }
): QueuedTaskInfo {
  return {
    deadline_seconds: 86400,
    fetched_at: "2026-03-29T00:00:00Z",
    server_name: "lettuce.science",
    ...overrides,
  };
}

// --- Default filter state helpers ---

const DEFAULT_FILTERS: TaskFilterState = {
  leafName: null,
  status: "all",
  showQueued: false,
};

// ============================================================
// applyTaskFilters — pure logic
// ============================================================

describe("applyTaskFilters", () => {
  const tasks: ActiveTaskInfo[] = [
    makeTask({ work_unit_id: "wu-r1", leaf_name: "Alpha", task_status: "running" }),
    makeTask({ work_unit_id: "wu-r2", leaf_name: "Beta", task_status: "running" }),
    makeTask({ work_unit_id: "wu-su", leaf_name: "Alpha", task_status: "suspended_user" }),
    makeTask({ work_unit_id: "wu-st", leaf_name: "Beta", task_status: "suspended_thermal" }),
    makeTask({ work_unit_id: "wu-ss", leaf_name: "Gamma", task_status: "suspended_scheduled" }),
    makeTask({ work_unit_id: "wu-e1", leaf_name: "Gamma", task_status: "error" }),
  ];

  const queued: QueuedTaskInfo[] = [
    makeQueued({ work_unit_id: "wu-q1", leaf_name: "Alpha" }),
    makeQueued({ work_unit_id: "wu-q2", leaf_name: "Beta" }),
  ];

  it("returns all tasks unchanged when no filters are set", () => {
    const result = applyTaskFilters(tasks, queued, DEFAULT_FILTERS);
    expect(result).toEqual(tasks);
  });

  it("filters by leafName", () => {
    const result = applyTaskFilters(tasks, queued, {
      ...DEFAULT_FILTERS,
      leafName: "Alpha",
    });
    expect(result).toHaveLength(2);
    expect(result.every((t) => t.leaf_name === "Alpha")).toBe(true);
  });

  it("filters by status 'running' — only running tasks", () => {
    const result = applyTaskFilters(tasks, queued, {
      ...DEFAULT_FILTERS,
      status: "running",
    });
    expect(result).toHaveLength(2);
    expect(result.every((t) => t.task_status === "running")).toBe(true);
  });

  it("filters by status 'suspended' — matches all suspended variants", () => {
    const result = applyTaskFilters(tasks, queued, {
      ...DEFAULT_FILTERS,
      status: "suspended",
    });
    expect(result).toHaveLength(3);
    expect(result.every((t) => t.task_status.startsWith("suspended"))).toBe(true);
    const statuses = result.map((t) => t.task_status);
    expect(statuses).toContain("suspended_user");
    expect(statuses).toContain("suspended_thermal");
    expect(statuses).toContain("suspended_scheduled");
  });

  it("filters by status 'error' — only error tasks", () => {
    const result = applyTaskFilters(tasks, queued, {
      ...DEFAULT_FILTERS,
      status: "error",
    });
    expect(result).toHaveLength(1);
    expect(result[0].task_status).toBe("error");
  });

  it("appends queued tasks when showQueued is true", () => {
    const result = applyTaskFilters(tasks, queued, {
      ...DEFAULT_FILTERS,
      showQueued: true,
    });
    // 6 active + 2 queued = 8
    expect(result).toHaveLength(8);
    // Last two should be the queued tasks
    const last2 = result.slice(-2);
    expect(last2.every((t) => (t.task_status as string) === "queued")).toBe(true);
  });

  it("applies leafName filter to queued tasks when showQueued is true", () => {
    const result = applyTaskFilters(tasks, queued, {
      leafName: "Alpha",
      status: "all",
      showQueued: true,
    });
    // 2 active Alpha + 1 queued Alpha = 3
    expect(result).toHaveLength(3);
    expect(result.every((t) => t.leaf_name === "Alpha")).toBe(true);
  });

  it("combines leafName and status filters", () => {
    const result = applyTaskFilters(tasks, queued, {
      leafName: "Alpha",
      status: "running",
      showQueued: false,
    });
    expect(result).toHaveLength(1);
    expect(result[0].work_unit_id).toBe("wu-r1");
  });

  it("returns empty array when no tasks match combined filters", () => {
    const result = applyTaskFilters(tasks, queued, {
      leafName: "Alpha",
      status: "error",
      showQueued: false,
    });
    expect(result).toHaveLength(0);
  });

  it("queued tasks have task_status 'queued' and zero progress", () => {
    const result = applyTaskFilters([], queued, {
      ...DEFAULT_FILTERS,
      showQueued: true,
    });
    expect(result).toHaveLength(2);
    for (const t of result) {
      expect((t.task_status as string)).toBe("queued");
      expect(t.progress_pct).toBe(0);
      expect(t.cpu_seconds).toBe(0);
    }
  });

  it("returns only active tasks when showQueued is true but queued array is empty", () => {
    const result = applyTaskFilters(tasks, [], {
      ...DEFAULT_FILTERS,
      showQueued: true,
    });
    expect(result).toHaveLength(6); // only the 6 active tasks
    expect(result).toEqual(tasks);
  });

  it("appends queued tasks even when status filter is 'running'", () => {
    // This documents current behavior: status filter does NOT exclude injected queued tasks
    const result = applyTaskFilters(tasks, queued, {
      leafName: null,
      status: "running",
      showQueued: true,
    });
    // 2 running active + 2 queued (queued are appended regardless of status filter)
    expect(result).toHaveLength(4);
    const statuses = result.map((t) => t.task_status as string);
    expect(statuses.filter((s) => s === "running")).toHaveLength(2);
    expect(statuses.filter((s) => s === "queued")).toHaveLength(2);
  });

  it("handles empty tasks array with no filters", () => {
    const result = applyTaskFilters([], [], DEFAULT_FILTERS);
    expect(result).toHaveLength(0);
  });

  it("handles empty tasks array with showQueued and queued tasks present", () => {
    const result = applyTaskFilters([], queued, {
      ...DEFAULT_FILTERS,
      showQueued: true,
    });
    expect(result).toHaveLength(2);
    expect(result.every((t) => (t.task_status as string) === "queued")).toBe(true);
  });
});

// ============================================================
// isQueuedTask
// ============================================================

describe("isQueuedTask", () => {
  it("returns true for tasks with task_status 'queued'", () => {
    const queued = makeTask({
      work_unit_id: "wu-q",
      leaf_name: "Q",
      task_status: "queued" as unknown as ActiveTaskInfo["task_status"],
    });
    expect(isQueuedTask(queued)).toBe(true);
  });

  it("returns false for running tasks", () => {
    const task = makeTask({ work_unit_id: "wu-1", leaf_name: "A", task_status: "running" });
    expect(isQueuedTask(task)).toBe(false);
  });

  it("returns false for suspended tasks", () => {
    const task = makeTask({ work_unit_id: "wu-2", leaf_name: "B", task_status: "suspended_user" });
    expect(isQueuedTask(task)).toBe(false);
  });

  it("returns false for error tasks", () => {
    const task = makeTask({ work_unit_id: "wu-3", leaf_name: "C", task_status: "error" });
    expect(isQueuedTask(task)).toBe(false);
  });
});

// ============================================================
// TaskFilters component
// ============================================================

describe("TaskFilters component", () => {
  const onChange = vi.fn();

  const fourTasks: ActiveTaskInfo[] = [
    makeTask({ work_unit_id: "wu-1", leaf_name: "Beta", task_status: "running" }),
    makeTask({ work_unit_id: "wu-2", leaf_name: "Alpha", task_status: "running" }),
    makeTask({ work_unit_id: "wu-3", leaf_name: "Alpha", task_status: "suspended_user" }),
    makeTask({ work_unit_id: "wu-4", leaf_name: "Gamma", task_status: "error" }),
  ];

  const noQueued: QueuedTaskInfo[] = [];

  beforeEach(() => {
    onChange.mockClear();
  });

  it("returns null when fewer than 4 tasks", () => {
    const threeTasks = fourTasks.slice(0, 3);
    const { container } = render(
      <TaskFilters
        tasks={threeTasks}
        filters={DEFAULT_FILTERS}
        onFiltersChange={onChange}
      />
    );
    expect(container.innerHTML).toBe("");
  });

  it("renders when exactly 4 tasks", () => {
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={DEFAULT_FILTERS}
        onFiltersChange={onChange}
      />
    );
    // Should have the leaf dropdown
    expect(screen.getByText("All Leafs")).toBeInTheDocument();
  });

  it("renders leaf dropdown with sorted unique leaf names", () => {
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={DEFAULT_FILTERS}
        onFiltersChange={onChange}
      />
    );

    const selects = screen.getAllByRole("combobox");
    const leafSelect = selects[0];
    const options = leafSelect.querySelectorAll("option");

    // "All Leafs" + Alpha, Beta, Gamma (sorted)
    expect(options).toHaveLength(4);
    expect(options[0].textContent).toBe("All Leafs");
    expect(options[1].textContent).toBe("Alpha");
    expect(options[2].textContent).toBe("Beta");
    expect(options[3].textContent).toBe("Gamma");
  });

  it("renders status dropdown with all options", () => {
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={DEFAULT_FILTERS}
        onFiltersChange={onChange}
      />
    );

    const selects = screen.getAllByRole("combobox");
    const statusSelect = selects[1];
    const options = statusSelect.querySelectorAll("option");

    expect(options).toHaveLength(4);
    expect(options[0].textContent).toBe("All");
    expect(options[1].textContent).toBe("Running");
    expect(options[2].textContent).toBe("Suspended");
    expect(options[3].textContent).toBe("Error");
  });

  it("renders Active and All toggle buttons", () => {
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={DEFAULT_FILTERS}
        onFiltersChange={onChange}
      />
    );

    expect(screen.getByRole("button", { name: "Active" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "All" })).toBeInTheDocument();
  });

  it("calls onFiltersChange with leafName when leaf is selected", async () => {
    const user = userEvent.setup();
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={DEFAULT_FILTERS}
        onFiltersChange={onChange}
      />
    );

    const selects = screen.getAllByRole("combobox");
    await user.selectOptions(selects[0], "Alpha");

    expect(onChange).toHaveBeenCalledWith({
      ...DEFAULT_FILTERS,
      leafName: "Alpha",
    });
  });

  it("calls onFiltersChange with null leafName when 'All Leafs' is selected", async () => {
    const user = userEvent.setup();
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={{ ...DEFAULT_FILTERS, leafName: "Alpha" }}
        onFiltersChange={onChange}
      />
    );

    const selects = screen.getAllByRole("combobox");
    await user.selectOptions(selects[0], "");

    expect(onChange).toHaveBeenCalledWith({
      ...DEFAULT_FILTERS,
      leafName: null,
    });
  });

  it("calls onFiltersChange with status when status is selected", async () => {
    const user = userEvent.setup();
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={DEFAULT_FILTERS}
        onFiltersChange={onChange}
      />
    );

    const selects = screen.getAllByRole("combobox");
    await user.selectOptions(selects[1], "running");

    expect(onChange).toHaveBeenCalledWith({
      ...DEFAULT_FILTERS,
      status: "running",
    });
  });

  it("calls onFiltersChange with showQueued=true when All toggle is clicked", async () => {
    const user = userEvent.setup();
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={DEFAULT_FILTERS}
        onFiltersChange={onChange}
      />
    );

    // "All" button is the second toggle
    const allButton = screen.getByRole("button", { name: "All" });
    await user.click(allButton);

    expect(onChange).toHaveBeenCalledWith({
      ...DEFAULT_FILTERS,
      showQueued: true,
    });
  });

  it("calls onFiltersChange with showQueued=false when Active toggle is clicked", async () => {
    const user = userEvent.setup();
    render(
      <TaskFilters
        tasks={fourTasks}
        filters={{ ...DEFAULT_FILTERS, showQueued: true }}
        onFiltersChange={onChange}
      />
    );

    const activeButton = screen.getByRole("button", { name: "Active" });
    await user.click(activeButton);

    expect(onChange).toHaveBeenCalledWith({
      ...DEFAULT_FILTERS,
      showQueued: false,
    });
  });
});
