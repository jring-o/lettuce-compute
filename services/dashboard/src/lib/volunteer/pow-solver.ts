// Main-thread wrapper around the one-shot pow-worker. It is the SINGLE owner of the
// worker's lifecycle: it spawns the worker, hands it the solve request, and terminate()s
// it on every settle path — solved, error, and cancel — so there is no leak path.
// Cancellation is terminate()-only because the worker's solve loop is synchronous and can
// never process a postMessage'd abort.

import type {
  PowSolution,
  SolverRequestMessage,
  SolverResponseMessage,
} from "./types";

export class PowSolveCancelledError extends Error {
  constructor(message = "proof-of-work solve cancelled") {
    super(message);
    this.name = "PowSolveCancelledError";
  }
}

export interface PowSolveHandle {
  promise: Promise<PowSolution>;
  cancel: () => void;
}

export function solveInWorker(
  challengeHex: string,
  publicKeyBase64url: string,
  difficultyBits: number,
  onProgress?: (attempts: number) => void
): PowSolveHandle {
  const worker = new Worker(new URL("./pow-worker.ts", import.meta.url), {
    type: "module",
  });

  let settled = false;
  let resolveFn!: (solution: PowSolution) => void;
  let rejectFn!: (err: Error) => void;
  const promise = new Promise<PowSolution>((resolve, reject) => {
    resolveFn = resolve;
    rejectFn = reject;
  });

  // Settle exactly once, always terminating the worker first.
  function settleResolve(solution: PowSolution): void {
    if (settled) return;
    settled = true;
    worker.terminate();
    resolveFn(solution);
  }
  function settleReject(err: Error): void {
    if (settled) return;
    settled = true;
    worker.terminate();
    rejectFn(err);
  }

  worker.onmessage = (event: MessageEvent<SolverResponseMessage>) => {
    const msg = event.data;
    switch (msg.type) {
      case "solved":
        settleResolve({ nonce: msg.nonce, attempts: msg.attempts });
        break;
      case "pow-progress":
        if (!settled) onProgress?.(msg.attempts);
        break;
      case "error":
        settleReject(new Error(msg.message));
        break;
    }
  };

  worker.onerror = (event: ErrorEvent) => {
    settleReject(new Error(event.message || "pow worker error"));
  };

  worker.onmessageerror = () => {
    settleReject(new Error("pow worker message error"));
  };

  const request: SolverRequestMessage = {
    type: "solve",
    challengeHex,
    publicKeyBase64url,
    difficultyBits,
  };
  worker.postMessage(request);

  return {
    promise,
    cancel: () => settleReject(new PowSolveCancelledError()),
  };
}
