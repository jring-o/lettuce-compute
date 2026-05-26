import { base64urlEncode, base64urlDecode } from "../identity";

// Mock IndexedDB with a simple in-memory store.
const idbStore = new Map<string, unknown>();

const mockObjectStore = {
  get: jest.fn((key: string) => {
    const req = {
      result: idbStore.get(key),
      onsuccess: null as (() => void) | null,
      onerror: null as (() => void) | null,
    };
    setTimeout(() => req.onsuccess?.());
    return req;
  }),
  put: jest.fn((value: unknown, key: string) => {
    idbStore.set(key, value);
    const req = {
      onsuccess: null as (() => void) | null,
      onerror: null as (() => void) | null,
    };
    setTimeout(() => req.onsuccess?.());
    return req;
  }),
  delete: jest.fn((key: string) => {
    idbStore.delete(key);
    const req = {
      onsuccess: null as (() => void) | null,
      onerror: null as (() => void) | null,
    };
    setTimeout(() => req.onsuccess?.());
    return req;
  }),
};

const mockTransaction = {
  objectStore: jest.fn(() => mockObjectStore),
};

const mockDB = {
  transaction: jest.fn(() => mockTransaction),
  objectStoreNames: { contains: jest.fn(() => true) },
  createObjectStore: jest.fn(),
  close: jest.fn(),
};

Object.defineProperty(global, "indexedDB", {
  value: {
    open: jest.fn(() => {
      const req = {
        result: mockDB,
        onupgradeneeded: null as (() => void) | null,
        onsuccess: null as (() => void) | null,
        onerror: null as (() => void) | null,
        error: null,
      };
      setTimeout(() => req.onsuccess?.());
      return req;
    }),
  },
  writable: true,
});

// Mock Web Crypto API for Ed25519.
const mockKeyPair = {
  privateKey: { type: "private", algorithm: { name: "Ed25519" } },
  publicKey: { type: "public", algorithm: { name: "Ed25519" } },
};

const mockPublicKeyRaw = new Uint8Array(32);
for (let i = 0; i < 32; i++) mockPublicKeyRaw[i] = i;

const mockPkcs8 = new Uint8Array(48);
for (let i = 0; i < 48; i++) mockPkcs8[i] = i + 100;

const mockSignature = new Uint8Array(64);
for (let i = 0; i < 64; i++) mockSignature[i] = i + 50;

const mockSha256 = new Uint8Array(32);
for (let i = 0; i < 32; i++) mockSha256[i] = i * 7;

Object.defineProperty(global, "crypto", {
  value: {
    subtle: {
      generateKey: jest.fn().mockResolvedValue(mockKeyPair),
      exportKey: jest.fn((format: string) => {
        if (format === "pkcs8") return Promise.resolve(mockPkcs8.buffer);
        if (format === "raw") return Promise.resolve(mockPublicKeyRaw.buffer);
        return Promise.reject(new Error(`Unknown format: ${format}`));
      }),
      importKey: jest.fn().mockResolvedValue(mockKeyPair.privateKey),
      sign: jest.fn().mockResolvedValue(mockSignature.buffer),
      digest: jest.fn().mockResolvedValue(mockSha256.buffer),
    },
    getRandomValues: jest.fn((arr: Uint8Array) => {
      for (let i = 0; i < arr.length; i++) arr[i] = Math.floor(Math.random() * 256);
      return arr;
    }),
  },
  writable: true,
});

// Import after mocks are set up.
import {
  getOrCreateIdentity,
  createNewIdentity,
  deleteIdentity,
} from "../identity";

describe("Ed25519 Identity Manager", () => {
  beforeEach(() => {
    idbStore.clear();
    jest.clearAllMocks();
  });

  describe("base64url encoding/decoding", () => {
    it("roundtrips correctly for various byte arrays", () => {
      const testCases = [
        new Uint8Array([0, 1, 2, 3]),
        new Uint8Array(32).fill(0xff),
        new Uint8Array([62, 63, 255, 0, 128]),
        new Uint8Array(0),
      ];

      for (const input of testCases) {
        const encoded = base64urlEncode(input);
        const decoded = base64urlDecode(encoded);
        expect(decoded).toEqual(input);
      }
    });

    it("produces URL-safe output without padding", () => {
      const data = new Uint8Array([62, 63, 255]);
      const encoded = base64urlEncode(data);
      expect(encoded).not.toContain("+");
      expect(encoded).not.toContain("/");
      expect(encoded).not.toContain("=");
    });
  });

  describe("getOrCreateIdentity", () => {
    it("generates a new keypair on first call", async () => {
      const identity = await getOrCreateIdentity();

      expect(identity.publicKey).toBeInstanceOf(Uint8Array);
      expect(identity.publicKey.length).toBe(32);
      expect(identity.publicKeyBase64url).toBeTruthy();
      expect(identity.fingerprint).toMatch(/^[0-9a-f]{16}$/);
      expect(crypto.subtle.generateKey).toHaveBeenCalledWith(
        "Ed25519",
        true,
        ["sign", "verify"]
      );
    });

    it("returns the same identity on subsequent calls", async () => {
      const first = await getOrCreateIdentity();
      const second = await getOrCreateIdentity();

      expect(first.publicKeyBase64url).toBe(second.publicKeyBase64url);
      expect(first.fingerprint).toBe(second.fingerprint);
      // generateKey called once for first, importKey for second.
      expect(crypto.subtle.generateKey).toHaveBeenCalledTimes(1);
      expect(crypto.subtle.importKey).toHaveBeenCalledTimes(1);
    });
  });

  describe("createNewIdentity", () => {
    it("generates a new keypair even if one exists", async () => {
      await getOrCreateIdentity();
      jest.clearAllMocks();

      await createNewIdentity();

      expect(crypto.subtle.generateKey).toHaveBeenCalledTimes(1);
      expect(crypto.subtle.exportKey).toHaveBeenCalledTimes(2);
    });
  });

  describe("deleteIdentity", () => {
    it("removes stored identity", async () => {
      await getOrCreateIdentity();
      expect(idbStore.size).toBe(1);

      await deleteIdentity();
      expect(idbStore.size).toBe(0);
    });
  });
});
