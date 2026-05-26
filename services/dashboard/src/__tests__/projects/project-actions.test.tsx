import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { ProjectActions } from "@/components/projects/project-actions";

// --- Mocks ---

const mockRefresh = jest.fn();
jest.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: mockRefresh }),
}));

const mockPauseLeaf = jest.fn().mockResolvedValue({ data: {} });
const mockResumeLeaf = jest.fn().mockResolvedValue({ data: {} });
const mockArchiveLeaf = jest.fn().mockResolvedValue({ data: {} });
jest.mock("@/lib/actions/projects", () => ({
  pauseLeaf: (...args: unknown[]) => mockPauseLeaf(...args),
  resumeLeaf: (...args: unknown[]) => mockResumeLeaf(...args),
  archiveLeaf: (...args: unknown[]) => mockArchiveLeaf(...args),
}));

beforeEach(() => {
  jest.clearAllMocks();
  window.confirm = jest.fn().mockReturnValue(true);
  window.alert = jest.fn();
});

describe("ProjectActions", () => {
  it("shows pause button for ACTIVE leafs", () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="ACTIVE"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );
    expect(screen.getByTestId("pause-button")).toBeInTheDocument();
    expect(screen.queryByTestId("resume-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("archive-button")).not.toBeInTheDocument();
  });

  it("shows resume button for PAUSED leafs", () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="PAUSED"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );
    expect(screen.getByTestId("resume-button")).toBeInTheDocument();
    expect(screen.queryByTestId("pause-button")).not.toBeInTheDocument();
  });

  it("shows archive button for PAUSED leafs", () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="PAUSED"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );
    expect(screen.getByTestId("archive-button")).toBeInTheDocument();
  });

  it("shows archive button for COMPLETED leafs", () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="COMPLETED"
        leafSlug="test"
        hasCompletedWorkUnits={true}
      />,
    );
    expect(screen.getByTestId("archive-button")).toBeInTheDocument();
    expect(screen.queryByTestId("pause-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("resume-button")).not.toBeInTheDocument();
  });

  it("shows download button when validated work units exist", () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="ACTIVE"
        leafSlug="test"
        hasCompletedWorkUnits={true}
      />,
    );
    expect(screen.getByTestId("download-button")).toBeInTheDocument();
    expect(screen.getByTestId("download-button")).toHaveAttribute(
      "href",
      "/api/download/p1?format=json",
    );
  });

  it("hides download button when no validated work units", () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="ACTIVE"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );
    expect(screen.queryByTestId("download-button")).not.toBeInTheDocument();
  });

  it("shows no action buttons for DRAFT leafs", () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="DRAFT"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );
    expect(screen.queryByTestId("pause-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("resume-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("archive-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("download-button")).not.toBeInTheDocument();
  });

  it("shows no action buttons for ARCHIVED leafs", () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="ARCHIVED"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );
    expect(screen.queryByTestId("pause-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("resume-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("archive-button")).not.toBeInTheDocument();
  });

  it("calls pauseLeaf and refreshes router on pause click", async () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="ACTIVE"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );

    fireEvent.click(screen.getByTestId("pause-button"));

    expect(window.confirm).toHaveBeenCalled();
    await waitFor(() => {
      expect(mockPauseLeaf).toHaveBeenCalledWith("p1");
      expect(mockRefresh).toHaveBeenCalled();
    });
  });

  it("does not call pauseLeaf when confirm is cancelled", async () => {
    (window.confirm as jest.Mock).mockReturnValue(false);

    render(
      <ProjectActions
        leafId="p1"
        leafState="ACTIVE"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );

    fireEvent.click(screen.getByTestId("pause-button"));

    expect(window.confirm).toHaveBeenCalled();
    expect(mockPauseLeaf).not.toHaveBeenCalled();
  });

  it("calls resumeLeaf on resume click (no confirmation needed)", async () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="PAUSED"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );

    fireEvent.click(screen.getByTestId("resume-button"));

    // Resume does not call confirm
    expect(window.confirm).not.toHaveBeenCalled();
    await waitFor(() => {
      expect(mockResumeLeaf).toHaveBeenCalledWith("p1");
      expect(mockRefresh).toHaveBeenCalled();
    });
  });

  it("calls archiveLeaf with confirmation on archive click", async () => {
    render(
      <ProjectActions
        leafId="p1"
        leafState="PAUSED"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );

    fireEvent.click(screen.getByTestId("archive-button"));

    expect(window.confirm).toHaveBeenCalled();
    await waitFor(() => {
      expect(mockArchiveLeaf).toHaveBeenCalledWith("p1");
      expect(mockRefresh).toHaveBeenCalled();
    });
  });

  it("does not call archiveLeaf when confirm is cancelled", async () => {
    (window.confirm as jest.Mock).mockReturnValue(false);

    render(
      <ProjectActions
        leafId="p1"
        leafState="PAUSED"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );

    fireEvent.click(screen.getByTestId("archive-button"));

    expect(window.confirm).toHaveBeenCalled();
    expect(mockArchiveLeaf).not.toHaveBeenCalled();
  });

  it("shows loading text during pause action", async () => {
    let resolveAction!: (value: unknown) => void;
    mockPauseLeaf.mockReturnValue(
      new Promise((resolve) => { resolveAction = resolve; }),
    );

    render(
      <ProjectActions
        leafId="p1"
        leafState="ACTIVE"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );

    fireEvent.click(screen.getByTestId("pause-button"));

    // Button should show loading text
    await waitFor(() => {
      expect(screen.getByTestId("pause-button")).toHaveTextContent("Pausing...");
    });

    // Resolve the action
    resolveAction({ data: {} });

    await waitFor(() => {
      expect(mockRefresh).toHaveBeenCalled();
    });
  });

  it("shows alert on action error", async () => {
    mockPauseLeaf.mockResolvedValue({
      error: { code: "FORBIDDEN", message: "Not allowed" },
    });

    render(
      <ProjectActions
        leafId="p1"
        leafState="ACTIVE"
        leafSlug="test"
        hasCompletedWorkUnits={false}
      />,
    );

    fireEvent.click(screen.getByTestId("pause-button"));

    await waitFor(() => {
      expect(window.alert).toHaveBeenCalledWith("Not allowed");
    });

    // Router should NOT refresh on error
    expect(mockRefresh).not.toHaveBeenCalled();
  });

  it("disables all buttons while an action is loading", async () => {
    let resolveAction!: (value: unknown) => void;
    mockPauseLeaf.mockReturnValue(
      new Promise((resolve) => { resolveAction = resolve; }),
    );

    render(
      <ProjectActions
        leafId="p1"
        leafState="ACTIVE"
        leafSlug="test"
        hasCompletedWorkUnits={true}
      />,
    );

    fireEvent.click(screen.getByTestId("pause-button"));

    // Pause button should be disabled during loading
    await waitFor(() => {
      expect(screen.getByTestId("pause-button")).toBeDisabled();
    });

    resolveAction({ data: {} });
  });
});
