import { useEffect, useLayoutEffect, useRef, useCallback, useState } from "react";
import { invoke } from "@tauri-apps/api/core";

interface VizFrameProps {
  vizBundlePath: string;
  workDir?: string;
  leafSlug: string;
  paused: boolean;
  mode?: "live" | "replay";
  replayData?: Record<string, unknown>;
}

/** Hash an ArrayBuffer for change detection (simple FNV-1a). */
function hashBytes(data: number[]): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < data.length; i++) {
    h ^= data[i];
    h = (h * 0x01000193) >>> 0;
  }
  return h;
}

/** Validate a relative path — reject traversal and absolute paths. */
function isValidRelPath(path: string): boolean {
  if (!path || path.startsWith("/") || path.startsWith("\\")) return false;
  const parts = path.replace(/\\/g, "/").split("/");
  return !parts.some((p) => p === ".." || p === "");
}

/**
 * VizFrame renders a sandboxed iframe that loads a viz bundle's index.html
 * and provides a postMessage bridge for file access via Tauri commands.
 *
 * Supports two modes:
 * - "live" (default): sets up file watching for real-time viz during computation
 * - "replay": sends persisted result data for post-completion visualization
 */
export function VizFrame({ vizBundlePath, workDir, leafSlug, paused, mode = "live", replayData }: VizFrameProps) {
  const iframeRef = useRef<HTMLIFrameElement>(null);
  const watchIntervalsRef = useRef<Map<string, ReturnType<typeof setInterval>>>(new Map());
  const watchHashesRef = useRef<Map<string, number>>(new Map());
  const readyRef = useRef(false);
  const [vizReady, setVizReady] = useState(false);

  const isReplay = mode === "replay";

  // Tell the Rust backend which directory to serve, then load via custom protocol.
  const indexUrl = "http://lettuce-viz.localhost/index.html";

  useEffect(() => {
    setVizReady(false);
    readyRef.current = false;
    invoke("set_viz_base", { path: vizBundlePath })
      .then(() => setVizReady(true))
      .catch(console.error);
  }, [vizBundlePath]);

  /** Post a message to the iframe. */
  const postToIframe = useCallback((msg: unknown) => {
    iframeRef.current?.contentWindow?.postMessage(msg, "*");
  }, []);

  /** Handle readFile request from viz iframe (live mode only). */
  const handleReadFile = useCallback(async (id: string, path: string) => {
    if (isReplay) return;
    if (!isValidRelPath(path)) {
      postToIframe({ type: "readFileResult", id, data: null, error: "invalid path" });
      return;
    }
    try {
      const data: number[] = await invoke("viz_read_file", { workDir, relPath: path });
      postToIframe({ type: "readFileResult", id, data });
    } catch (e) {
      postToIframe({ type: "readFileResult", id, data: null, error: String(e) });
    }
  }, [workDir, postToIframe, isReplay]);

  /** Handle listFiles request from viz iframe (live mode only). */
  const handleListFiles = useCallback(async (id: string, pattern?: string) => {
    if (isReplay) return;
    try {
      const files: string[] = await invoke("viz_list_files", { workDir, pattern: pattern ?? null });
      postToIframe({ type: "listFilesResult", id, files });
    } catch (e) {
      postToIframe({ type: "listFilesResult", id, files: [], error: String(e) });
    }
  }, [workDir, postToIframe, isReplay]);

  /** Start watching a file for changes (live mode only). */
  const handleWatchFile = useCallback((path: string, interval?: number) => {
    if (isReplay) return;
    if (!isValidRelPath(path)) return;

    // Clear existing watch on same path.
    const existing = watchIntervalsRef.current.get(path);
    if (existing) clearInterval(existing);

    const ms = interval ?? 1000;
    const id = setInterval(async () => {
      try {
        const data: number[] = await invoke("viz_read_file", { workDir, relPath: path });
        const newHash = hashBytes(data);
        const prevHash = watchHashesRef.current.get(path);
        if (prevHash !== newHash) {
          watchHashesRef.current.set(path, newHash);
          postToIframe({ type: "fileChanged", path, data });
        }
      } catch {
        // File may not exist yet — ignore errors during polling.
      }
    }, ms);

    watchIntervalsRef.current.set(path, id);
  }, [workDir, postToIframe, isReplay]);

  /** Stop watching a file. */
  const handleUnwatchFile = useCallback((path: string) => {
    const id = watchIntervalsRef.current.get(path);
    if (id) {
      clearInterval(id);
      watchIntervalsRef.current.delete(path);
      watchHashesRef.current.delete(path);
    }
  }, []);

  /** Clear all watch intervals. */
  const clearAllWatches = useCallback(() => {
    for (const id of watchIntervalsRef.current.values()) {
      clearInterval(id);
    }
    watchIntervalsRef.current.clear();
    watchHashesRef.current.clear();
  }, []);

  // Pause/unpause: clear watches when paused (live mode only).
  useEffect(() => {
    if (paused && !isReplay) {
      clearAllWatches();
    }
  }, [paused, clearAllWatches, isReplay]);

  // Cleanup on unmount.
  useEffect(() => {
    return () => clearAllWatches();
  }, [clearAllWatches]);

  /** Send vizInit (and replayData in replay mode) to the iframe. */
  const sendInit = useCallback(() => {
    readyRef.current = true;
    postToIframe({
      type: "vizInit",
      mode: isReplay ? "replay" : "live",
      workDir: isReplay ? "" : workDir,
      leafSlug,
      params: {},
    });

    // In replay mode, send result data after init.
    if (isReplay && replayData) {
      // Small delay to let the viz initialize before sending data.
      setTimeout(() => {
        postToIframe({ type: "replayData", ...replayData });
      }, 50);
    }
  }, [workDir, leafSlug, postToIframe, isReplay, replayData]);

  // Register message listener in useLayoutEffect to ensure it's active before
  // the iframe can fire events (fixes the vizReady/vizInit race condition).
  useLayoutEffect(() => {
    const handler = (event: MessageEvent) => {
      if (event.source !== iframeRef.current?.contentWindow) return;

      const msg = event.data;
      if (!msg || typeof msg.type !== "string") return;

      switch (msg.type) {
        case "vizReady":
          sendInit();
          break;
        case "readFile":
          if (msg.id && msg.path) handleReadFile(msg.id, msg.path);
          break;
        case "listFiles":
          if (msg.id) handleListFiles(msg.id, msg.pattern);
          break;
        case "watchFile":
          if (msg.path) handleWatchFile(msg.path, msg.interval);
          break;
        case "unwatchFile":
          if (msg.path) handleUnwatchFile(msg.path);
          break;
      }
    };

    window.addEventListener("message", handler);

    return () => window.removeEventListener("message", handler);
  }, [sendInit, handleReadFile, handleListFiles, handleWatchFile, handleUnwatchFile]);

  // Fallback: if iframe loads from cache and vizReady was never received, send vizInit after a short delay.
  const handleIframeLoad = useCallback(() => {
    setTimeout(() => {
      if (!readyRef.current) {
        sendInit();
      }
    }, 150);
  }, [sendInit]);

  if (!vizReady) {
    return <div style={{ width: "100%", height: "100%", background: "#0a0a0f" }} />;
  }

  return (
    <iframe
      ref={iframeRef}
      src={indexUrl}
      sandbox="allow-scripts allow-same-origin"
      onLoad={handleIframeLoad}
      style={{
        width: "100%",
        height: "100%",
        border: "none",
        background: "#0a0a0f",
      }}
      title="Visualization"
    />
  );
}
