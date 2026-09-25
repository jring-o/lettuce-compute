import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { invoke } from "@tauri-apps/api/core";
import { forwardWebviewLogs } from "./webview-log";

/** The `log_from_webview` calls made so far, as `[level, message]`. */
function forwarded(): Array<[string, string]> {
  return vi
    .mocked(invoke)
    .mock.calls.filter(([cmd]) => cmd === "log_from_webview")
    .map(([, args]) => {
      const { level, message } = args as { level: string; message: string };
      return [level, message];
    });
}

describe("forwardWebviewLogs", () => {
  let stop: () => void;
  let printedErrors: unknown[][];
  let printedWarnings: unknown[][];

  beforeEach(() => {
    vi.clearAllMocks();
    printedErrors = [];
    printedWarnings = [];
    // Stand-ins for the web view's console, so the test output stays clean
    // and what still reaches the console can be checked.
    vi.spyOn(console, "error").mockImplementation((...args: unknown[]) => {
      printedErrors.push(args);
    });
    vi.spyOn(console, "warn").mockImplementation((...args: unknown[]) => {
      printedWarnings.push(args);
    });
    stop = forwardWebviewLogs();
  });

  afterEach(() => {
    stop();
    vi.restoreAllMocks();
  });

  it("forwards console errors and warnings and still prints them", () => {
    console.error("could not load credit:", { code: "DAEMON_UNREACHABLE", status: 0 });
    console.warn("slow response", 1200);

    expect(forwarded()).toEqual([
      ["error", 'could not load credit: {"code":"DAEMON_UNREACHABLE","status":0}'],
      ["warn", "slow response 1200"],
    ]);
    expect(printedErrors).toHaveLength(1);
    expect(printedWarnings).toHaveLength(1);
  });

  it("forwards an uncaught error with its stack and where it happened", () => {
    const error = new Error("render failed");
    window.dispatchEvent(
      new ErrorEvent("error", {
        error,
        message: "render failed",
        filename: "app.js",
        lineno: 10,
        colno: 4,
      })
    );

    const [[level, message]] = forwarded();
    expect(level).toBe("error");
    expect(message).toMatch(/^uncaught error: Error: render failed/);
    expect(message).toContain("(app.js:10:4)");
  });

  it("forwards an unhandled promise rejection", () => {
    const event = new Event("unhandledrejection");
    Object.assign(event, { reason: { code: "CONFLICT", message: "not paused", status: 409 } });
    window.dispatchEvent(event);

    expect(forwarded()).toEqual([
      ["error", 'unhandled promise rejection: {"code":"CONFLICT","message":"not paused","status":409}'],
    ]);
  });

  it("does not throw or loop when the host cannot take the line", async () => {
    vi.mocked(invoke).mockRejectedValueOnce(new Error("no host"));
    expect(() => console.error("boom")).not.toThrow();
    await Promise.resolve();
    expect(forwarded()).toHaveLength(1);
    expect(printedErrors).toHaveLength(1);
  });

  it("cuts very long messages", () => {
    console.error("x".repeat(10_000));
    const [[, message]] = forwarded();
    expect(message).toHaveLength(4000);
  });

  it("stops forwarding once undone", () => {
    stop();
    console.error("after");
    window.dispatchEvent(new ErrorEvent("error", { message: "after" }));
    expect(forwarded()).toEqual([]);
    stop = () => {};
  });
});
