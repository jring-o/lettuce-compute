import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { TaskContextMenu, type TaskActions } from "./task-context-menu";
import { makeTask, makeActions } from "./test-helpers";

function renderMenu(task: ActiveTaskInfo, actions: TaskActions, open = true) {
  return render(
    <TaskContextMenu
      task={task}
      actions={actions}
      open={open}
      onOpenChange={vi.fn()}
      trigger={<button>Trigger</button>}
    />
  );
}

describe("TaskContextMenu", () => {
  let actions: TaskActions;

  beforeEach(() => {
    actions = makeActions();
  });

  // --- Menu item visibility based on task status ---

  it("shows Suspend item when task is running", async () => {
    const task = makeTask({
      work_unit_id: "wu-run-01",
      leaf_name: "Running Leaf",
      task_status: "running",
    });
    renderMenu(task, actions);
    expect(await screen.findByText("Suspend")).toBeInTheDocument();
  });

  it("does not show Resume when task is running", async () => {
    const task = makeTask({
      work_unit_id: "wu-run-02",
      leaf_name: "Running Leaf",
      task_status: "running",
    });
    renderMenu(task, actions);
    // Wait for menu content to appear
    await screen.findByText("Suspend");
    expect(screen.queryByText("Resume")).not.toBeInTheDocument();
  });

  it("shows Resume item when task is suspended_user", async () => {
    const task = makeTask({
      work_unit_id: "wu-sus-01",
      leaf_name: "Suspended Leaf",
      task_status: "suspended_user",
    });
    renderMenu(task, actions);
    expect(await screen.findByText("Resume")).toBeInTheDocument();
  });

  it("does not show Suspend when task is suspended_user", async () => {
    const task = makeTask({
      work_unit_id: "wu-sus-02",
      leaf_name: "Suspended Leaf",
      task_status: "suspended_user",
    });
    renderMenu(task, actions);
    await screen.findByText("Resume");
    expect(screen.queryByText("Suspend")).not.toBeInTheDocument();
  });

  it("shows Resume item when task is suspended_thermal", async () => {
    const task = makeTask({
      work_unit_id: "wu-thm-01",
      leaf_name: "Thermal Leaf",
      task_status: "suspended_thermal",
    });
    renderMenu(task, actions);
    expect(await screen.findByText("Resume")).toBeInTheDocument();
  });

  it("shows Resume item when task is suspended_scheduled", async () => {
    const task = makeTask({
      work_unit_id: "wu-sch-01",
      leaf_name: "Scheduled Leaf",
      task_status: "suspended_scheduled",
    });
    renderMenu(task, actions);
    expect(await screen.findByText("Resume")).toBeInTheDocument();
  });

  it("shows neither Suspend nor Resume when task is in error state", async () => {
    const task = makeTask({
      work_unit_id: "wu-err-01",
      leaf_name: "Error Leaf",
      task_status: "error",
    });
    renderMenu(task, actions);
    // Abort is always present
    await screen.findByRole("menuitem", { name: "Abort" });
    expect(screen.queryByText("Suspend")).not.toBeInTheDocument();
    expect(screen.queryByText("Resume")).not.toBeInTheDocument();
  });

  // --- Always-present items ---

  it("always shows Abort item", async () => {
    const task = makeTask({
      work_unit_id: "wu-abort-01",
      leaf_name: "Any Leaf",
      task_status: "running",
    });
    renderMenu(task, actions);
    expect(await screen.findByRole("menuitem", { name: "Abort" })).toBeInTheDocument();
  });

  it("always shows Show Details item", async () => {
    const task = makeTask({
      work_unit_id: "wu-details-01",
      leaf_name: "Any Leaf",
      task_status: "running",
    });
    renderMenu(task, actions);
    expect(await screen.findByText("Show Details")).toBeInTheDocument();
  });

  it("always shows Copy Work Unit ID item", async () => {
    const task = makeTask({
      work_unit_id: "wu-copyid-01",
      leaf_name: "Any Leaf",
      task_status: "running",
    });
    renderMenu(task, actions);
    expect(await screen.findByText("Copy Work Unit ID")).toBeInTheDocument();
  });

  // --- Action callbacks ---

  it("calls onSuspend with work_unit_id when Suspend is clicked", async () => {
    const user = userEvent.setup();
    const task = makeTask({
      work_unit_id: "wu-suspend-cb-01",
      leaf_name: "Suspendable",
      task_status: "running",
    });
    renderMenu(task, actions);

    const item = await screen.findByText("Suspend");
    await user.click(item);
    expect(actions.onSuspend).toHaveBeenCalledWith("wu-suspend-cb-01");
  });

  it("calls onResume with work_unit_id when Resume is clicked", async () => {
    const user = userEvent.setup();
    const task = makeTask({
      work_unit_id: "wu-resume-cb-01",
      leaf_name: "Resumable",
      task_status: "suspended_user",
    });
    renderMenu(task, actions);

    const item = await screen.findByText("Resume");
    await user.click(item);
    expect(actions.onResume).toHaveBeenCalledWith("wu-resume-cb-01");
  });

  it("opens abort confirmation dialog when Abort is clicked", async () => {
    const user = userEvent.setup();
    const task = makeTask({
      work_unit_id: "wu-abort-cb-01",
      leaf_name: "Abortable",
      task_status: "running",
    });
    renderMenu(task, actions);

    const item = await screen.findByRole("menuitem", { name: "Abort" });
    await user.click(item);

    // Confirm dialog should appear
    expect(await screen.findByText("Abort this task?")).toBeInTheDocument();
    expect(
      screen.getByText("This will kill the process and the work unit will be reassigned.")
    ).toBeInTheDocument();
  });

  it("calls onAbort when abort is confirmed in the dialog", async () => {
    const user = userEvent.setup();
    const task = makeTask({
      work_unit_id: "wu-abort-confirm-01",
      leaf_name: "Confirm Abort",
      task_status: "running",
    });
    renderMenu(task, actions);

    // Click Abort menu item to open dialog
    const item = await screen.findByRole("menuitem", { name: "Abort" });
    await user.click(item);

    // Confirm the dialog
    const confirmButton = await screen.findByRole("button", { name: "Abort" });
    await user.click(confirmButton);

    expect(actions.onAbort).toHaveBeenCalledWith("wu-abort-confirm-01");
  });

  it("does not call onAbort when abort dialog is cancelled", async () => {
    const user = userEvent.setup();
    const task = makeTask({
      work_unit_id: "wu-abort-cancel-01",
      leaf_name: "Cancel Abort",
      task_status: "running",
    });
    renderMenu(task, actions);

    // Click Abort menu item to open dialog
    const item = await screen.findByRole("menuitem", { name: "Abort" });
    await user.click(item);

    // Cancel the dialog
    const cancelButton = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelButton);

    expect(actions.onAbort).not.toHaveBeenCalled();
  });

  it("calls onShowDetails with the task when Show Details is clicked", async () => {
    const user = userEvent.setup();
    const task = makeTask({
      work_unit_id: "wu-details-cb-01",
      leaf_name: "Detail Task",
      task_status: "running",
    });
    renderMenu(task, actions);

    const item = await screen.findByText("Show Details");
    await user.click(item);
    expect(actions.onShowDetails).toHaveBeenCalledWith(task);
  });

  it("calls onCopyId with work_unit_id when Copy Work Unit ID is clicked", async () => {
    const user = userEvent.setup();
    const task = makeTask({
      work_unit_id: "wu-copy-cb-01",
      leaf_name: "Copy Task",
      task_status: "running",
    });
    renderMenu(task, actions);

    const item = await screen.findByText("Copy Work Unit ID");
    await user.click(item);
    expect(actions.onCopyId).toHaveBeenCalledWith("wu-copy-cb-01");
  });

  // --- Controlled open state ---

  it("does not render menu content when open is false", () => {
    const task = makeTask({
      work_unit_id: "wu-closed-01",
      leaf_name: "Closed Menu",
      task_status: "running",
    });
    renderMenu(task, actions, false);
    expect(screen.queryByText("Suspend")).not.toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Abort" })).not.toBeInTheDocument();
  });
});
