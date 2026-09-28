import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RestartRunningTasksNote } from "./restart-running-tasks-note";

const mockStatus = vi.fn();
vi.mock("@/hooks/use-daemon-status", () => ({
  useDaemonStatus: () => mockStatus(),
}));
const restartTask = vi.fn(() => Promise.resolve());
vi.mock("@/hooks/use-api", () => ({
  useClient: () => ({ client: { restartTask }, error: null }),
}));

function running(...ids: string[]) {
  return { status: { active_tasks: ids.map((id) => ({ work_unit_id: id })) } };
}

describe("RestartRunningTasksNote", () => {
  beforeEach(() => vi.clearAllMocks());

  it("says a saved CPU setting applies to new tasks and restarts the running ones on request", async () => {
    const user = userEvent.setup();
    mockStatus.mockReturnValue(running("wu-1", "wu-2"));
    render(<RestartRunningTasksNote show />);
    expect(screen.getByText(/applies to tasks started from now on/)).toBeInTheDocument();
    expect(screen.getByText(/The 2 running tasks keep the cores and settings they started with/)).toBeInTheDocument();
    await user.click(screen.getByText("Restart them with the new settings…"));
    expect(screen.getByText("Restart the 2 running tasks?")).toBeInTheDocument();
    expect(screen.getByText(/lost unless its leaf saves checkpoints/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Restart" }));
    await waitFor(() => expect(restartTask).toHaveBeenCalledTimes(2));
    expect(restartTask).toHaveBeenCalledWith("wu-1");
    expect(restartTask).toHaveBeenCalledWith("wu-2");
    expect(await screen.findByText(/Restarting 2 running tasks with the new settings/)).toBeInTheDocument();
  });

  it("shows nothing before a setting is saved, or while no task runs", () => {
    mockStatus.mockReturnValue(running("wu-1"));
    const { container, rerender } = render(<RestartRunningTasksNote show={false} />);
    expect(container.firstChild).toBeNull();
    mockStatus.mockReturnValue(running());
    rerender(<RestartRunningTasksNote show />);
    expect(container.firstChild).toBeNull();
  });
});
