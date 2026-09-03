import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { invoke, defaultCommandResult } from "@tauri-apps/api/core";
import { emit } from "@tauri-apps/api/event";

/**
 * The wizard stands in for itself: two buttons that fire the callbacks the
 * real one fires at the same points of its run.
 */
vi.mock("@/components/wizard/setup-wizard", () => ({
  SetupWizard: ({
    onInitialized,
    onComplete,
  }: {
    onInitialized?: () => void;
    onComplete: () => void;
  }) => (
    <div>
      <p>wizard</p>
      <button onClick={() => onInitialized?.()}>config written</button>
      <button onClick={onComplete}>daemon up</button>
    </div>
  ),
}));

vi.mock("@/components/layout/tab-layout", () => ({
  TabLayout: () => <p>dashboard</p>,
}));

import { App } from "./App";

describe("App", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // TB-55: the Rust host starts the tray poll, notifications and the Podman
  // auto-start on `app_initialized`. It must be emitted when the wizard
  // reports the config written, not only when the daemon is up — otherwise a
  // slow or failed first start left the tray at "Stopped" for the session.
  it("tells the host the install is initialized as soon as the wizard has written the config", async () => {
    const user = userEvent.setup();
    let initialized = false;
    vi.mocked(invoke).mockImplementation(async (cmd: string) =>
      cmd === "is_initialized" ? initialized : defaultCommandResult(cmd)
    );
    render(<App />);
    await screen.findByText("wizard");

    await user.click(screen.getByText("config written"));
    await waitFor(() => expect(emit).toHaveBeenCalledWith("app_initialized"));
    // Still the wizard: the daemon is not up yet.
    expect(screen.getByText("wizard")).toBeInTheDocument();

    initialized = true;
    await user.click(screen.getByText("daemon up"));
    await screen.findByText("dashboard");
  });
});
