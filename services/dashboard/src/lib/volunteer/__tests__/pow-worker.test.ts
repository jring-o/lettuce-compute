// pow-worker tests — the worker runs in a DedicatedWorkerGlobalScope, so we mock
// self.postMessage / self.onmessage and drive it via the dynamic-import pattern
// (mirroring volunteer-worker.test.ts). The solve runs for real at a low difficulty, so a
// posted "solved" nonce is checked to genuinely satisfy the solution rule.

import type { SolverRequestMessage, SolverResponseMessage } from "../types";
import { base64urlEncode, bytesToHex } from "../identity";
import { leadingZeroBits } from "../pow";
import { sha256 } from "../sha256";

const postedMessages: SolverResponseMessage[] = [];
let onMessageHandler:
  | ((event: MessageEvent<SolverRequestMessage>) => void)
  | null = null;

Object.defineProperty(global, "self", {
  value: {
    postMessage: jest.fn((msg: SolverResponseMessage) => {
      postedMessages.push(msg);
    }),
    set onmessage(
      handler: ((event: MessageEvent<SolverRequestMessage>) => void) | null
    ) {
      onMessageHandler = handler;
    },
    get onmessage() {
      return onMessageHandler;
    },
  },
  writable: true,
  configurable: true,
});

function seqBytes(start: number, n: number): Uint8Array {
  const b = new Uint8Array(n);
  for (let i = 0; i < n; i++) b[i] = (start + i) & 0xff;
  return b;
}

function manualDigest(
  challenge: Uint8Array,
  publicKey: Uint8Array,
  nonce: number
): Uint8Array {
  const nb = new Uint8Array(8);
  new DataView(nb.buffer).setUint32(4, nonce, false);
  const preimage = new Uint8Array(72);
  preimage.set(challenge, 0);
  preimage.set(publicKey, 32);
  preimage.set(nb, 64);
  return sha256(preimage);
}

async function loadWorker(): Promise<void> {
  jest.resetModules();
  postedMessages.length = 0;
  onMessageHandler = null;
  await import("../pow-worker");
}

function send(msg: SolverRequestMessage): void {
  onMessageHandler?.({ data: msg } as MessageEvent<SolverRequestMessage>);
}

describe("pow-worker", () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it("solves a low-difficulty challenge and posts a valid nonce", async () => {
    await loadWorker();

    const challenge = seqBytes(0, 32); // 0x00..0x1f
    const publicKey = seqBytes(32, 32); // 0x20..0x3f
    send({
      type: "solve",
      challengeHex: bytesToHex(challenge),
      publicKeyBase64url: base64urlEncode(publicKey),
      difficultyBits: 8,
    });

    const solved = postedMessages.find((m) => m.type === "solved");
    expect(solved).toBeDefined();
    if (solved && solved.type === "solved") {
      const nonce = Number(solved.nonce);
      expect(solved.attempts).toBe(nonce + 1);
      // The posted nonce must genuinely clear the difficulty, recomputed independently.
      expect(leadingZeroBits(manualDigest(challenge, publicKey, nonce))).toBeGreaterThanOrEqual(8);
    }
  });

  it("posts an error for malformed challenge hex", async () => {
    await loadWorker();

    send({
      type: "solve",
      challengeHex: "zz",
      publicKeyBase64url: base64urlEncode(seqBytes(32, 32)),
      difficultyBits: 8,
    });

    const error = postedMessages.find((m) => m.type === "error");
    expect(error).toBeDefined();
    expect(postedMessages.find((m) => m.type === "solved")).toBeUndefined();
  });

  it("ignores non-solve messages", async () => {
    await loadWorker();

    // @ts-expect-error — exercising an unexpected message shape at runtime.
    send({ type: "noop" });

    expect(postedMessages).toHaveLength(0);
  });
});
