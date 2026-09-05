import { useEffect, useLayoutEffect, useRef, useCallback, useState } from "react";
import { invoke, convertFileSrc } from "@tauri-apps/api/core";

interface VizFrameProps {
  /**
   * Directory holding the bundle's `index.html`. The daemon reports it as
   * `viz_bundle_path`. For a running unit it is the bundle extracted into the
   * unit's work directory (`{work_dir}/.lettuce-viz`, or that directory's
   * single wrapper folder when the tarball had one — the daemon resolves it
   * before reporting). For a stored result it is the daemon's kept copy under
   * `<data dir>/results/viz/<bundle key>/`, which outlives the work directory.
   */
  vizBundlePath: string;
  /** The unit's work directory; `readFile` / `listFiles` / `watchFile` are resolved inside it. Live mode only. */
  workDir?: string;
  /**
   * Passed to the bundle as `vizInit.leafSlug`. For a live task the daemon
   * reports only the leaf's display name, so the app sends that; the head's
   * dashboard and replay send the real slug.
   */
  leafSlug: string;
  paused: boolean;
  mode?: "live" | "replay";
  replayData?: Record<string, unknown>;
  /**
   * Called once when the host gives up on the page (see
   * `VizUnavailableReason`). The frame replaces itself with a one-line note
   * either way; a parent that wants to collapse the space the frame took —
   * the Overview's 320 px panel — listens here.
   */
  onUnavailable?: (reason: VizUnavailableReason) => void;
}

/**
 * Why the host stopped showing the page and put a note in its place (TB-69):
 * - "unsupported": the page's `vizReady` carried a `modes` list without the
 *   mode this frame is in — a replay-only bundle shown live, or vice versa.
 * - "silent": the page posted nothing within `VIZ_SILENT_TIMEOUT_MS` of its
 *   `load` event. It threw before its handshake (no WebGL, say) or never
 *   speaks the protocol; either way nothing will be drawn.
 * - "idle": live mode, no `modes` declared, and no `readFile` / `listFiles` /
 *   `watchFile` within `VIZ_IDLE_TIMEOUT_MS` of `vizInit` — a page written for
 *   replay only, shown a mode it ignores.
 */
export type VizUnavailableReason = "unsupported" | "silent" | "idle";

/** How long after the frame's `load` event a page may stay quiet before it is presumed dead. */
export const VIZ_SILENT_TIMEOUT_MS = 5000;
/** How long after `vizInit` an undeclared live page may go without a file request before it is presumed replay-only. */
export const VIZ_IDLE_TIMEOUT_MS = 15000;

/**
 * The one-line note shown in a frame's place. `leafName` (the Overview has
 * one; the History modal does not) makes the sentence name the leaf.
 */
export function describeVizUnavailable(
  reason: VizUnavailableReason,
  mode: "live" | "replay",
  leafName?: string,
): string {
  const name = leafName?.trim();
  if (reason === "silent") {
    return name
      ? `The visualization for ${name} could not start on this machine.`
      : "The visualization could not start on this machine.";
  }
  if (mode === "replay") {
    return name
      ? `The visualization for ${name} has no replay view.`
      : "This visualization has no replay view.";
  }
  return name
    ? `${name} has no live view. Finished units can be replayed from History.`
    : "This leaf has no live view. Finished units can be replayed from History.";
}

/**
 * Custom-protocol name under which the Rust host serves the active bundle
 * (`register_uri_scheme_protocol("lettuce-viz", ...)` in `main.rs`). Its URL
 * form differs by platform — `http://lettuce-viz.localhost/...` on Windows,
 * `lettuce-viz://localhost/...` on macOS and Linux — so it must be built with
 * `convertFileSrc`, never hard-coded.
 */
