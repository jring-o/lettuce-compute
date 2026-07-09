// THE cross-language proof-of-work pin, mirroring services/infrastructure/pow/pow_test.go
// case-for-case. The digests, nonces, and leading-zero counts are hardcoded real outputs
// (independently reconfirmed with Node's crypto), so any divergence in the preimage
// layout, nonce encoding, or bit counting fails loudly here.

import { hexToBytes, leadingZeroBits, solvePow } from "../pow";
import { sha256 } from "../sha256";
import { bytesToHex } from "../identity";

// seqBytes returns n bytes whose values are start, start+1, … (mod 256) — the same
// one-line vector construction the Go golden test uses.
function seqBytes(start: number, n: number): Uint8Array {
  const b = new Uint8Array(n);
  for (let i = 0; i < n; i++) b[i] = (start + i) & 0xff;
  return b;
}

// manualDigest recomputes the solution digest independently of the solver, spelling out
// the documented preimage layout: SHA-256(challenge || publicKey || nonce as 8 big-endian
// bytes). This is the analogue of the Go test's manualDigest.
function manualDigest(
  challenge: Uint8Array,
  publicKey: Uint8Array,
  nonce: number
): Uint8Array {
  const nb = new Uint8Array(8);
  new DataView(nb.buffer).setUint32(4, nonce, false); // nonces here are < 2^32
  const preimage = new Uint8Array(challenge.length + publicKey.length + 8);
  preimage.set(challenge, 0);
  preimage.set(publicKey, challenge.length);
  preimage.set(nb, challenge.length + publicKey.length);
  return sha256(preimage);
}

// verifySolution mirrors the head's VerifySolution: a >= comparison of the digest's
// leading zero bits against the difficulty target.
function verifySolution(
  challenge: Uint8Array,
  publicKey: Uint8Array,
  nonce: number,
  difficultyBits: number
): boolean {
  return leadingZeroBits(manualDigest(challenge, publicKey, nonce)) >= difficultyBits;
}

describe("leadingZeroBits", () => {
  const highBitFirst = new Uint8Array(32);
  highBitFirst[0] = 0x80;
  const oneFirst = new Uint8Array(32);
  oneFirst[0] = 0x01;

  const cases: Array<[string, Uint8Array, number]> = [
    ["all-zero 32 bytes is fully saturated", new Uint8Array(32), 256],
    ["high bit set in first byte is zero", highBitFirst, 0],
    ["0x01 leaves seven leading zeros", oneFirst, 7],
    ["zero byte then 0xFF stops at eight", Uint8Array.from([0x00, 0xff]), 8],
    ["zero byte then 0x0F adds four", Uint8Array.from([0x00, 0x0f]), 12],
  ];

  it.each(cases)("%s", (_name, digest, want) => {
    expect(leadingZeroBits(digest)).toBe(want);
  });
});

describe("solvePow golden vector (cross-language pin with pow_test.go)", () => {
  const challenge = seqBytes(0, 32); // 0x00..0x1f
  const publicKey = seqBytes(32, 32); // 0x20..0x3f

  it("pins the nonce=1 preimage byte layout", () => {
    expect(bytesToHex(manualDigest(challenge, publicKey, 1))).toBe(
      "7f812700537ee9f8def5ab067d299b26f39ddf28875594ae0f95b6b1e40ce4c0"
    );
  });

  it("returns the first nonce (scanning from 0) that clears difficulty 16", () => {
    const sol = solvePow(challenge, publicKey, 16);
    expect(sol.nonce).toBe("19497");
    expect(sol.attempts).toBe(19498);
  });

  it("pins the difficulty-16 solution's digest and exact leading-zero count", () => {
    const d = manualDigest(challenge, publicKey, 19497);
    expect(bytesToHex(d)).toBe(
      "000040eb278da4930113209945a1b4d48614ba74bec414cc4b4418e1491e525b"
    );
    expect(leadingZeroBits(d)).toBe(17);
  });

  it("verifies as a >= comparison: accepted at 16 and 17, rejected at 18", () => {
    expect(verifySolution(challenge, publicKey, 19497, 16)).toBe(true);
    expect(verifySolution(challenge, publicKey, 19497, 17)).toBe(true);
    expect(verifySolution(challenge, publicKey, 19497, 18)).toBe(false);
  });
});

