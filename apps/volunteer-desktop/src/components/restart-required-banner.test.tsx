import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { invoke } from "@tauri-apps/api/core";
import { RestartRequiredBanner } from "./restart-required-banner";

const mockInvoke = vi.mocked(invoke);

describe("RestartRequiredBanner", () => {
  beforeEach(() => {
    mockInvoke.mockReset();
    mockInvoke.mockResolvedValue(undefined);
  });

  it("shows the message and a restart button", () => {
    render(<RestartRequiredBanner message="Trust settings are saved." />);
    expect(screen.getByText("Trust settings are saved.")).toBeInTheDocument();
    expect(screen.getByText("Restart Lettuce now")).toBeInTheDocument();
  });

  it("restarts the daemon and reports back when done", async () => {
    const user = userEvent.setup();
    const onRestarted = vi.fn();
    let finish: () => void = () => {};
    mockInvoke.mockImplementation(
      () => new Promise<void>((resolve) => { finish = resolve; })
    );

    render(<RestartRequiredBanner message="m" onRestarted={onRestarted} />);
    await user.click(screen.getByText("Restart Lettuce now"));

    expect(mockInvoke).toHaveBeenCalledWith("restart_daemon");
    expect(screen.getByText("Restarting...")).toBeDisabled();
    expect(screen.getByText(/This can take up to a minute/)).toBeInTheDocument();
    expect(onRestarted).not.toHaveBeenCalled();

    finish();
    await waitFor(() => expect(onRestarted).toHaveBeenCalledOnce());
    expect(screen.getByText("Restart Lettuce now")).not.toBeDisabled();
  });

  it("shows the error and lets the user retry when the restart fails", async () => {
    const user = userEvent.setup();
    const onRestarted = vi.fn();
    mockInvoke.mockRejectedValueOnce("daemon did not stop within 30s");

    render(<RestartRequiredBanner message="m" onRestarted={onRestarted} />);
    await user.click(screen.getByText("Restart Lettuce now"));

    await waitFor(() => {
      expect(
        screen.getByText("Restart failed: daemon did not stop within 30s")
      ).toBeInTheDocument();
    });
    expect(onRestarted).not.toHaveBeenCalled();
    expect(screen.getByText("Restart Lettuce now")).not.toBeDisabled();
  });

  it("offers a dismiss control only when onDismiss is given", async () => {
    const user = userEvent.setup();
    const onDismiss = vi.fn();

    const { rerender } = render(<RestartRequiredBanner message="m" />);
    expect(screen.queryByLabelText("Dismiss restart notice")).not.toBeInTheDocument();

    rerender(<RestartRequiredBanner message="m" onDismiss={onDismiss} />);
    await user.click(screen.getByLabelText("Dismiss restart notice"));
    expect(onDismiss).toHaveBeenCalledOnce();
  });
});
