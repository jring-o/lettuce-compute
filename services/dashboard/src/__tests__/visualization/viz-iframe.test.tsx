import { render, act, cleanup } from "@testing-library/react";
import { VizIframe } from "@/components/visualization/VizIframe";

describe("VizIframe", () => {
  // base64url-encode the way the component does (btoa + url-safe substitutions).
  const b64url = (s: string) =>
    btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

  const VIZ_ORIGIN = "https://viz.example.com";

  const defaultProps = {
    vizBundleUrl: "https://example.com/bundle.tar.gz",
    vizOrigin: VIZ_ORIGIN,
    leafSlug: "nbody-sim",
    resultOutputData: null as Record<string, unknown> | null,
  };

  afterEach(() => {
    cleanup();
  });

  it("renders a sandboxed iframe (allow-scripts + allow-same-origin)", () => {
    render(<VizIframe {...defaultProps} />);
    const iframe = document.querySelector("iframe");
    expect(iframe).toBeInTheDocument();
    // allow-same-origin is required so the bundle can use storage/WASM/etc.;
    // origin isolation comes from serving on the separate VIZ_ORIGIN host.
    expect(iframe).toHaveAttribute(
      "sandbox",
      "allow-scripts allow-same-origin",
    );
  });

  it("constructs the path-based src URL on the viz origin", () => {
    render(<VizIframe {...defaultProps} />);
    const iframe = document.querySelector("iframe");
    const expectedSrc = `${VIZ_ORIGIN}/api/viz/bundle/${b64url(
      "https://example.com/bundle.tar.gz",
    )}/index.html`;
    expect(iframe).toHaveAttribute("src", expectedSrc);
  });

  it("encodes special characters in vizBundleUrl", () => {
    render(
      <VizIframe
        {...defaultProps}
        vizBundleUrl="https://example.com/path?a=1&b=2"
      />,
    );
    const iframe = document.querySelector("iframe");
    const expectedSrc = `${VIZ_ORIGIN}/api/viz/bundle/${b64url(
      "https://example.com/path?a=1&b=2",
    )}/index.html`;
    expect(iframe).toHaveAttribute("src", expectedSrc);
  });

  it("adds a message event listener on mount", () => {
    const addSpy = jest.spyOn(window, "addEventListener");
    render(<VizIframe {...defaultProps} />);

    const messageCalls = addSpy.mock.calls.filter(
      ([type]) => type === "message",
    );
    expect(messageCalls.length).toBeGreaterThanOrEqual(1);

    addSpy.mockRestore();
  });

  it("removes the message event listener on unmount", () => {
    const removeSpy = jest.spyOn(window, "removeEventListener");
    const { unmount } = render(<VizIframe {...defaultProps} />);

    unmount();

    const messageCalls = removeSpy.mock.calls.filter(
      ([type]) => type === "message",
    );
    expect(messageCalls.length).toBeGreaterThanOrEqual(1);

    removeSpy.mockRestore();
  });

  it("applies dark background styling", () => {
    render(<VizIframe {...defaultProps} />);
    const iframe = document.querySelector("iframe") as HTMLIFrameElement;
    expect(iframe.style.backgroundColor).toBe("rgb(10, 10, 15)");
  });

  // NOTE: The following tests document expected behavior but have limitations
  // in jsdom. In jsdom, iframe.contentWindow is null (no real frame loading),
  // so postMessage-based communication cannot be fully tested. The component
  // guards against null contentWindow, so these tests verify the guard paths.

  it("does not crash when resultOutputData is provided before iframe is ready", () => {
    // This exercises the queuing path (pendingDataRef).
    // Since jsdom has no real contentWindow, the data is queued but never sent.
    expect(() => {
      render(
        <VizIframe
          {...defaultProps}
          resultOutputData={{ particles: [1, 2, 3] }}
        />,
      );
    }).not.toThrow();
  });

  it("does not crash when receiving a message event with non-object data", () => {
    render(<VizIframe {...defaultProps} />);

    // Dispatch message events with various non-object payloads
    expect(() => {
      act(() => {
        window.dispatchEvent(
          new MessageEvent("message", { data: null }),
        );
        window.dispatchEvent(
          new MessageEvent("message", { data: "string-payload" }),
        );
        window.dispatchEvent(
          new MessageEvent("message", { data: 42 }),
        );
      });
    }).not.toThrow();
  });

  it("does not crash when receiving a vizReady message (jsdom: no contentWindow)", () => {
    // In jsdom, iframeRef.current?.contentWindow is null, so the source check
    // (event.source !== iframeRef.current?.contentWindow) will not match.
    // This confirms the component handles that gracefully.
    render(<VizIframe {...defaultProps} />);

    expect(() => {
      act(() => {
        window.dispatchEvent(
          new MessageEvent("message", {
            data: { type: "vizReady" },
            source: null as unknown as Window,
          }),
        );
      });
    }).not.toThrow();
  });

  it("handles resultOutputData changing across re-renders", () => {
    const { rerender } = render(<VizIframe {...defaultProps} />);

    expect(() => {
      rerender(
        <VizIframe
          {...defaultProps}
          resultOutputData={{ step: 1 }}
        />,
      );
      rerender(
        <VizIframe
          {...defaultProps}
          resultOutputData={{ step: 2 }}
        />,
      );
      rerender(
        <VizIframe
          {...defaultProps}
          resultOutputData={null}
        />,
      );
    }).not.toThrow();
  });
});