describe("solvePow public-key binding (diff 12)", () => {
  const challenge = seqBytes(0, 32); // 0x00..0x1f
  const keyA = seqBytes(32, 32); // 0x20..0x3f
  const keyB = seqBytes(64, 32); // 0x40..0x5f

  it("finds the pinned first nonce for key A", () => {
    expect(solvePow(challenge, keyA, 12).nonce).toBe("2286");
  });

  it("binds the public key into the preimage: A's nonce is not B's solution", () => {
    const dA = manualDigest(challenge, keyA, 2286);
    const dB = manualDigest(challenge, keyB, 2286);
    expect(bytesToHex(dA)).toBe(
      "000fb9757b2cac4a44c169521be41f7b7a7b56931dadb9d8bceab38e4a6b7e5c"
    );
    expect(bytesToHex(dB)).toBe(
      "9cc1515db812f8746ff8f4dc60d4f0240d47598b5473dc654373b2f2e51881cd"
    );
    expect(leadingZeroBits(dB)).toBe(0);
    expect(verifySolution(challenge, keyA, 2286, 12)).toBe(true);
    expect(verifySolution(challenge, keyB, 2286, 12)).toBe(false);
  });
});

describe("solvePow input validation", () => {
  it("throws when challenge is not 32 bytes", () => {
    expect(() => solvePow(new Uint8Array(31), new Uint8Array(32), 8)).toThrow();
  });

  it("throws when publicKey is not 32 bytes", () => {
    expect(() => solvePow(new Uint8Array(32), new Uint8Array(33), 8)).toThrow();
  });
});

describe("solvePow onProgress cadence", () => {
  const challenge = seqBytes(0, 32);
  const publicKey = seqBytes(32, 32);

  it("does not report progress when the solve finishes under one interval", () => {
    // The difficulty-16 golden solve takes 19,498 attempts, below the 65,536 cadence.
    const onProgress = jest.fn();
    solvePow(challenge, publicKey, 16, onProgress);
    expect(onProgress).not.toHaveBeenCalled();
  });

  it("reports progress at every 65,536th attempt for a longer solve", () => {
    // The first difficulty-18 solution is nonce 293300 (293,301 attempts), so progress
    // fires exactly at 65536, 131072, 196608, and 262144.
    const seen: number[] = [];
    const sol = solvePow(challenge, publicKey, 18, (attempts) => seen.push(attempts));
    expect(sol.nonce).toBe("293300");
    expect(seen).toEqual([65536, 131072, 196608, 262144]);
  });
});

describe("hexToBytes", () => {
  it("round-trips with bytesToHex", () => {
    const bytes = seqBytes(0, 32);
    expect(hexToBytes(bytesToHex(bytes))).toEqual(bytes);
  });

  it("decodes lowercase and uppercase hex", () => {
    expect(Array.from(hexToBytes("00ff10"))).toEqual([0x00, 0xff, 0x10]);
    expect(Array.from(hexToBytes("AABBCC"))).toEqual([0xaa, 0xbb, 0xcc]);
  });

  it("decodes the empty string to an empty array", () => {
    expect(hexToBytes("")).toEqual(new Uint8Array(0));
  });

  it("throws on odd length", () => {
    expect(() => hexToBytes("abc")).toThrow();
  });

  it("throws on non-hex characters", () => {
    expect(() => hexToBytes("zz")).toThrow();
    expect(() => hexToBytes("0x12")).toThrow();
  });
});
