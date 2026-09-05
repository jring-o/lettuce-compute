/**
 * Tests for the lettuce-viz SDK retry logic (S105).
 *
 * The SDK lives at guides/examples/lettuce-viz/lettuce-viz.js.
 * We test it in jsdom via relative import.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

// We dynamically import the module to get fresh state per test.
// The SDK registers a global message listener on module load,
// so we need to isolate each test.

async function loadSdk() {
  // Use vi.importActual with the relative path from this file to the SDK
  const mod = await import(
    /* @vite-ignore */
    "../../../../guides/examples/lettuce-viz/lettuce-viz.js"
  );
  return mod.createVizClient;
}

describe("lettuce-viz SDK", () => {
  let postMessageSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    vi.useFakeTimers();
    // Spy on parent postMessage (in jsdom, window.parent === window)
    postMessageSpy = vi.spyOn(window.parent, "postMessage");
  });

  afterEach(() => {
    vi.useRealTimers();
    postMessageSpy.mockRestore();
  });

  it("ready() sends vizReady message to parent", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    // Call ready() — it should immediately post vizReady
    client.ready();

    expect(postMessageSpy).toHaveBeenCalledWith(
      { type: "vizReady" },
      "*"
    );
  });

  it("ready() retries vizReady up to 5 times at 200ms intervals", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    client.ready();

    // First call is immediate
    expect(postMessageSpy).toHaveBeenCalledTimes(1);

    // Advance 200ms — second retry
    vi.advanceTimersByTime(200);
    expect(postMessageSpy).toHaveBeenCalledTimes(2);

    // Advance another 200ms — third retry
    vi.advanceTimersByTime(200);
    expect(postMessageSpy).toHaveBeenCalledTimes(3);

    // Advance 200ms — fourth retry
    vi.advanceTimersByTime(200);
    expect(postMessageSpy).toHaveBeenCalledTimes(4);

    // Advance 200ms — fifth retry (count reaches 5, stops)
    vi.advanceTimersByTime(200);
    expect(postMessageSpy).toHaveBeenCalledTimes(5);

    // Advance another 200ms — no more retries
    vi.advanceTimersByTime(200);
    expect(postMessageSpy).toHaveBeenCalledTimes(5);
  });

  it("ready() stops retrying when vizInit is received", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const readyPromise = client.ready();

    // First call is immediate
    expect(postMessageSpy).toHaveBeenCalledTimes(1);

    // Advance 200ms — second retry
    vi.advanceTimersByTime(200);
    expect(postMessageSpy).toHaveBeenCalledTimes(2);

    // Simulate vizInit from parent
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "vizInit",
          mode: "live",
          workDir: "/work",
          leafSlug: "test-leaf",
          params: { foo: "bar" },
        },
      })
    );

    // The promise should resolve
    const initData = await readyPromise;
    expect(initData.mode).toBe("live");
    expect(initData.leafSlug).toBe("test-leaf");
    expect(initData.params.foo).toBe("bar");

    // No more retries after vizInit
    vi.advanceTimersByTime(1000);
    expect(postMessageSpy).toHaveBeenCalledTimes(2); // no additional calls
  });

  it("ready() resolves immediately if vizInit was already received", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    // First call to ready triggers vizReady posting
    const readyPromise1 = client.ready();

    // Simulate vizInit
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "vizInit",
          mode: "replay",
          workDir: "/work",
          leafSlug: "replay-leaf",
          params: {},
        },
      })
    );

    const initData = await readyPromise1;
    expect(initData.mode).toBe("replay");

    // Second call should resolve immediately
    const initData2 = await client.ready();
    expect(initData2.mode).toBe("replay");
    expect(initData2.leafSlug).toBe("replay-leaf");
  });

  it("readFile sends postMessage and resolves with ArrayBuffer", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const filePromise = client.readFile("data.json");

    // Find the posted message for readFile
    const readFileCall = postMessageSpy.mock.calls.find(
      ([msg]) => msg?.type === "readFile"
    );
    expect(readFileCall).toBeTruthy();
    const reqId = readFileCall![0].id;
    expect(readFileCall![0].path).toBe("data.json");

    // Simulate response
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "readFileResult",
          id: reqId,
          data: [72, 101, 108, 108, 111], // "Hello"
        },
      })
    );

    const result = await filePromise;
    expect(result).toBeInstanceOf(ArrayBuffer);
    const text = new TextDecoder().decode(new Uint8Array(result as ArrayBuffer));
    expect(text).toBe("Hello");
  });

  it("listFiles sends postMessage and resolves with file list", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const listPromise = client.listFiles("*.json");

    const listCall = postMessageSpy.mock.calls.find(
      ([msg]) => msg?.type === "listFiles"
    );
    expect(listCall).toBeTruthy();
    const reqId = listCall![0].id;

    // Simulate response
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "listFilesResult",
          id: reqId,
          files: ["a.json", "b.json"],
        },
      })
    );

    const result = await listPromise;
    expect(result).toEqual(["a.json", "b.json"]);
  });

  it("watchFile dispatches fileChanged callback on change", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const callback = vi.fn();
    client.watchFile("output.bin", 500, callback);

    // Verify watchFile message was sent
    const watchCall = postMessageSpy.mock.calls.find(
      ([msg]) => msg?.type === "watchFile"
    );
    expect(watchCall).toBeTruthy();
    expect(watchCall![0].path).toBe("output.bin");
    expect(watchCall![0].interval).toBe(500);

    // Simulate fileChanged from host
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "fileChanged",
          path: "output.bin",
          data: [1, 2, 3],
        },
      })
    );

    expect(callback).toHaveBeenCalledTimes(1);
    const arg = callback.mock.calls[0][0];
    expect(arg).toBeInstanceOf(ArrayBuffer);
  });

  // --- S105 coverage gaps: error paths, unwatchFile, onReplayData ---

  it("readFile rejects with Error when readFileResult has error", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const filePromise = client.readFile("missing.json");

    // Find the posted readFile message to get the request id
    const readFileCall = postMessageSpy.mock.calls.find(
      ([msg]) => msg?.type === "readFile"
    );
    expect(readFileCall).toBeTruthy();
    const reqId = readFileCall![0].id;

    // Simulate error response
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "readFileResult",
          id: reqId,
          data: null,
          error: "file not found",
        },
      })
    );

    await expect(filePromise).rejects.toThrow("file not found");
  });

  it("listFiles rejects with Error when listFilesResult has error", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const listPromise = client.listFiles("*.csv");

    const listCall = postMessageSpy.mock.calls.find(
      ([msg]) => msg?.type === "listFiles"
    );
    expect(listCall).toBeTruthy();
    const reqId = listCall![0].id;

    // Simulate error response
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "listFilesResult",
          id: reqId,
          files: [],
          error: "permission denied",
        },
      })
    );

    await expect(listPromise).rejects.toThrow("permission denied");
  });

  it("unwatchFile sends unwatchFile message and removes callback", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const callback = vi.fn();
    client.watchFile("data.bin", 1000, callback);

    // Unwatch
    client.unwatchFile("data.bin");

    // Verify unwatchFile message was sent
    const unwatchCall = postMessageSpy.mock.calls.find(
      ([msg]) => msg?.type === "unwatchFile"
    );
    expect(unwatchCall).toBeTruthy();
    expect(unwatchCall![0].path).toBe("data.bin");

    // Subsequent fileChanged should NOT trigger the callback
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "fileChanged",
          path: "data.bin",
          data: [4, 5, 6],
        },
      })
    );

    expect(callback).not.toHaveBeenCalled();
  });

  it("onReplayData registers handler that receives replayData messages", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const handler = vi.fn();
    client.onReplayData(handler);

    // Simulate replayData from host
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "replayData",
          keyframe_snapshots: [[1, 2], [3, 4]],
          time_series: { step: [0, 1] },
        },
      })
    );

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler.mock.calls[0][0].type).toBe("replayData");
    expect(handler.mock.calls[0][0].keyframe_snapshots).toEqual([[1, 2], [3, 4]]);
  });

  it("listFiles resolves with empty array when no files field in response", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    const listPromise = client.listFiles();

    const listCall = postMessageSpy.mock.calls.find(
      ([msg]) => msg?.type === "listFiles"
    );
    const reqId = listCall![0].id;

    // Simulate response with no files field
    window.dispatchEvent(
      new MessageEvent("message", {
        data: {
          type: "listFilesResult",
          id: reqId,
          // files omitted
        },
      })
    );

    const result = await listPromise;
    expect(result).toEqual([]);
  });

  // --- TB-69: a page may declare which modes it implements ---

  it("ready() sends the declared modes with every vizReady, retries included (TB-69)", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient({ modes: ["replay"] });

    client.ready();

    expect(postMessageSpy).toHaveBeenCalledWith(
      { type: "vizReady", modes: ["replay"] },
      "*"
    );

    vi.advanceTimersByTime(200);
    expect(postMessageSpy).toHaveBeenCalledTimes(2);
    expect(postMessageSpy).toHaveBeenLastCalledWith(
      { type: "vizReady", modes: ["replay"] },
      "*"
    );
  });

  it("ready() without modes posts the bare vizReady, so an undeclared page claims both modes (TB-69)", async () => {
    const createVizClient = await loadSdk();
    const client = createVizClient();

    client.ready();

    const [msg] = postMessageSpy.mock.calls[0];
    expect(msg).toEqual({ type: "vizReady" });
    expect("modes" in msg).toBe(false);
  });
});
