"use client";

import { useRef, useEffect, useCallback, useMemo } from "react";

interface VizIframeProps {
  vizBundleUrl: string;
  vizOrigin: string;
  /** The app's own origin (PLATFORM_URL). The viz MUST NOT share it. */
  platformUrl: string;
  leafSlug: string;
  resultOutputData: Record<string, unknown> | null;
}

/**
 * Resolves the isolation origin for the viz iframe, or null if the config is
 * not safe (BG-08). The visualization bundle is AUTHOR-CONTROLLED JavaScript;
 * it must run on an origin that is DISTINCT from the app origin, so its
 * `allow-same-origin` sandbox grant confers the viz origin's authority (which
 * holds no cookies, no API, no sensitive DOM) rather than the app's.
 *
 * Returns the parsed origin (scheme+host+port) of vizOrigin only when it is a
 * parseable URL whose origin differs from the app's. Fails closed (null) when
 * VIZ_ORIGIN is empty, unparseable, host/scheme/port-equal to PLATFORM_URL, or
 * when PLATFORM_URL itself is missing/unparseable (distinctness can't be
 * proven). Comparison is on PARSED origins, not raw strings (R1.6).
 */
export function resolveVizOrigin(
  vizOrigin: string,
  platformUrl: string,
): string | null {
  if (!vizOrigin) return null;
  let viz: URL;
  try {
    viz = new URL(vizOrigin);
  } catch {
    return null;
  }
  let app: URL;
  try {
    app = new URL(platformUrl);
  } catch {
    // Can't prove the viz origin is distinct from the app → fail closed.
    return null;
  }
  if (viz.origin === app.origin) return null;
  return viz.origin;
}

export function VizIframe({
  vizBundleUrl,
  vizOrigin,
  platformUrl,
  leafSlug,
  resultOutputData,
}: VizIframeProps) {
  const iframeRef = useRef<HTMLIFrameElement>(null);
  const readyRef = useRef(false);
  const pendingDataRef = useRef<Record<string, unknown> | null>(null);

  // The single isolation knob. null → do not render the iframe (fail closed).
  const isolationOrigin = useMemo(
    () => resolveVizOrigin(vizOrigin, platformUrl),
    [vizOrigin, platformUrl],
  );

  const sendReplayData = useCallback(
    (data: Record<string, unknown> | null) => {
      const iframe = iframeRef.current;
      if (!iframe?.contentWindow || !readyRef.current || !data) return;
      if (!isolationOrigin) return;
      // Target the viz origin explicitly (never "*"): a hostile frame that
      // later navigates away must not receive the replay payload.
      iframe.contentWindow.postMessage(
        { type: "replayData", ...data },
        isolationOrigin,
      );
    },
    [isolationOrigin],
  );

  // Listen for vizReady, then send vizInit + pending replayData.
  useEffect(() => {
    if (!isolationOrigin) return;

    const handleMessage = (event: MessageEvent) => {
      // Accept only messages from OUR iframe AND from the viz origin — the
      // latter stops a hostile frame (or a navigated-away one) spoofing
      // vizReady.
      if (event.origin !== isolationOrigin) return;
      if (event.source !== iframeRef.current?.contentWindow) return;
      const msg = event.data;
      if (!msg || typeof msg !== "object") return;

      if (msg.type === "vizReady") {
        iframeRef.current?.contentWindow?.postMessage(
          {
            type: "vizInit",
            mode: "replay",
            workDir: "",
            leafSlug,
            params: {},
          },
          isolationOrigin,
        );
        readyRef.current = true;

        if (pendingDataRef.current) {
          sendReplayData(pendingDataRef.current);
          pendingDataRef.current = null;
        }
      }
    };

    window.addEventListener("message", handleMessage);
    return () => {
      window.removeEventListener("message", handleMessage);
      readyRef.current = false;
    };
  }, [isolationOrigin, leafSlug, sendReplayData]);

  // Send replayData when resultOutputData changes.
  useEffect(() => {
    if (!resultOutputData) return;
    if (readyRef.current) {
      sendReplayData(resultOutputData);
    } else {
      pendingDataRef.current = resultOutputData;
    }
  }, [resultOutputData, sendReplayData]);

  // Fail closed: without a distinct, parseable VIZ_ORIGIN the author bundle
  // would run on the app origin. Render an explanatory placeholder instead.
  if (!isolationOrigin) {
    return (
      <div
        data-testid="viz-unavailable"
        className="flex items-center justify-center h-full p-6 text-center text-muted-foreground"
        style={{ backgroundColor: "#0a0a0f", colorScheme: "dark" }}
      >
        Visualization unavailable — the operator has not configured a separate
        visualization origin.
      </div>
    );
  }

  const encodedUrl = btoa(vizBundleUrl).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  const src = `${isolationOrigin}/api/viz/bundle/${encodedUrl}/index.html`;

  return (
    <iframe
      ref={iframeRef}
      src={src}
      sandbox="allow-scripts allow-same-origin"
      className="w-full h-full border-0"
      style={{
        backgroundColor: "#0a0a0f",
        colorScheme: "dark",
      }}
    />
  );
}
