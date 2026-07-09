// SHA-256 correctness pins: the FIPS 180-4 example vectors plus a midstate-equivalence
// check that the compress/SHA256_INIT primitive reproduces the one-shot sha256() — the
// exact two-block path pow.ts relies on.

import { compress, sha256, SHA256_INIT } from "../sha256";
import { bytesToHex } from "../identity";

function ascii(s: string): Uint8Array {
  return new TextEncoder().encode(s);
}

describe("sha256", () => {
  it("hashes the empty string", () => {
    expect(bytesToHex(sha256(new Uint8Array(0)))).toBe(
      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    );
  });

  it('hashes "abc"', () => {
    expect(bytesToHex(sha256(ascii("abc")))).toBe(
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
    );
  });

  it("hashes the 448-bit (two-block-padded) message", () => {
    expect(
      bytesToHex(
        sha256(
          ascii("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq")
        )
      )
    ).toBe("248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
  });

  it("hashes a >64-byte multi-block input (112-byte FIPS vector)", () => {
    expect(
      bytesToHex(
        sha256(
          ascii(
            "abcdefghbcdefghicdefghijdefghijkefghijklfghijklmghijklmnhijklmnoijklmnopjklmnopqklmnopqrlmnopqrsmnopqrstnopqrstu"
          )
        )
      )
    ).toBe("cf5b16a778af8380036ce59e7b0492370b249b11e8f07a51afac45037afee9d1");
  });

  it("hashes a long input spanning many blocks (one million 'a')", () => {
    expect(bytesToHex(sha256(new Uint8Array(1_000_000).fill(0x61)))).toBe(
      "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0"
    );
  });
});

describe("compress midstate equivalence", () => {
  it("reproduces sha256() for a 72-byte two-block message (pow.ts's path)", () => {
    // 72 bytes = one full block + an 8-byte remainder, matching challenge||publicKey||
    // nonce8. Compress the first block, then a manually padded final block, and compare
    // to the one-shot hash.
    const msg = new Uint8Array(72);
    for (let i = 0; i < msg.length; i++) msg[i] = (i * 7 + 3) & 0xff;

    const state = new Uint32Array(SHA256_INIT);
    compress(state, msg, 0);

    const finalBlock = new Uint8Array(64);
    finalBlock.set(msg.subarray(64, 72), 0);
    finalBlock[8] = 0x80;
    finalBlock[62] = 0x02; // 72 * 8 = 576 = 0x0240
    finalBlock[63] = 0x40;
    compress(state, finalBlock, 0);

    const digest = new Uint8Array(32);
    const view = new DataView(digest.buffer);
    for (let i = 0; i < 8; i++) view.setUint32(i * 4, state[i], false);

    expect(bytesToHex(digest)).toBe(bytesToHex(sha256(msg)));
  });

  it("reproduces sha256() for a single 64-byte block message", () => {
    // A 64-byte message pads into a second all-padding block; compressing both by hand
    // must equal the one-shot hash.
    const msg = new Uint8Array(64);
    for (let i = 0; i < msg.length; i++) msg[i] = (i * 11 + 1) & 0xff;

    const state = new Uint32Array(SHA256_INIT);
    compress(state, msg, 0);

    const finalBlock = new Uint8Array(64);
    finalBlock[0] = 0x80;
    finalBlock[62] = 0x02; // 64 * 8 = 512 = 0x0200
    finalBlock[63] = 0x00;
    compress(state, finalBlock, 0);

    const digest = new Uint8Array(32);
    const view = new DataView(digest.buffer);
    for (let i = 0; i < 8; i++) view.setUint32(i * 4, state[i], false);

    expect(bytesToHex(digest)).toBe(bytesToHex(sha256(msg)));
  });
});
