// pow-solver wrapper tests — mocks the Worker constructor (mirroring the MockWorker
// precedent in pool-manager.test.ts) to assert the wrapper posts the solve request,
// settles exactly once on each terminal event, always terminates the worker, and rejects
// cancels with PowSolveCancelledError.

import type { SolverResponseMessage } from "../types";

// Mock Worker constructor.
class MockWorker {
  onmessage: ((event: MessageEvent<SolverResponseMessage>) => void) | null = null;
  onerror: ((event: ErrorEvent) => void) | null = null;
  onmessageerror: ((event: MessageEvent) => void) | null = null;
  postMessage = jest.fn();
  terminate = jest.fn();

  emit(msg: SolverResponseMessage): void {
    this.onmessage?.({ data: msg } as MessageEvent<SolverResponseMessage>);
  }

  emitError(message: string): void {
    this.onerror?.({ message } as ErrorEvent);
  }

  emitMessageError(): void {
    this.onmessageerror?.({} as MessageEvent);
  }
}

const createdWorkers: MockWorker[] = [];

// eslint-disable-next-line @typescript-eslint/no-explicit-any
(global as any).Worker = jest.fn().mockImplementation(() => {
  const w = new MockWorker();
  createdWorkers.push(w);
  return w;
});

import { PowSolveCancelledError, solveInWorker } from "../pow-solver";

describe("solveInWorker", () => {
  beforeEach(() => {
    jest.clearAllMocks();
    createdWorkers.length = 0;
  });

  it("posts the solve request with the correct payload", () => {
    solveInWorker("00ff", "pubkey-b64url", 20);

    expect(createdWorkers).toHaveLength(1);
    expect(createdWorkers[0].postMessage).toHaveBeenCalledWith({
      type: "solve",
      challengeHex: "00ff",
      publicKeyBase64url: "pubkey-b64url",
      difficultyBits: 20,
    });
  });

  it("resolves with the PowSolution and terminates on 'solved'", async () => {
    const { promise } = solveInWorker("00ff", "pubkey-b64url", 20);
    createdWorkers[0].emit({ type: "solved", nonce: "12345", attempts: 12346 });

    await expect(promise).resolves.toEqual({ nonce: "12345", attempts: 12346 });
    expect(createdWorkers[0].terminate).toHaveBeenCalledTimes(1);
  });

  it("forwards progress to onProgress without settling", async () => {
    const onProgress = jest.fn();
    const { promise } = solveInWorker("00ff", "pubkey-b64url", 20, onProgress);

    createdWorkers[0].emit({ type: "pow-progress", attempts: 65536 });
    createdWorkers[0].emit({ type: "pow-progress", attempts: 131072 });
    expect(onProgress).toHaveBeenNthCalledWith(1, 65536);
    expect(onProgress).toHaveBeenNthCalledWith(2, 131072);
    expect(createdWorkers[0].terminate).not.toHaveBeenCalled();

    createdWorkers[0].emit({ type: "solved", nonce: "7", attempts: 131100 });
    await expect(promise).resolves.toEqual({ nonce: "7", attempts: 131100 });
  });

  it("rejects and terminates on an 'error' message", async () => {
    const { promise } = solveInWorker("00ff", "pubkey-b64url", 20);
    createdWorkers[0].emit({ type: "error", message: "bad hex" });

    await expect(promise).rejects.toThrow("bad hex");
    expect(createdWorkers[0].terminate).toHaveBeenCalledTimes(1);
  });

  it("rejects and terminates on worker onerror", async () => {
    const { promise } = solveInWorker("00ff", "pubkey-b64url", 20);
    createdWorkers[0].emitError("worker crashed");

    await expect(promise).rejects.toThrow("worker crashed");
    expect(createdWorkers[0].terminate).toHaveBeenCalledTimes(1);
  });

  it("rejects and terminates on worker onmessageerror", async () => {
    const { promise } = solveInWorker("00ff", "pubkey-b64url", 20);
    createdWorkers[0].emitMessageError();

    await expect(promise).rejects.toThrow(/message error/);
    expect(createdWorkers[0].terminate).toHaveBeenCalledTimes(1);
  });

  it("cancel() terminates and rejects with PowSolveCancelledError", async () => {
    const { promise, cancel } = solveInWorker("00ff", "pubkey-b64url", 20);
    cancel();

    await expect(promise).rejects.toBeInstanceOf(PowSolveCancelledError);
    expect(createdWorkers[0].terminate).toHaveBeenCalledTimes(1);
  });

  it("ignores events that arrive after the promise has settled", async () => {
    const onProgress = jest.fn();
    const { promise } = solveInWorker("00ff", "pubkey-b64url", 20, onProgress);

    createdWorkers[0].emit({ type: "solved", nonce: "1", attempts: 2 });
    await expect(promise).resolves.toEqual({ nonce: "1", attempts: 2 });

    // A late error, progress, and second solved must not re-settle, re-terminate, or
    // fire progress.
    createdWorkers[0].emit({ type: "error", message: "late error" });
    createdWorkers[0].emit({ type: "pow-progress", attempts: 999 });
    createdWorkers[0].emit({ type: "solved", nonce: "2", attempts: 3 });

    await expect(promise).resolves.toEqual({ nonce: "1", attempts: 2 });
    expect(createdWorkers[0].terminate).toHaveBeenCalledTimes(1);
    expect(onProgress).not.toHaveBeenCalled();
  });

  it("cancel() after settle is a no-op", async () => {
    const { promise, cancel } = solveInWorker("00ff", "pubkey-b64url", 20);
    createdWorkers[0].emit({ type: "solved", nonce: "1", attempts: 2 });
    await expect(promise).resolves.toEqual({ nonce: "1", attempts: 2 });

    cancel();
    expect(createdWorkers[0].terminate).toHaveBeenCalledTimes(1);
  });
});
