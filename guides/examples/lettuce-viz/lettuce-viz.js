/**
 * lettuce-viz SDK — Lightweight helper for Lettuce viz bundles.
 *
 * Provides convenience wrappers around the postMessage protocol used to
 * communicate between a viz bundle (running in a sandboxed iframe) and
 * the Lettuce desktop app or dashboard.
 *
 * Usage:
 *   import { createVizClient } from './lettuce-viz.js';
 *   const viz = createVizClient({ modes: ['live', 'replay'] });
 *   const init = await viz.ready();
 *   // init = { mode: 'live' | 'replay', leafSlug, params }
 *
 * This SDK is NOT required — viz bundles can use raw postMessage directly.
 */

/**
 * Create a viz client that communicates with the Lettuce host via postMessage.
 * @param {object} [options]
 * @param {("live"|"replay")[]} [options.modes] - The modes this page implements.
 *   Sent with every `vizReady`; a host shows the page only in a declared mode
 *   and puts a note in its place otherwise. Omit it to claim both modes — the
 *   desktop app then expects a live page to ask for a file within 15 s of
 *   `vizInit` (see the README).
 * @returns {object} Viz client API
 */
export function createVizClient(options = {}) {
  const _modes = Array.isArray(options.modes) ? options.modes.slice() : null;
  let _initResolve = null;
  let _initData = null;
  const _pendingRequests = new Map(); // id -> { resolve, reject }
  const _fileWatchers = new Map(); // path -> callback
  let _replayHandler = null;
  let _idCounter = 0;
  let _vizReadyRetryTimer = null;
  let _vizReadyRetryCount = 0;
  let _initialized = false;

  function nextId() {
    return `viz-${++_idCounter}-${Date.now()}`;
  }

  // Single message dispatcher.
  window.addEventListener("message", (event) => {
    const msg = event.data;
    if (!msg || typeof msg.type !== "string") return;

    switch (msg.type) {
      case "vizInit":
        _initialized = true;
        if (_vizReadyRetryTimer) {
          clearTimeout(_vizReadyRetryTimer);
          _vizReadyRetryTimer = null;
        }
        _initData = {
          mode: msg.mode,
          workDir: msg.workDir,
          leafSlug: msg.leafSlug,
          params: msg.params || {},
        };
        if (_initResolve) {
          _initResolve(_initData);
          _initResolve = null;
        }
        break;

      case "readFileResult": {
        const pending = _pendingRequests.get(msg.id);
        if (pending) {
          _pendingRequests.delete(msg.id);
          if (msg.error) {
            pending.reject(new Error(msg.error));
          } else {
            // Convert number array to ArrayBuffer if needed.
            const data = msg.data;
            if (Array.isArray(data)) {
              const buf = new Uint8Array(data).buffer;
              pending.resolve(buf);
            } else if (data instanceof ArrayBuffer) {
              pending.resolve(data);
            } else {
              pending.resolve(data);
            }
          }
        }
        break;
      }

      case "listFilesResult": {
        const pending = _pendingRequests.get(msg.id);
        if (pending) {
          _pendingRequests.delete(msg.id);
          if (msg.error) {
            pending.reject(new Error(msg.error));
          } else {
            pending.resolve(msg.files || []);
          }
        }
        break;
      }

      case "fileChanged": {
        const cb = _fileWatchers.get(msg.path);
        if (cb) {
          const data = msg.data;
          if (Array.isArray(data)) {
            cb(new Uint8Array(data).buffer);
          } else {
            cb(data);
          }
        }
        break;
      }

      case "replayData":
        if (_replayHandler) {
          _replayHandler(msg);
        }
        break;
    }
  });

  return {
    /**
     * Wait for initialization from the host. Sends vizReady signal.
     * @returns {Promise<{mode: string, leafSlug: string, params: object}>}
     */
    ready() {
      if (_initData) return Promise.resolve(_initData);

      // Signal readiness to the parent, with retries in case the parent's
      // message listener isn't registered yet (race condition on fast loads).
      function sendVizReady() {
        window.parent.postMessage(
          _modes ? { type: "vizReady", modes: _modes } : { type: "vizReady" },
          "*"
        );
        _vizReadyRetryCount++;
        if (_vizReadyRetryCount < 5 && !_initialized) {
          _vizReadyRetryTimer = setTimeout(sendVizReady, 200);
        }
      }
      _vizReadyRetryCount = 0;
      sendVizReady();

      return new Promise((resolve) => {
        _initResolve = resolve;
      });
    },

    /**
     * Read a file from the work directory (live mode only).
     * @param {string} path - Relative path within the work directory
     * @returns {Promise<ArrayBuffer>}
     */
    readFile(path) {
      const id = nextId();
      return new Promise((resolve, reject) => {
        _pendingRequests.set(id, { resolve, reject });
        window.parent.postMessage({ type: "readFile", id, path }, "*");
      });
    },

    /**
     * List files in the work directory (live mode only).
     * @param {string} [pattern] - Optional glob pattern (e.g., "*.json")
     * @returns {Promise<string[]>}
     */
    listFiles(pattern) {
      const id = nextId();
      return new Promise((resolve, reject) => {
        _pendingRequests.set(id, { resolve, reject });
        window.parent.postMessage({ type: "listFiles", id, pattern }, "*");
      });
    },

    /**
     * Start watching a file for changes (live mode only).
     * @param {string} path - Relative path to watch
     * @param {number} intervalMs - Polling interval in ms (default 1000)
     * @param {function} callback - Called with ArrayBuffer on file change
     */
    watchFile(path, intervalMs, callback) {
      _fileWatchers.set(path, callback);
      window.parent.postMessage(
        { type: "watchFile", path, interval: intervalMs || 1000 },
        "*"
      );
    },

    /**
     * Stop watching a file.
     * @param {string} path - Path to stop watching
     */
    unwatchFile(path) {
      _fileWatchers.delete(path);
      window.parent.postMessage({ type: "unwatchFile", path }, "*");
    },

    /**
     * Register a handler for replay data (dashboard mode).
     * The callback receives the full replayData message object, which includes
     * `type: "replayData"` plus all keys spread from the result's `output_data`.
     * The data shape is viz-type-specific (e.g., N-body uses `keyframe_snapshots` and `time_series`).
     * @param {function} callback - Called with the raw replayData message
     */
    onReplayData(callback) {
      _replayHandler = callback;
    },
  };
}
