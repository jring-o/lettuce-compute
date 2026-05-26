"use client";

import { useRef, useEffect, useCallback } from "react";

interface VizIframeProps {
  vizBundleUrl: string;
  vizOrigin: string;
  leafSlug: string;
  resultOutputData: Record<string, unknown> | null;
}

export function VizIframe({
  vizBundleUrl,
  vizOrigin,
  leafSlug,
  resultOutputData,
}: VizIframeProps) {
  const iframeRef = useRef<HTMLIFrameElement>(null);
  const readyRef = useRef(false);
  const pendingDataRef = useRef<Record<string, unknown> | null>(null);

  const sendReplayData = useCallback(
    (data: Record<string, unknown> | null) => {
      const iframe = iframeRef.current;
      if (!iframe?.contentWindow || !readyRef.current || !data) return;
      iframe.contentWindow.postMessage(
        { type: "replayData", ...data },
        "*",
      );
    },
    [],
  );

  // Listen for vizReady, then send vizInit + pending replayData
  useEffect(() => {
    const handleMessage = (event: MessageEvent) => {
      if (event.source !== iframeRef.current?.contentWindow) return;
      const msg = event.data;
      if (!msg || typeof msg !== "object") return;

      if (msg.type === "vizReady") {
        // Send vizInit
        iframeRef.current?.contentWindow?.postMessage(
          {
            type: "vizInit",
            mode: "replay",
            workDir: "",
            leafSlug,
            params: {},
          },
          "*",
        );
        readyRef.current = true;

        // Send any pending replay data
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
  }, [leafSlug, sendReplayData]);

  // Send replayData when resultOutputData changes
  useEffect(() => {
    if (!resultOutputData) return;
    if (readyRef.current) {
      sendReplayData(resultOutputData);
    } else {
      // Iframe not ready yet — queue it
      pendingDataRef.current = resultOutputData;
    }
  }, [resultOutputData, sendReplayData]);

  const encodedUrl = btoa(vizBundleUrl).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  const src = `${vizOrigin}/api/viz/bundle/${encodedUrl}/index.html`;

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
