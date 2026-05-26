// Tests for the path-based viz bundle route:
//   /api/viz/bundle/<base64url-tarball-url>/<...file-path>
// Covers the C3 security fix: origin allowlist, SSRF resolved-IP check,
// redirect rejection, response hardening, and Host-based origin isolation.

// --- Mocks ---

// Mock fetch globally
const mockFetch = jest.fn();
global.fetch = mockFetch as unknown as typeof fetch;

// Mock zlib.gunzipSync
const mockGunzipSync = jest.fn();
jest.mock("zlib", () => ({
  gunzipSync: (...args: unknown[]) => mockGunzipSync(...args),
}));

// Mock node:dns so DNS resolution is deterministic in tests.
const mockLookup = jest.fn();
jest.mock("node:dns", () => ({
  __esModule: true,
  default: {
    promises: {
      lookup: (...args: unknown[]) => mockLookup(...args),
    },
  },
}));

// Mock next/server — NextRequest, NextResponse
jest.mock("next/server", () => {
  class MockNextResponse {
    body: Uint8Array | null;
    status: number;
    headers: Map<string, string>;

    constructor(
      body: Uint8Array | null,
      init?: { status?: number; headers?: Record<string, string> },
    ) {
      this.body = body;
      this.status = init?.status ?? 200;
      this.headers = new Map(Object.entries(init?.headers ?? {}));
    }

    static json(data: unknown, init?: { status?: number }) {
      const instance = new MockNextResponse(null, { status: init?.status });
      (instance as unknown as Record<string, unknown>)._jsonData = data;
      return instance;
    }
  }

  class MockNextRequest {
    url: string;
    headers: Map<string, string>;
    nextUrl: { searchParams: URLSearchParams };

    constructor(url: string, init?: { headers?: Record<string, string> }) {
      this.url = url;
      const lower: Array<[string, string]> = Object.entries(
        init?.headers ?? {},
      ).map(([k, v]) => [k.toLowerCase(), v]);
      this.headers = new Map(lower);
      this.nextUrl = { searchParams: new URL(url).searchParams };
    }
  }

  return {
    NextRequest: MockNextRequest,
    NextResponse: MockNextResponse,
  };
});

import { GET } from "@/app/api/viz/bundle/[url]/[...path]/route";
import { NextRequest } from "next/server";

// Helper to build a minimal tar buffer.
function buildTarBuffer(
  files: Array<{ name: string; content: string }>,
): Buffer {
  const blocks: Buffer[] = [];

  for (const { name, content } of files) {
    const header = Buffer.alloc(512, 0);
    header.write(name, 0, Math.min(name.length, 100), "utf8");
    const sizeOctal = content.length.toString(8).padStart(11, "0");
    header.write(sizeOctal, 124, 12, "utf8");
    header[156] = 0x30; // regular file
    blocks.push(header);

    const dataSize = Math.ceil(content.length / 512) * 512;
    const dataBlock = Buffer.alloc(dataSize, 0);
    dataBlock.write(content, 0, content.length, "utf8");
    blocks.push(dataBlock);
  }

  blocks.push(Buffer.alloc(1024, 0)); // end-of-archive
  return Buffer.concat(blocks);
}

// base64url-encode a tarball URL the way VizIframe does.
function enc(url: string): string {
  return Buffer.from(url, "utf8").toString("base64url");
}

// Build the route's second arg.
function ctx(encodedUrl: string, segments: string[]) {
  return { params: Promise.resolve({ url: encodedUrl, path: segments }) };
}

// Default request (no Host header relevance unless VIZ_ORIGIN is set).
function req(headers?: Record<string, string>) {
  return new NextRequest("http://localhost/api/viz/bundle/x/index.html", {
    headers,
  });
}

const PLATFORM = "https://platform.example.com";

// The route caches extracted bundles by tarball URL in a module-level Map that
// persists across tests. Generate a unique URL per call so each test triggers a
// fresh fetch instead of hitting a sibling test's cached entry.
let urlCounter = 0;
function platformTarball(): string {
  urlCounter += 1;
  return `${PLATFORM}/binaries/v-${urlCounter}.tar.gz`;
}

function mockTarballResponse(name: string, content: string) {
  const tarBuf = buildTarBuffer([{ name, content }]);
  mockGunzipSync.mockReturnValue(tarBuf);
  mockFetch.mockResolvedValue({
    ok: true,
    headers: new Map(),
    arrayBuffer: () => Promise.resolve(tarBuf.buffer),
  });
}

const ORIGINAL_ENV = { ...process.env };

