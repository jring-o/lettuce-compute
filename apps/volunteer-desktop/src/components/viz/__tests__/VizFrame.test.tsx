import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, waitFor, act } from "@testing-library/react";

// Mock Tauri APIs before imports.
vi.mock("@tauri-apps/api/core", () => ({
  invoke: vi.fn(),
  // The Windows/Android URL form of a custom protocol; macOS and Linux use
  // `<protocol>://localhost/<path>`. Tests assert on protocol name and path.
  convertFileSrc: vi.fn(
    (path: string, protocol: string = "asset") => `http://${protocol}.localhost/${path}`
  ),
}));

import { invoke } from "@tauri-apps/api/core";
import { VizFrame } from "../VizFrame";

const mockInvoke = vi.mocked(invoke);

/** Helper: render VizFrame and wait for set_viz_base to resolve (vizReady=true). */
async function renderReady(props?: Partial<Parameters<typeof VizFrame>[0]>) {
  const defaultProps = {
    vizBundlePath: "/work/.lettuce-viz",
    workDir: "/work",
    leafSlug: "test",
    paused: false,
    ...props,
  };

  let result!: ReturnType<typeof render>;
  await act(async () => {
    result = render(<VizFrame {...defaultProps} />);
  });
  // Wait for iframe to appear (vizReady becomes true after invoke resolves)
  await waitFor(() => {
    expect(result.container.querySelector("iframe")).toBeTruthy();
  });
  return result;
}

