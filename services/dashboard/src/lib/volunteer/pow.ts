// Registration proof-of-work solver core. The solution rule is the cross-language
// contract shared by golden byte vector with the head's pow package
// (services/infrastructure/pow) and pinned case-for-case by pow.test.ts:
//
//   digest = SHA-256(challenge[32] || publicKey[32] || nonce as 8 big-endian bytes)
//   valid  = leadingZeroBits(digest) >= difficultyBits
//
// Any change to the preimage layout, nonce encoding, or bit counting here orphans every
// shipped solver, so the golden pins must move in lockstep with the head.

import { compress, SHA256_INIT } from "./sha256";
import type { PowSolution } from "./types";

// onProgress is invoked every POW_PROGRESS_INTERVAL attempts. The solve loop is
// synchronous (it cannot use timers), so progress is reported on an attempt-count cadence.
const POW_PROGRESS_INTERVAL = 65536;

// hexToBytes decodes a hex string to bytes, rejecting odd lengths and non-hex characters
// (identity.ts has bytesToHex and base64url codecs but no hex decode; challenge_hex needs
// one).
export function hexToBytes(hex: string): Uint8Array {
  if (hex.length % 2 !== 0) {
    throw new Error(`hex string has odd length ${hex.length}`);
  }
  if (!/^[0-9a-fA-F]*$/.test(hex)) {
    throw new Error("hex string contains a non-hex character");
  }
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = Number.parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

// leadingZeroBits counts the most-significant zero bits before the first set bit: whole
// zero bytes times eight, then the leading zeros within the first non-zero byte. This is
// the exact quantity difficulty targets are compared against.
export function leadingZeroBits(digest: Uint8Array): number {
  let n = 0;
  for (let i = 0; i < digest.length; i++) {
    const b = digest[i];
    if (b === 0) {
      n += 8;
      continue;
    }
    // clz32 counts leading zeros in a 32-bit word; b occupies the low 8 bits, so its
    // 24 high zero bits are subtracted off.
    n += Math.clz32(b) - 24;
    break;
  }
  return n;
}

// solvePow scans nonces from 0 and returns the first one whose digest clears
// difficultyBits leading zero bits. Scanning from 0 makes the result the cross-language
// reference solution (the golden vectors pin the exact first nonce). challenge and
// publicKey MUST be 32 bytes each — the head schema guarantees this and the midstate
// optimization below depends on it.
export function solvePow(
  challenge: Uint8Array,
  publicKey: Uint8Array,
  difficultyBits: number,
  onProgress?: (attempts: number) => void
): PowSolution {
  if (challenge.length !== 32) {
    throw new Error(`challenge must be 32 bytes, got ${challenge.length}`);
  }
  if (publicKey.length !== 32) {
    throw new Error(`publicKey must be 32 bytes, got ${publicKey.length}`);
  }

  // challenge || publicKey is exactly one 64-byte SHA-256 block. Compress it once; every
  // attempt resumes from this midstate and only recompresses the final block.
  const firstBlock = new Uint8Array(64);
  firstBlock.set(challenge, 0);
  firstBlock.set(publicKey, 32);
  const midstate = new Uint32Array(SHA256_INIT);
  compress(midstate, firstBlock, 0);

  // The final block is the SHA-256 padding of a 72-byte message: the 8-byte nonce, then
  // 0x80, zero fill, and the 64-bit big-endian bit length (72 * 8 = 576 = 0x0240). Only
  // the first eight bytes (the nonce) change per attempt.
  const finalBlock = new Uint8Array(64);
  finalBlock[8] = 0x80;
  finalBlock[62] = 0x02;
  finalBlock[63] = 0x40;
  const finalView = new DataView(finalBlock.buffer);

  const state = new Uint32Array(8);
  const digest = new Uint8Array(32);
  const digestView = new DataView(digest.buffer);

  for (let nonce = 0; ; nonce++) {
    // nonce as 8 big-endian bytes. Attempts stay far below 2^53, so the hi/lo split is
    // exact.
    finalView.setUint32(0, Math.floor(nonce / 0x100000000), false);
    finalView.setUint32(4, nonce % 0x100000000, false);

    state.set(midstate);
    compress(state, finalBlock, 0);
    for (let i = 0; i < 8; i++) {
      digestView.setUint32(i * 4, state[i], false);
    }

    if (leadingZeroBits(digest) >= difficultyBits) {
      return { nonce: String(nonce), attempts: nonce + 1 };
    }

    const attempts = nonce + 1;
    if (onProgress && attempts % POW_PROGRESS_INTERVAL === 0) {
      onProgress(attempts);
    }
  }
}
