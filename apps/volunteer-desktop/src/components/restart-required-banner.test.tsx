import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { invoke } from "@tauri-apps/api/core";
import { RestartRequiredBanner } from "./restart-required-banner";
import {
  markRestartRequired,
  resetRestartRequiredForTest,
  useRestartRequired,
} from "@/hooks/use-restart-required";
import { renderHook } from "@testing-library/react";

const mockInvoke = vi.mocked(invoke);

describe("RestartRequiredBanner", () => {
  beforeEach(() => {
    mockInvoke.mockReset();
    mockInvoke.mockResolvedValue(undefined);
    resetRestartRequiredForTest();
  });

  it("renders nothing while no restart is pending", () => {
    render(<RestartRequiredBanner />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("appears with every recorded reason once one is marked, and a restart button", () => {
    render(<RestartRequiredBanner />);
    act(() => {
      markRestartRequired("Trust settings are saved.");
      markRestartRequired("Your disk allowance is now 21 GB.");
      markRestartRequired("Trust settings are saved.");
    });

    const status = screen.getByRole("status");
    expect(status).toHaveTextContent("Restart required");
    expect(screen.getByText("Trust settings are saved.")).toBeInTheDocument();
    expect(screen.getByText("Your disk allowance is now 21 GB.")).toBeInTheDocument();
    expect(screen.getAllByText("Trust settings are saved.")).toHaveLength(1);
    expect(screen.getByText("Restart Lettuce now")).toBeInTheDocument();
  });

  it("restarts the daemon and clears the notice when the restart succeeds", async () => {
    const user = userEvent.setup();
    let finish: () => void = () => {};
    mockInvoke.mockImplementation(
      () => new Promise<void>((resolve) => { finish = resolve; })
    );
    markRestartRequired("m");

    render(<RestartRequiredBanner />);
    await user.click(screen.getByText("Restart Lettuce now"));

    expect(mockInvoke).toHaveBeenCalledWith("restart_daemon");
    expect(screen.getByText("Restarting...")).toBeDisabled();
    expect(screen.getByText(/This can take up to a minute/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Dismiss restart notice")).not.toBeInTheDocument();

    finish();
    await waitFor(() => expect(screen.queryByRole("status")).not.toBeInTheDocument());
  });

  it("shows the error, keeps the notice and lets the user retry when the restart fails", async () => {
    const user = userEvent.setup();
    mockInvoke.mockRejectedValueOnce("daemon did not stop within 30s");
    markRestartRequired("m");

    render(<RestartRequiredBanner />);
    await user.click(screen.getByText("Restart Lettuce now"));

    await waitFor(() => {
      expect(
        screen.getByText("Restart failed: daemon did not stop within 30s")
      ).toBeInTheDocument();
    });
    expect(screen.getByText("m")).toBeInTheDocument();
    expect(screen.getByText("Restart Lettuce now")).not.toBeDisabled();
  });

  it("can be dismissed without restarting, which clears the pending reasons", async () => {
    const user = userEvent.setup();
    markRestartRequired("m");
    const { result } = renderHook(() => useRestartRequired());

    render(<RestartRequiredBanner />);
    await user.click(screen.getByLabelText("Dismiss restart notice"));

    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(mockInvoke).not.toHaveBeenCalledWith("restart_daemon");
    expect(result.current.restartRequired).toBe(false);
  });
});