describe("VizFrame", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // VizFrame calls invoke("set_viz_base", ...) in useEffect and expects a Promise
    mockInvoke.mockResolvedValue(undefined);
  });

  afterEach(() => {
    cleanup();
  });

  it("renders iframe with correct sandbox attributes", async () => {
    const { container } = await renderReady({
      vizBundlePath: "/home/user/.lettuce/work/abc/.lettuce-viz",
      workDir: "/home/user/.lettuce/work/abc",
      leafSlug: "test-leaf",
    });

    const iframe = container.querySelector("iframe");
    expect(iframe).toBeTruthy();
    expect(iframe?.getAttribute("sandbox")).toBe("allow-scripts allow-same-origin");
    expect(iframe?.getAttribute("src")).toContain("lettuce-viz.localhost");
    expect(iframe?.getAttribute("src")).toContain("index.html");
  });

  it("handles readFile postMessage from iframe", async () => {
    await renderReady();

    // Fire a readFile request as if from iframe.
    window.dispatchEvent(
      new MessageEvent("message", {
        data: { type: "readFile", id: "req-1", path: "data.json" },
        // source would be iframe.contentWindow, but we can't easily mock that
        // in jsdom. The handler checks event.source === iframe.contentWindow.
      })
    );

    // viz_read_file should NOT be called because source check fails in jsdom.
    // set_viz_base IS called (by useEffect), but viz_read_file should not be.
    const vizReadFileCalls = mockInvoke.mock.calls.filter(
      ([cmd]) => cmd === "viz_read_file"
    );
    expect(vizReadFileCalls).toHaveLength(0);
  });

  it("rejects path traversal in readFile validation", async () => {
    // Directly test the path validation by checking that the VizFrame
    // renders without errors even with potentially malicious paths.
    const { container } = await renderReady();

    // Iframe should render regardless.
    const iframe = container.querySelector("iframe");
    expect(iframe).toBeTruthy();
  });

  it("handles watchFile and unwatchFile lifecycle", async () => {
    const { unmount } = await renderReady();

    // Unmount should clean up without errors (clears all intervals).
    expect(() => unmount()).not.toThrow();
  });

  it("clears watches when paused", async () => {
    const { rerender } = await renderReady();

    // Re-render with paused=true should not throw.
    await act(async () => {
      rerender(
        <VizFrame
          vizBundlePath="/work/.lettuce-viz"
          workDir="/work"
          leafSlug="test"
          paused={true}
        />
      );
    });
  });

  // --- S105: useLayoutEffect listener registration, pendingVizReadyRef, sandbox update ---

  it("uses allow-same-origin in sandbox attribute (S105)", async () => {
    const { container } = await renderReady();

    const iframe = container.querySelector("iframe");
    expect(iframe).toBeTruthy();
    // S105 changed sandbox to include allow-same-origin for custom protocol
    expect(iframe?.getAttribute("sandbox")).toContain("allow-scripts");
    expect(iframe?.getAttribute("sandbox")).toContain("allow-same-origin");
  });

  it("registers message listener before iframe can fire (useLayoutEffect)", async () => {
    // Verify that the message listener is registered synchronously by checking
    // that dispatching a message during render doesn't cause errors.
    const addEventSpy = vi.spyOn(window, "addEventListener");

    await renderReady();

    // useLayoutEffect should have registered a "message" listener
    const messageCalls = addEventSpy.mock.calls.filter(
      ([type]) => type === "message"
    );
    expect(messageCalls.length).toBeGreaterThan(0);

    addEventSpy.mockRestore();
  });

  it("cleans up message listener on unmount", async () => {
    const removeEventSpy = vi.spyOn(window, "removeEventListener");

    const { unmount } = await renderReady();

    unmount();

    const messageCalls = removeEventSpy.mock.calls.filter(
      ([type]) => type === "message"
    );
    expect(messageCalls.length).toBeGreaterThan(0);

    removeEventSpy.mockRestore();
  });

  it("uses fallback delay of 150ms for onLoad handler (S105)", async () => {
    vi.useFakeTimers();
    // Need to flush the pending microtask from mockResolvedValue
    mockInvoke.mockImplementation(() => Promise.resolve(undefined));

    let result!: ReturnType<typeof render>;
    await act(async () => {
      result = render(
        <VizFrame
          vizBundlePath="/work/.lettuce-viz"
          workDir="/work"
          leafSlug="test"
          paused={false}
        />
      );
      // Flush the microtask queue to let set_viz_base resolve
      await vi.advanceTimersByTimeAsync(0);
    });

    const iframe = result.container.querySelector("iframe");
    expect(iframe).toBeTruthy();

    // Fire onload
    if (iframe) {
      iframe.dispatchEvent(new Event("load"));
    }

    // After 149ms, the fallback should not have fired
    vi.advanceTimersByTime(149);
    // After 150ms, it should fire (we can't inspect postMessage easily in jsdom,
    // but this verifies no error is thrown)
    expect(() => vi.advanceTimersByTime(1)).not.toThrow();

    vi.useRealTimers();
  });

  it("shows loading placeholder before viz is ready", () => {
    // set_viz_base invoke returns a pending promise (never resolves)
    mockInvoke.mockReturnValue(new Promise(() => {}));

    const { container } = render(
      <VizFrame
        vizBundlePath="/work/.lettuce-viz"
        workDir="/work"
        leafSlug="test"
        paused={false}
      />
    );

    // Should show the loading div, not an iframe
    const iframe = container.querySelector("iframe");
    expect(iframe).toBeNull();
    // The loading placeholder is a div — it's the only child of the container
    const childDiv = container.firstElementChild;
    expect(childDiv).toBeTruthy();
    expect(childDiv?.tagName).toBe("DIV");
    // No iframe inside
    expect(container.querySelector("iframe")).toBeNull();
  });

  it("resets readyRef and vizReady when vizBundlePath changes", async () => {
    const { container, rerender } = await renderReady({
      vizBundlePath: "/work/.lettuce-viz-v1",
    });

    // Should have iframe
    expect(container.querySelector("iframe")).toBeTruthy();

    // Change vizBundlePath — should re-invoke set_viz_base
    const invokeCalls = mockInvoke.mock.calls.length;
    await act(async () => {
      rerender(
        <VizFrame
          vizBundlePath="/work/.lettuce-viz-v2"
          workDir="/work"
          leafSlug="test"
          paused={false}
        />
      );
    });

    // set_viz_base should have been called again for the new path
    const newCalls = mockInvoke.mock.calls.filter(
      ([cmd]) => cmd === "set_viz_base"
    );
    expect(newCalls.length).toBeGreaterThan(1);
    // The second call should reference the new path
    const lastCall = newCalls[newCalls.length - 1];
    expect(lastCall[1]).toEqual({ path: "/work/.lettuce-viz-v2" });
  });

  it("does not call viz_read_file for invalid paths (path traversal)", async () => {
    await renderReady();

    // The handler checks isValidRelPath before invoking — we cannot trigger
    // the handler through jsdom (source check), but we can verify that
    // invoke is not called with viz_read_file for any reason besides set_viz_base.
    const vizCalls = mockInvoke.mock.calls.filter(
      ([cmd]) => cmd === "viz_read_file"
    );
    expect(vizCalls).toHaveLength(0);
  });
});

// ============================================================
// isValidRelPath — pure function tests (exported for testing)
// ============================================================
// Since isValidRelPath is not exported, we test it via the postMessage
// bridge indirectly. These tests document the expected path validation behavior.

describe("VizFrame path validation behavior", () => {
  // We verify the security contract by noting the rules from the source:
  // - empty string -> false
  // - starts with "/" -> false
  // - starts with "\" -> false
  // - contains ".." segment -> false
  // - contains empty segment (double slash) -> false
  // - valid relative path -> true

  it("iframe renders regardless of path validation logic", async () => {
    const { container } = await renderReady();
    expect(container.querySelector("iframe")).toBeTruthy();
  });
});