export const VIZ_PROTOCOL = "lettuce-viz";

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
export function isValidRelPath(path: string): boolean {
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
export function VizFrame({
  vizBundlePath,
  workDir,
  leafSlug,
  paused,
  mode = "live",
  replayData,
  onUnavailable,
}: VizFrameProps) {
  const iframeRef = useRef<HTMLIFrameElement>(null);
  const watchIntervalsRef = useRef<Map<string, ReturnType<typeof setInterval>>>(new Map());
  const watchHashesRef = useRef<Map<string, number>>(new Map());
  const readyRef = useRef(false);
  const [vizReady, setVizReady] = useState(false);
  const [vizError, setVizError] = useState<string | null>(null);

  // Liveness bookkeeping (TB-69). `spokeRef`: the page posted anything at all;
  // `requestedRef`: it asked for a file; `declaredModesRef`: the `modes` list
  // from its `vizReady`, if any. The two timers turn silence into a verdict.
  const [unavailable, setUnavailable] = useState<VizUnavailableReason | null>(null);
  const spokeRef = useRef(false);
  const requestedRef = useRef(false);
  const declaredModesRef = useRef<string[] | null>(null);
  const idleArmedRef = useRef(false);
  const silentTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const idleTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const onUnavailableRef = useRef(onUnavailable);
  onUnavailableRef.current = onUnavailable;

  const isReplay = mode === "replay";

  const indexUrl = convertFileSrc("index.html", VIZ_PROTOCOL);

  const clearLivenessTimers = useCallback(() => {
    if (silentTimerRef.current) {
      clearTimeout(silentTimerRef.current);
      silentTimerRef.current = null;
    }
    if (idleTimerRef.current) {
      clearTimeout(idleTimerRef.current);
      idleTimerRef.current = null;
    }
  }, []);

  // Tell the Rust backend which directory to serve, then load via the custom protocol.
  useEffect(() => {
    setVizReady(false);
    setVizError(null);
    setUnavailable(null);
    readyRef.current = false;
    spokeRef.current = false;
    requestedRef.current = false;
    declaredModesRef.current = null;
    idleArmedRef.current = false;
    clearLivenessTimers();
    let cancelled = false;
    invoke("set_viz_base", { path: vizBundlePath })
      .then(() => {
        if (!cancelled) setVizReady(true);
      })
      .catch((e) => {
        if (!cancelled) setVizError(String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [vizBundlePath, clearLivenessTimers]);

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

  /** Stop showing the page: drop its watches and timers, put the note in its place. */
  const giveUp = useCallback((reason: VizUnavailableReason) => {
    clearLivenessTimers();
    clearAllWatches();
    setUnavailable((current) => current ?? reason);
  }, [clearLivenessTimers, clearAllWatches]);

  // Pause/unpause: clear watches when paused (live mode only).
  useEffect(() => {
    if (paused && !isReplay) {
      clearAllWatches();
    }
  }, [paused, clearAllWatches, isReplay]);

  // Cleanup on unmount.
  useEffect(() => {
    return () => {
      clearAllWatches();
      clearLivenessTimers();
    };
  }, [clearAllWatches, clearLivenessTimers]);

  // Report a verdict to the parent once.
  useEffect(() => {
    if (unavailable) onUnavailableRef.current?.(unavailable);
  }, [unavailable]);

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

    // A live page that declared nothing gets one idle window to ask for a
    // file; one that never does is a replay-only page ignoring live mode.
    if (!isReplay && !idleArmedRef.current) {
      idleArmedRef.current = true;
      if (!declaredModesRef.current && !requestedRef.current) {
        idleTimerRef.current = setTimeout(() => {
          idleTimerRef.current = null;
          if (!requestedRef.current && !declaredModesRef.current) giveUp("idle");
        }, VIZ_IDLE_TIMEOUT_MS);
      }
    }
  }, [workDir, leafSlug, postToIframe, isReplay, replayData, giveUp]);

  // Register message listener in useLayoutEffect to ensure it's active before
  // the iframe can fire events (fixes the vizReady/vizInit race condition).
  useLayoutEffect(() => {
    const noteRequest = () => {
      requestedRef.current = true;
      if (idleTimerRef.current) {
        clearTimeout(idleTimerRef.current);
        idleTimerRef.current = null;
      }
    };

    const handler = (event: MessageEvent) => {
      if (event.source !== iframeRef.current?.contentWindow) return;

      const msg = event.data;
      if (!msg || typeof msg.type !== "string") return;

      // Any message at all proves the page is running.
      spokeRef.current = true;
      if (silentTimerRef.current) {
        clearTimeout(silentTimerRef.current);
        silentTimerRef.current = null;
      }

      switch (msg.type) {
        case "vizReady": {
          const modes = Array.isArray(msg.modes)
            ? (msg.modes as unknown[]).filter((m): m is string => typeof m === "string")
            : null;
          if (modes) {
            declaredModesRef.current = modes;
            if (!modes.includes(mode)) {
              giveUp("unsupported");
              break;
            }
            // A page that says it supports this mode is believed: no idle test.
            if (idleTimerRef.current) {
              clearTimeout(idleTimerRef.current);
              idleTimerRef.current = null;
            }
          }
          sendInit();
          break;
        }
        case "readFile":
          noteRequest();
          if (msg.id && msg.path) handleReadFile(msg.id, msg.path);
          break;
        case "listFiles":
          noteRequest();
          if (msg.id) handleListFiles(msg.id, msg.pattern);
          break;
        case "watchFile":
          noteRequest();
          if (msg.path) handleWatchFile(msg.path, msg.interval);
          break;
        case "unwatchFile":
          if (msg.path) handleUnwatchFile(msg.path);
          break;
      }
    };

    window.addEventListener("message", handler);

    return () => window.removeEventListener("message", handler);
  }, [mode, sendInit, giveUp, handleReadFile, handleListFiles, handleWatchFile, handleUnwatchFile]);

  // Fallback: if iframe loads from cache and vizReady was never received, send
  // vizInit after a short delay. The same moment arms the silence test: a page
  // that has said nothing by now has VIZ_SILENT_TIMEOUT_MS to say anything.
  const handleIframeLoad = useCallback(() => {
    setTimeout(() => {
      if (!readyRef.current) {
        sendInit();
      }
    }, 150);
    if (!spokeRef.current && !silentTimerRef.current) {
      silentTimerRef.current = setTimeout(() => {
        silentTimerRef.current = null;
        if (!spokeRef.current) giveUp("silent");
      }, VIZ_SILENT_TIMEOUT_MS);
    }
  }, [sendInit, giveUp]);

  if (vizError) {
    // The bundle directory could not be opened — typically because the unit's
    // work directory (which holds the extracted bundle) has been removed.
    return (
      <div
        data-testid="viz-error"
        role="alert"
        style={{
          width: "100%",
          height: "100%",
          background: "#0a0a0f",
          color: "#a1a1aa",
          display: "flex",
          flexDirection: "column",
          alignItems: "center",
          justifyContent: "center",
          gap: "0.5rem",
          padding: "1.5rem",
          textAlign: "center",
          fontSize: "0.875rem",
        }}
      >
        <span style={{ color: "#e4e4e7", fontWeight: 500 }}>
          {isReplay
            ? "The visualization files for this result are no longer on this machine."
            : "The visualization files could not be opened."}
        </span>
        <code style={{ fontSize: "0.75rem", wordBreak: "break-all" }}>{vizError}</code>
      </div>
    );
  }

  if (unavailable) {
    // The page loaded but will not draw in this mode (or at all): the note
    // stands in for it. Not an alert — nothing is wrong with the unit.
    return (
      <div
        data-testid="viz-unavailable"
        data-reason={unavailable}
        role="status"
        style={{
          width: "100%",
          height: "100%",
          background: "#0a0a0f",
          color: "#a1a1aa",
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          padding: "1.5rem",
          textAlign: "center",
          fontSize: "0.875rem",
        }}
      >
        {describeVizUnavailable(unavailable, mode)}
      </div>
    );
  }

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
