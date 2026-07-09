// Synchronous, dependency-free SHA-256 over Uint8Array (FIPS 180-4). The registration
// proof-of-work solver runs ~2^20 hashes in a tight loop, so it cannot use the async
// crypto.subtle digest; this implementation stays on 32-bit typed-array arithmetic with
// no BigInt in the hot path. Correctness is pinned by FIPS vectors and the cross-language
// golden vectors shared with the head's pow package (see sha256.test.ts / pow.test.ts).
//
// Besides the one-shot sha256(), this module exposes the block compression primitive
// (SHA256_INIT + compress) so pow.ts can precompute the challenge||publicKey midstate
// once and recompress only the final nonce block per attempt.

// Initial hash values: the first 32 bits of the fractional parts of the square roots of
// the first eight primes (FIPS 180-4 §5.3.3).
export const SHA256_INIT: readonly number[] = [
  0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c,
  0x1f83d9ab, 0x5be0cd19,
];

// Round constants: the first 32 bits of the fractional parts of the cube roots of the
// first sixty-four primes (FIPS 180-4 §4.2.2).
const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1,
  0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
  0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786,
  0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147,
  0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
  0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
  0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a,
  0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
  0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

// Reusable message schedule. The compression runs once per hashed block — millions of
// times during a solve — so the schedule buffer is allocated once, not per call.
const W = new Uint32Array(64);

// compress folds one 64-byte block (at block[offset .. offset+63]) into the eight-word
// state, updating it in place. Callers may seed state with SHA256_INIT for a fresh hash
// or with a saved midstate to continue a partially hashed message.
export function compress(state: Uint32Array, block: Uint8Array, offset: number): void {
  for (let i = 0; i < 16; i++) {
    const j = offset + i * 4;
    W[i] =
      (block[j] << 24) | (block[j + 1] << 16) | (block[j + 2] << 8) | block[j + 3];
  }
  for (let i = 16; i < 64; i++) {
    const w15 = W[i - 15];
    const s0 =
      ((w15 >>> 7) | (w15 << 25)) ^ ((w15 >>> 18) | (w15 << 14)) ^ (w15 >>> 3);
    const w2 = W[i - 2];
    const s1 =
      ((w2 >>> 17) | (w2 << 15)) ^ ((w2 >>> 19) | (w2 << 13)) ^ (w2 >>> 10);
    W[i] = (W[i - 16] + s0 + W[i - 7] + s1) | 0;
  }

  let a = state[0];
  let b = state[1];
  let c = state[2];
  let d = state[3];
  let e = state[4];
  let f = state[5];
  let g = state[6];
  let h = state[7];

  for (let i = 0; i < 64; i++) {
    const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
    const ch = (e & f) ^ (~e & g);
    const t1 = (h + S1 + ch + K[i] + W[i]) | 0;
    const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
    const maj = (a & b) ^ (a & c) ^ (b & c);
    const t2 = (S0 + maj) | 0;

    h = g;
    g = f;
    f = e;
    e = (d + t1) | 0;
    d = c;
    c = b;
    b = a;
    a = (t1 + t2) | 0;
  }

  state[0] = (state[0] + a) | 0;
  state[1] = (state[1] + b) | 0;
  state[2] = (state[2] + c) | 0;
  state[3] = (state[3] + d) | 0;
  state[4] = (state[4] + e) | 0;
  state[5] = (state[5] + f) | 0;
  state[6] = (state[6] + g) | 0;
  state[7] = (state[7] + h) | 0;
}

// sha256 returns the 32-byte digest of data.
export function sha256(data: Uint8Array): Uint8Array {
  const msgLen = data.length;

  // Pad to a whole number of 64-byte blocks: 0x80, then zeros, then the 64-bit
  // big-endian bit length occupying the final 8 bytes.
  const paddedLen = (Math.floor((msgLen + 8) / 64) + 1) * 64;
  const padded = new Uint8Array(paddedLen);
  padded.set(data);
  padded[msgLen] = 0x80;

  const view = new DataView(padded.buffer);
  const bitLen = msgLen * 8;
  view.setUint32(paddedLen - 8, Math.floor(bitLen / 0x100000000), false);
  view.setUint32(paddedLen - 4, bitLen >>> 0, false);

  const state = new Uint32Array(SHA256_INIT);
  for (let offset = 0; offset < paddedLen; offset += 64) {
    compress(state, padded, offset);
  }

  const out = new Uint8Array(32);
  const outView = new DataView(out.buffer);
  for (let i = 0; i < 8; i++) {
    outView.setUint32(i * 4, state[i], false);
  }
  return out;
}
