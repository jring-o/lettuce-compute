import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { VolunteerLeafCard, BrowseLeafCard } from "./project-card";

describe("VolunteerLeafCard", () => {
  const defaultProps = {
    leafName: "Climate Prediction",
    serverName: "Research Lab A",
    serverAddress: "grpc://lab-a.example.com:50051",
    status: "active" as const,
    creditEarned: 1500,
    workUnitsCompleted: 42,
  };

  it("renders leaf name and server info", () => {
    render(<VolunteerLeafCard {...defaultProps} />);
    expect(screen.getByText("Climate Prediction")).toBeInTheDocument();
    expect(
      screen.getByText("Research Lab A · grpc://lab-a.example.com:50051")
    ).toBeInTheDocument();
  });

  it("renders active status badge", () => {
    render(<VolunteerLeafCard {...defaultProps} status="active" />);
    expect(screen.getByText("Active")).toBeInTheDocument();
  });

  it("renders paused status badge", () => {
    render(<VolunteerLeafCard {...defaultProps} status="paused" />);
    expect(screen.getByText("Paused")).toBeInTheDocument();
  });

  it("renders completed status badge", () => {
    render(<VolunteerLeafCard {...defaultProps} status="completed" />);
    expect(screen.getByText("Completed")).toBeInTheDocument();
  });

  it("renders error status badge", () => {
    render(<VolunteerLeafCard {...defaultProps} status="error" />);
    expect(screen.getByText("Error")).toBeInTheDocument();
  });

  it("renders credit earned and work units", () => {
    render(<VolunteerLeafCard {...defaultProps} />);
    expect(screen.getByText("Credit earned")).toBeInTheDocument();
    expect(screen.getByText("1,500")).toBeInTheDocument();
    expect(screen.getByText("Work units")).toBeInTheDocument();
    expect(screen.getByText("42")).toBeInTheDocument();
  });

  it("renders research area when provided", () => {
    render(
      <VolunteerLeafCard {...defaultProps} researchArea="Climate Science" />
    );
    expect(screen.getByText("Climate Science")).toBeInTheDocument();
  });

  it("does not render research area when not provided", () => {
    render(<VolunteerLeafCard {...defaultProps} />);
    expect(screen.queryByText("Climate Science")).not.toBeInTheDocument();
  });

  it("does not show detach button when onDetach is not provided", () => {
    render(<VolunteerLeafCard {...defaultProps} />);
    expect(screen.queryByText("Detach")).not.toBeInTheDocument();
  });

  it("shows detach button when onDetach is provided", () => {
    const onDetach = vi.fn();
    render(<VolunteerLeafCard {...defaultProps} onDetach={onDetach} />);
    expect(screen.getByText("Detach")).toBeInTheDocument();
  });

  it("shows confirmation dialog on detach click", async () => {
    const user = userEvent.setup();
    const onDetach = vi.fn();
    render(<VolunteerLeafCard {...defaultProps} onDetach={onDetach} />);

    await user.click(screen.getByText("Detach"));

    // Should show confirmation with leaf name (appears twice: title + confirmation bold)
    expect(
      screen.getByText(/Stop contributing to/)
    ).toBeInTheDocument();
    const nameMatches = screen.getAllByText("Climate Prediction");
    expect(nameMatches.length).toBe(2); // title + <strong> in confirmation
    // Should show Detach confirm button and Cancel
    const buttons = screen.getAllByRole("button");
    const detachBtn = buttons.find((b) => b.textContent === "Detach");
    expect(detachBtn).toBeInTheDocument();
    expect(screen.getByText("Cancel")).toBeInTheDocument();
    // Should NOT have called onDetach yet
    expect(onDetach).not.toHaveBeenCalled();
  });

  it("calls onDetach when confirmation is accepted", async () => {
    const user = userEvent.setup();
    const onDetach = vi.fn();
    render(<VolunteerLeafCard {...defaultProps} onDetach={onDetach} />);

    // Click initial detach
    await user.click(screen.getByText("Detach"));
    // Click confirm detach (destructive button)
    const buttons = screen.getAllByRole("button");
    const confirmBtn = buttons.find((b) => b.textContent === "Detach");
    await user.click(confirmBtn!);

    expect(onDetach).toHaveBeenCalledOnce();
  });

  it("hides confirmation dialog when cancel is clicked", async () => {
    const user = userEvent.setup();
    const onDetach = vi.fn();
    render(<VolunteerLeafCard {...defaultProps} onDetach={onDetach} />);

    await user.click(screen.getByText("Detach"));
    expect(screen.getByText("Cancel")).toBeInTheDocument();

    await user.click(screen.getByText("Cancel"));

    // Confirmation should be gone, original Detach button should be back
    expect(screen.queryByText("Cancel")).not.toBeInTheDocument();
    expect(screen.getByText("Detach")).toBeInTheDocument();
    expect(onDetach).not.toHaveBeenCalled();
  });
});

describe("BrowseLeafCard", () => {
  const defaultProps = {
    leafName: "Protein Folding",
    description: "Simulate protein folding dynamics",
    resourceRequirements: "CPU + GPU",
    activeVolunteers: 150,
  };

  it("renders leaf name and description", () => {
    render(<BrowseLeafCard {...defaultProps} />);
    expect(screen.getByText("Protein Folding")).toBeInTheDocument();
    expect(
      screen.getByText("Simulate protein folding dynamics")
    ).toBeInTheDocument();
  });

  it("renders resource requirements", () => {
    render(<BrowseLeafCard {...defaultProps} />);
    expect(screen.getByText("CPU + GPU")).toBeInTheDocument();
  });

  it("renders active volunteers count", () => {
    render(<BrowseLeafCard {...defaultProps} />);
    expect(screen.getByText("Active volunteers")).toBeInTheDocument();
    expect(screen.getByText("150")).toBeInTheDocument();
  });

  it("renders research area when provided", () => {
    render(
      <BrowseLeafCard {...defaultProps} researchArea="Biology" />
    );
    expect(screen.getByText("Biology")).toBeInTheDocument();
  });

  it("does not render research area when not provided", () => {
    render(<BrowseLeafCard {...defaultProps} />);
    expect(screen.queryByText("Biology")).not.toBeInTheDocument();
  });

  it("renders attach button when onAttach is provided", () => {
    const onAttach = vi.fn();
    render(<BrowseLeafCard {...defaultProps} onAttach={onAttach} />);
    expect(screen.getByText("Attach")).toBeInTheDocument();
  });

  it("does not render attach button when onAttach is not provided", () => {
    render(<BrowseLeafCard {...defaultProps} />);
    expect(screen.queryByText("Attach")).not.toBeInTheDocument();
  });

  it("calls onAttach when attach button is clicked", async () => {
    const user = userEvent.setup();
    const onAttach = vi.fn();
    render(<BrowseLeafCard {...defaultProps} onAttach={onAttach} />);

    await user.click(screen.getByText("Attach"));
    expect(onAttach).toHaveBeenCalledOnce();
  });

  it("disables button and shows 'Attaching...' when isAttaching is true", () => {
    const onAttach = vi.fn();
    render(
      <BrowseLeafCard
        {...defaultProps}
        onAttach={onAttach}
        isAttaching={true}
      />
    );
    const button = screen.getByText("Attaching...");
    expect(button).toBeInTheDocument();
    expect(button.closest("button")).toBeDisabled();
  });
});
