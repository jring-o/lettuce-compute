// One-shot Web Worker entry for registration proof-of-work. Spawned per solve; the caller
// (pow-solver.ts) owns the lifecycle and terminate()s it. The solve loop is synchronous,
// so this worker never processes an incoming message mid-solve — cancellation is
// terminate()-only from the main thread, and there is deliberately no "ready" handshake
// (the Worker spec buffers messages posted before the script finishes loading).

import { base64urlDecode } from "./identity";
import { hexToBytes, solvePow } from "./pow";
import type { SolverRequestMessage, SolverResponseMessage } from "./types";

const ctx = self as unknown as {
  postMessage(msg: unknown): void;
  onmessage: ((ev: MessageEvent) => void) | null;
};

function post(msg: SolverResponseMessage): void {
  ctx.postMessage(msg);
}

ctx.onmessage = (event: MessageEvent<SolverRequestMessage>) => {
  const msg = event.data;
  if (msg.type !== "solve") return;

  try {
    const challenge = hexToBytes(msg.challengeHex);
    const publicKey = base64urlDecode(msg.publicKeyBase64url);
    const solution = solvePow(
      challenge,
      publicKey,
      msg.difficultyBits,
      (attempts) => post({ type: "pow-progress", attempts })
    );
    post({ type: "solved", nonce: solution.nonce, attempts: solution.attempts });
  } catch (err) {
    post({ type: "error", message: err instanceof Error ? err.message : String(err) });
  }
};
