// Ed25519 identity manager for browser volunteers.
// Uses Web Crypto API for key operations and IndexedDB for persistence.

const DB_NAME = "lettuce-volunteer";
const STORE_NAME = "identity";
const KEY_ID = "primary";

export interface VolunteerIdentity {
  publicKey: Uint8Array;
  privateKey: CryptoKey;
  publicKeyBase64url: string;
  fingerprint: string;
}

function bytesToBinary(data: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < data.length; i++) {
    binary += String.fromCharCode(data[i]);
  }
  return binary;
}

export function bytesToBase64(data: Uint8Array): string {
  return btoa(bytesToBinary(data));
}

export function base64urlEncode(data: Uint8Array): string {
  return bytesToBase64(data).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function bytesToHex(data: Uint8Array): string {
  return Array.from(data)
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

export function base64urlDecode(str: string): Uint8Array {
  // Restore base64 padding and standard chars.
  let base64 = str.replace(/-/g, "+").replace(/_/g, "/");
  while (base64.length % 4 !== 0) {
    base64 += "=";
  }
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

async function computeFingerprint(publicKey: Uint8Array): Promise<string> {
  const hash = await crypto.subtle.digest("SHA-256", publicKey.buffer as ArrayBuffer);
  return bytesToHex(new Uint8Array(hash)).slice(0, 16);
}

function openDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, 1);
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(STORE_NAME)) {
        db.createObjectStore(STORE_NAME);
      }
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

function idbGet(
  db: IDBDatabase,
  key: string
): Promise<{ pkcs8: ArrayBuffer; raw: ArrayBuffer } | undefined> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, "readonly");
    const store = tx.objectStore(STORE_NAME);
    const request = store.get(key);
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

function idbPut(
  db: IDBDatabase,
  key: string,
  value: { pkcs8: ArrayBuffer; raw: ArrayBuffer }
): Promise<void> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, "readwrite");
    const store = tx.objectStore(STORE_NAME);
    const request = store.put(value, key);
    request.onsuccess = () => resolve();
    request.onerror = () => reject(request.error);
  });
}

function idbDelete(db: IDBDatabase, key: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE_NAME, "readwrite");
    const store = tx.objectStore(STORE_NAME);
    const request = store.delete(key);
    request.onsuccess = () => resolve();
    request.onerror = () => reject(request.error);
  });
}

async function buildIdentity(
  privateKey: CryptoKey,
  publicKeyRaw: Uint8Array
): Promise<VolunteerIdentity> {
  return {
    publicKey: publicKeyRaw,
    privateKey,
    publicKeyBase64url: base64urlEncode(publicKeyRaw),
    fingerprint: await computeFingerprint(publicKeyRaw),
  };
}

export async function createNewIdentity(): Promise<VolunteerIdentity> {
  const keyPair = await crypto.subtle.generateKey("Ed25519", true, [
    "sign",
    "verify",
  ]);

  const pkcs8 = await crypto.subtle.exportKey("pkcs8", keyPair.privateKey);
  const raw = await crypto.subtle.exportKey("raw", keyPair.publicKey);
  const publicKeyRaw = new Uint8Array(raw);

  const db = await openDB();
  await idbPut(db, KEY_ID, { pkcs8, raw });
  db.close();

  return buildIdentity(keyPair.privateKey, publicKeyRaw);
}

export async function getOrCreateIdentity(): Promise<VolunteerIdentity> {
  const db = await openDB();
  const stored = await idbGet(db, KEY_ID);

  if (stored) {
    const privateKey = await crypto.subtle.importKey(
      "pkcs8",
      stored.pkcs8,
      "Ed25519",
      false,
      ["sign"]
    );
    const publicKeyRaw = new Uint8Array(stored.raw);
    db.close();
    return buildIdentity(privateKey, publicKeyRaw);
  }

  db.close();
  return createNewIdentity();
}

export async function deleteIdentity(): Promise<void> {
  const db = await openDB();
  await idbDelete(db, KEY_ID);
  db.close();
}