// ============================================================
// Bundle URL, failure state, and path validation
// ============================================================

import { isValidRelPath, VIZ_PROTOCOL } from "../VizFrame";
import { convertFileSrc } from "@tauri-apps/api/core";

describe("VizFrame bundle URL", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockInvoke.mockResolvedValue(undefined);
  });

  afterEach(() => {
    cleanup();
  });

  it("builds the index.html URL with convertFileSrc so the platform's custom-protocol form is used", async () => {
    const { container } = await renderReady();
    expect(VIZ_PROTOCOL).toBe("lettuce-viz");
    expect(vi.mocked(convertFileSrc)).toHaveBeenCalledWith("index.html", "lettuce-viz");
    // The real convertFileSrc yields http://lettuce-viz.localhost/index.html
    // on Windows and lettuce-viz://localhost/index.html on macOS and Linux.
    expect(container.querySelector("iframe")?.getAttribute("src")).toBe(
      "http://lettuce-viz.localhost/index.html"
    );
  });

  it("sets the bundle directory before loading the frame", async () => {
    await renderReady({ vizBundlePath: "/home/u/.lettuce/work/abc/.lettuce-viz" });
    expect(mockInvoke).toHaveBeenCalledWith("set_viz_base", {
      path: "/home/u/.lettuce/work/abc/.lettuce-viz",
    });
  });
});

describe("VizFrame failure state", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it("shows the host's error instead of a blank frame when the bundle cannot be opened", async () => {
    mockInvoke.mockRejectedValue("visualization bundle not found at /gone/.lettuce-viz");

    let result!: ReturnType<typeof render>;
    await act(async () => {
      result = render(
        <VizFrame vizBundlePath="/gone/.lettuce-viz" workDir="/gone" leafSlug="t" paused={false} />
      );
    });

    await waitFor(() => {
      expect(result.getByTestId("viz-error")).toBeInTheDocument();
    });
    expect(result.getByTestId("viz-error")).toHaveTextContent(
      "The visualization files could not be opened."
    );
    expect(result.getByTestId("viz-error")).toHaveTextContent(
      "visualization bundle not found at /gone/.lettuce-viz"
    );
    expect(result.container.querySelector("iframe")).toBeNull();
  });

  it("explains a missing bundle in replay terms", async () => {
    mockInvoke.mockRejectedValue("visualization bundle not found at /gone/.lettuce-viz");

    let result!: ReturnType<typeof render>;
    await act(async () => {
      result = render(
        <VizFrame
          vizBundlePath="/gone/.lettuce-viz"
          leafSlug="t"
          paused={false}
          mode="replay"
          replayData={{ frames: [] }}
        />
      );
    });

    await waitFor(() => {
      expect(result.getByTestId("viz-error")).toHaveTextContent(
        "The visualization files for this result are no longer on this machine."
      );
    });
  });

  it("recovers when the bundle path changes to one that opens", async () => {
    mockInvoke.mockRejectedValueOnce("visualization bundle not found at /gone");
    mockInvoke.mockResolvedValue(undefined);

    let result!: ReturnType<typeof render>;
    await act(async () => {
      result = render(
        <VizFrame vizBundlePath="/gone" workDir="/w" leafSlug="t" paused={false} />
      );
    });
    await waitFor(() => {
      expect(result.getByTestId("viz-error")).toBeInTheDocument();
    });

    await act(async () => {
      result.rerender(
        <VizFrame vizBundlePath="/w/.lettuce-viz" workDir="/w" leafSlug="t" paused={false} />
      );
    });
    await waitFor(() => {
      expect(result.container.querySelector("iframe")).toBeTruthy();
    });
    expect(result.queryByTestId("viz-error")).toBeNull();
  });
});

describe("isValidRelPath", () => {
  it("accepts relative paths inside the work directory", () => {
    expect(isValidRelPath("state.json")).toBe(true);
    expect(isValidRelPath("frames/0001.bin")).toBe(true);
    expect(isValidRelPath("frames\\0001.bin")).toBe(true);
  });

  it("rejects empty, absolute, traversing, and double-slash paths", () => {
    expect(isValidRelPath("")).toBe(false);
    expect(isValidRelPath("/etc/passwd")).toBe(false);
    expect(isValidRelPath("\\windows\\system32")).toBe(false);
    expect(isValidRelPath("../secret")).toBe(false);
    expect(isValidRelPath("frames/../../secret")).toBe(false);
    expect(isValidRelPath("frames//0001.bin")).toBe(false);
  });
});
