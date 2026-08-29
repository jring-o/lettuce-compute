import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, act } from "@testing-library/react";

// Mock both Tauri modules before any imports
vi.mock("@tauri-apps/api/event", () => ({
  listen: vi.fn(() => Promise.resolve(() => {})),
}));

vi.mock("@tauri-apps/api/core", () => ({
  invoke: vi.fn(),
}));

vi.mock("lucide-react", () => ({
  Download: (props: any) => <span data-testid="download-icon" {...props} />,
  X: (props: any) => <span data-testid="x-icon" {...props} />,
}));

import { listen } from "@tauri-apps/api/event";
import { invoke } from "@tauri-apps/api/core";
import { UpdateBanner } from "./update-banner";

const mockListen = vi.mocked(listen);
const mockInvoke = vi.mocked(invoke);

describe("UpdateBanner", () => {
  let listeners: Record<string, (event: any) => void>;

  beforeEach(() => {
    vi.clearAllMocks();
    listeners = {};

    mockListen.mockImplementation((event: string, callback: any) => {
      listeners[event] = callback;
      return Promise.resolve(() => {});
    });
  });

  it("renders nothing initially (no update available)", () => {
    const { container } = render(<UpdateBanner />);
    expect(container.firstChild).toBeNull();
  });

  it("shows banner when update:available event fires", () => {
    render(<UpdateBanner />);

    act(() => {
      listeners["update:available"]?.({
        payload: { version: "1.2.0", body: "Bug fixes" },
      });
    });

    expect(screen.getByText("Update available: v1.2.0")).toBeInTheDocument();
    expect(screen.getByText("Install Now")).toBeInTheDocument();
  });

  it("dismisses banner when X is clicked", () => {
    render(<UpdateBanner />);

    act(() => {
      listeners["update:available"]?.({
        payload: { version: "1.2.0" },
      });
    });

    expect(screen.getByText("Update available: v1.2.0")).toBeInTheDocument();

    fireEvent.click(screen.getByLabelText("Dismiss update"));
    expect(screen.queryByText("Update available: v1.2.0")).not.toBeInTheDocument();
  });

  it("calls install_update command when Install Now is clicked", () => {
    mockInvoke.mockResolvedValue(undefined as never);

    render(<UpdateBanner />);

    act(() => {
      listeners["update:available"]?.({
        payload: { version: "1.2.0" },
      });
    });

    fireEvent.click(screen.getByText("Install Now"));
    expect(mockInvoke).toHaveBeenCalledWith("install_update");
  });

  it("shows download progress when update:progress event fires", () => {
    mockInvoke.mockResolvedValue(undefined as never);

    render(<UpdateBanner />);

    act(() => {
      listeners["update:available"]?.({
        payload: { version: "1.2.0" },
      });
    });

    fireEvent.click(screen.getByText("Install Now"));

    act(() => {
      listeners["update:progress"]?.({
        payload: { progress_pct: 45 },
      });
    });

    expect(screen.getByText("Downloading update...")).toBeInTheDocument();
    expect(screen.getByText("45%")).toBeInTheDocument();
  });

  it("shows error if install_update fails", async () => {
    mockInvoke.mockRejectedValue(new Error("Download failed"));

    render(<UpdateBanner />);

    act(() => {
      listeners["update:available"]?.({
        payload: { version: "1.2.0" },
      });
    });

    fireEvent.click(screen.getByText("Install Now"));

    await vi.waitFor(() => {
      expect(screen.getByText("Download failed")).toBeInTheDocument();
    });
  });

  it("registers event listeners on mount", () => {
    render(<UpdateBanner />);
    expect(mockListen).toHaveBeenCalledWith("update:available", expect.any(Function));
    expect(mockListen).toHaveBeenCalledWith("update:progress", expect.any(Function));
  });
});