beforeEach(() => {
  jest.clearAllMocks();
  process.env = { ...ORIGINAL_ENV };
  process.env.PLATFORM_URL = PLATFORM;
  delete process.env.VIZ_ORIGIN;
  delete process.env.VIZ_BUNDLE_ALLOWED_ORIGINS;
  // Default DNS lookup -> public address (overridden per-test where needed).
  mockLookup.mockResolvedValue([{ address: "93.184.216.34", family: 4 }]);
});

afterAll(() => {
  process.env = ORIGINAL_ENV;
});

describe("GET /api/viz/bundle/[url]/[...path] — legit flow", () => {
  it("serves a file from an allowlisted (PLATFORM_URL) origin", async () => {
    mockTarballResponse("./viz/index.html", "<html>ok</html>");
    const url = platformTarball();

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(mockFetch).toHaveBeenCalled();
    expect(response.status).toBe(200);
    expect(response.headers.get("Content-Type")).toBe("text/html");
  });

  it("serves a file from an explicit VIZ_BUNDLE_ALLOWED_ORIGINS entry", async () => {
    process.env.VIZ_BUNDLE_ALLOWED_ORIGINS =
      "https://cdn.example.com, https://other.example.com";
    mockTarballResponse("./viz/main.js", "console.log(1)");
    const url = "https://cdn.example.com/bundles/v1/viz.tar.gz";

    const response = await GET(req(), ctx(enc(url), ["main.js"]));

    expect(response.status).toBe(200);
    expect(response.headers.get("Content-Type")).toBe("application/javascript");
  });

  it("passes redirect: 'error' to fetch", async () => {
    mockTarballResponse("./viz/index.html", "<html></html>");
    const url = platformTarball();

    await GET(req(), ctx(enc(url), ["index.html"]));

    expect(mockFetch).toHaveBeenCalledWith(
      url,
      expect.objectContaining({ redirect: "error" }),
    );
  });
});

describe("GET /api/viz/bundle — origin allowlist (core fix)", () => {
  it("rejects a non-allowlisted origin with 400 ORIGIN_NOT_ALLOWED", async () => {
    const url = "https://evil.example.com/payload.tar.gz";

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("ORIGIN_NOT_ALLOWED");
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("rejects a non-http(s) scheme", async () => {
    const url = "file:///etc/passwd";

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("INVALID_URL");
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("rejects an unparseable URL", async () => {
    const response = await GET(req(), ctx(enc("not a url"), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("INVALID_URL");
    expect(mockFetch).not.toHaveBeenCalled();
  });
});

describe("GET /api/viz/bundle — SSRF resolved-IP defense", () => {
  it("blocks when an allowlisted host resolves to a loopback IP", async () => {
    process.env.VIZ_BUNDLE_ALLOWED_ORIGINS = "https://rebind.example.com";
    mockLookup.mockResolvedValue([{ address: "127.0.0.1", family: 4 }]);
    const url = "https://rebind.example.com/v.tar.gz";

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("SSRF_BLOCKED");
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("blocks when an allowlisted host resolves to a private 10/8 IP", async () => {
    process.env.VIZ_BUNDLE_ALLOWED_ORIGINS = "https://rebind.example.com";
    mockLookup.mockResolvedValue([{ address: "10.5.6.7", family: 4 }]);
    const url = "https://rebind.example.com/v.tar.gz";

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("SSRF_BLOCKED");
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("blocks when ANY resolved address is private (mixed result)", async () => {
    process.env.VIZ_BUNDLE_ALLOWED_ORIGINS = "https://rebind.example.com";
    mockLookup.mockResolvedValue([
      { address: "93.184.216.34", family: 4 },
      { address: "169.254.169.254", family: 4 },
    ]);
    const url = "https://rebind.example.com/v.tar.gz";

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("SSRF_BLOCKED");
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("blocks an IPv6 unique-local resolved address", async () => {
    process.env.VIZ_BUNDLE_ALLOWED_ORIGINS = "https://rebind.example.com";
    mockLookup.mockResolvedValue([{ address: "fd00::1", family: 6 }]);
    const url = "https://rebind.example.com/v.tar.gz";

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("SSRF_BLOCKED");
  });

  it("blocks a literal private-IP allowlisted origin without a DNS lookup", async () => {
    process.env.VIZ_BUNDLE_ALLOWED_ORIGINS = "http://127.0.0.1:9000";
    const url = "http://127.0.0.1:9000/v.tar.gz";

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("SSRF_BLOCKED");
    expect(mockLookup).not.toHaveBeenCalled();
    expect(mockFetch).not.toHaveBeenCalled();
  });
});

describe("GET /api/viz/bundle — redirect rejection", () => {
  it("returns 502 when fetch rejects due to a redirect", async () => {
    // Node's undici throws on redirect when redirect: 'error'.
    mockFetch.mockRejectedValue(
      new TypeError("unexpected redirect, redirect mode is set to error"),
    );
    const url = platformTarball();

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(502);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("BUNDLE_FETCH_ERROR");
  });
});

describe("GET /api/viz/bundle — response hardening", () => {
  it("sets X-Content-Type-Options: nosniff and frame-ancestors CSP", async () => {
    mockTarballResponse("./viz/index.html", "<html></html>");
    const url = platformTarball();

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(200);
    expect(response.headers.get("X-Content-Type-Options")).toBe("nosniff");
    expect(response.headers.get("Content-Security-Policy")).toBe(
      `frame-ancestors ${PLATFORM}`,
    );
    expect(response.headers.get("Cache-Control")).toBe("public, max-age=3600");
  });

  it("returns application/wasm for .wasm files", async () => {
    mockTarballResponse("./viz/compute.wasm", "wasm-stub");
    const url = platformTarball();

    const response = await GET(req(), ctx(enc(url), ["compute.wasm"]));

    expect(response.status).toBe(200);
    expect(response.headers.get("Content-Type")).toBe("application/wasm");
  });
});

describe("GET /api/viz/bundle — path traversal guards (regression)", () => {
  it("rejects '..' traversal in the file path", async () => {
    const url = platformTarball();

    const response = await GET(
      req(),
      ctx(enc(url), ["..", "..", "etc", "passwd"]),
    );

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("INVALID_PATH");
    expect(mockFetch).not.toHaveBeenCalled();
  });
});

describe("GET /api/viz/bundle — file lookup & upstream errors", () => {
  it("returns 404 when the file is not in the bundle", async () => {
    mockTarballResponse("./viz/index.html", "<html></html>");
    const url = platformTarball();

    const response = await GET(req(), ctx(enc(url), ["nope.js"]));

    expect(response.status).toBe(404);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("NOT_FOUND");
  });

  it("returns 502 when upstream fetch fails", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 503,
      statusText: "Service Unavailable",
    });
    const url = platformTarball();

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(502);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("BUNDLE_FETCH_ERROR");
  });
});

describe("GET /api/viz/bundle — F2 extraction caps (gzip-bomb defense)", () => {
  // The route mocks `zlib` at the module level. To simulate the
  // gunzip-output-too-large case we make mockGunzipSync THROW with the same
  // error shape Node uses when maxOutputLength is exceeded.
  it("rejects with 400 BUNDLE_TOO_LARGE when gunzip exceeds maxOutputLength", async () => {
    // Tiny "compressed" buffer (content unused — gunzip is mocked).
    const tinyBuf = Buffer.from([0x1f, 0x8b, 0x08, 0x00]);
    mockFetch.mockResolvedValue({
      ok: true,
      headers: new Map(),
      arrayBuffer: () => Promise.resolve(tinyBuf.buffer),
    });
    const err = new Error(
      "Cannot create a Buffer larger than maxOutputLength",
    ) as Error & { code?: string };
    err.code = "ERR_BUFFER_TOO_LARGE";
    mockGunzipSync.mockImplementation(() => {
      throw err;
    });

    const url = platformTarball();
    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("BUNDLE_TOO_LARGE");
  });

  it("rejects with 400 BUNDLE_TOO_LARGE when a single entry exceeds the per-entry cap", async () => {
    // Build a tar with one entry whose declared size (in the header) exceeds
    // the per-entry 100 MB cap. We don't need to actually allocate the
    // payload — the route checks `size > MAX_ENTRY_SIZE` from the parsed
    // header octal BEFORE materializing the entry, so a fabricated header is
    // enough to trigger the cap.
    const header = Buffer.alloc(512, 0);
    header.write("huge.bin", 0, 8, "utf8");
    // 200 MB in octal padded to 11 digits.
    const tooBig = (200 * 1024 * 1024).toString(8).padStart(11, "0");
    header.write(tooBig, 124, 12, "utf8");
    header[156] = 0x30; // regular file
    const tarBuf = Buffer.concat([header, Buffer.alloc(1024, 0)]);
    mockGunzipSync.mockReturnValue(tarBuf);
    mockFetch.mockResolvedValue({
      ok: true,
      headers: new Map(),
      arrayBuffer: () => Promise.resolve(tarBuf.buffer),
    });

    const url = platformTarball();
    const response = await GET(req(), ctx(enc(url), ["huge.bin"]));

    expect(response.status).toBe(400);
    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(data.error.code).toBe("BUNDLE_TOO_LARGE");
  });

  it("rejects with 400 BUNDLE_TOO_LARGE when many entries together exceed the total cap", async () => {
    // Construct DECOMPRESSED tar contents whose entry header sizes SUM to
    // more than the 500 MB total cap, each at exactly the 100 MB per-entry
    // cap so the per-entry guard (which trips on strict `>`) doesn't fire.
    // 6 * 100 MB = 600 MB > 500 MB cap.
    //
    // The "compressed" buffer fed to fetch is tiny (so we don't hit the
    // 500 MB DOWNLOAD cap); gunzipSync is mocked to RETURN the large tar
    // payload, which is the bytes the extractor sees. The extractor walks
    // `offset += ceil(size/512)*512` past each entry's payload, so the
    // payloads must be present (as zeros) or the loop will exit early when
    // offset overshoots buf.length.
    const per = 100 * 1024 * 1024; // exactly at the per-entry cap
    const count = 6;
    const headerStride = 512 + Math.ceil(per / 512) * 512; // header + payload
    const tarBuf = Buffer.alloc(count * headerStride + 1024, 0);
    const sizeOctal = per.toString(8).padStart(11, "0");
    for (let i = 0; i < count; i += 1) {
      const base = i * headerStride;
      tarBuf.write(`f${i}.bin`, base, 6, "utf8");
      tarBuf.write(sizeOctal, base + 124, 12, "utf8");
      tarBuf[base + 156] = 0x30; // regular file
    }
    // gunzip returns the 600 MB tar; the compressed bytes we hand to fetch
    // are a tiny stand-in so the existing download cap doesn't fire.
    const tinyCompressed = Buffer.from([0x1f, 0x8b, 0x08, 0x00]);
    mockGunzipSync.mockReturnValue(tarBuf);
    mockFetch.mockResolvedValue({
      ok: true,
      headers: new Map(),
      arrayBuffer: () => Promise.resolve(tinyCompressed.buffer),
    });

    const url = platformTarball();
    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    const data = (response as unknown as Record<string, unknown>)._jsonData as {
      error: { code: string };
    };
    expect(response.status).toBe(400);
    expect(data.error.code).toBe("BUNDLE_TOO_LARGE");
  });

  it("positive control: a small tar.gz still extracts and serves normally", async () => {
    // Regression guard — make sure the caps don't reject legitimate small
    // bundles. Re-uses the existing happy-path tar helper.
    mockTarballResponse("./viz/index.html", "<html>ok</html>");
    const url = platformTarball();

    const response = await GET(req(), ctx(enc(url), ["index.html"]));

    expect(response.status).toBe(200);
    expect(response.headers.get("Content-Type")).toBe("text/html");
  });

  it("does NOT cache a bundle that failed the cap check", async () => {
    // After a too-large rejection, a follow-up request for the same URL
    // should refetch (the failed extract must not have been cached). Use the
    // gunzip-throw path to simulate the breach.
    const tinyBuf = Buffer.from([0x1f, 0x8b, 0x08, 0x00]);
    mockFetch.mockResolvedValue({
      ok: true,
      headers: new Map(),
      arrayBuffer: () => Promise.resolve(tinyBuf.buffer),
    });
    const err = new Error("output too large") as Error & { code?: string };
    err.code = "ERR_BUFFER_TOO_LARGE";
    mockGunzipSync.mockImplementation(() => {
      throw err;
    });

    const url = platformTarball();
    const r1 = await GET(req(), ctx(enc(url), ["index.html"]));
    expect(r1.status).toBe(400);
    const r2 = await GET(req(), ctx(enc(url), ["index.html"]));
    expect(r2.status).toBe(400);

    // Both calls hit fetch — confirms nothing was cached from the failed
    // first attempt.
    expect(mockFetch).toHaveBeenCalledTimes(2);
  });
});

describe("GET /api/viz/bundle — origin isolation (Host binding)", () => {
  it("returns 404 when VIZ_ORIGIN is set and Host does not match", async () => {
    process.env.VIZ_ORIGIN = "https://viz.example.com";
    mockTarballResponse("./viz/index.html", "<html></html>");
    const url = platformTarball();

    // Request comes in on the MAIN app host, not the viz host.
    const response = await GET(
      req({ host: "platform.example.com" }),
      ctx(enc(url), ["index.html"]),
    );

    expect(response.status).toBe(404);
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("serves when VIZ_ORIGIN is set and Host matches the viz host", async () => {
    process.env.VIZ_ORIGIN = "https://viz.example.com";
    mockTarballResponse("./viz/index.html", "<html></html>");
    const url = platformTarball();

    const response = await GET(
      req({ host: "viz.example.com" }),
      ctx(enc(url), ["index.html"]),
    );

    expect(response.status).toBe(200);
  });

  it("skips the Host check when VIZ_ORIGIN is unset (local dev)", async () => {
    delete process.env.VIZ_ORIGIN;
    mockTarballResponse("./viz/index.html", "<html></html>");
    const url = platformTarball();

    const response = await GET(
      req({ host: "anything.local" }),
      ctx(enc(url), ["index.html"]),
    );

    expect(response.status).toBe(200);
  });
});
