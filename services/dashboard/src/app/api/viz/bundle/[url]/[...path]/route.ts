import { NextRequest, NextResponse } from "next/server";
import { gunzipSync } from "zlib";
import dns from "node:dns";
import net from "node:net";

// In-memory cache: tarball URL -> Map<filePath, {data, contentType}>
const bundleCache = new Map<string, Map<string, { data: Buffer; contentType: string }>>();

// F2: extraction-time caps. The pre-existing maxDownloadSize bounds COMPRESSED
// bytes from the network; these bound DECOMPRESSED bytes so a small gzip bomb
// cannot expand into multi-GB of memory.
//   - MAX_DECOMPRESSED_SIZE: total decompressed bytes across the whole tarball.
//     Matches the existing 500 MB download cap so legitimate bundles (the
//     documented examples are well under this) still fit.
//   - MAX_ENTRY_SIZE: per-file cap so a single oversized entry is rejected
//     even if it would fit under the total cap. 100 MB matches the
//     volunteer-cli viz extractor.
const MAX_DECOMPRESSED_SIZE = 500 * 1024 * 1024;
const MAX_ENTRY_SIZE = 100 * 1024 * 1024;

/**
 * F2: thrown when the tarball exceeds an extraction-time size cap (total
 * decompressed bytes or per-entry bytes). Surfaced as a 400 to the client so
 * caller can distinguish a hostile/oversized bundle from an upstream fetch
 * error (which is a 502).
 */
class BundleTooLargeError extends Error {}

const MIME_TYPES: Record<string, string> = {
  ".html": "text/html",
  ".js": "application/javascript",
  ".mjs": "application/javascript",
  ".css": "text/css",
  ".json": "application/json",
  ".wasm": "application/wasm",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".gif": "image/gif",
};

function getContentType(filePath: string): string {
  const ext = filePath.substring(filePath.lastIndexOf(".")).toLowerCase();
  return MIME_TYPES[ext] ?? "application/octet-stream";
}

/**
 * Normalize a URL to a canonical `scheme://host[:port]` origin string.
 * Returns null if the URL is unparseable or uses a non-http(s) scheme.
 */
function normalizeOrigin(urlStr: string): string | null {
  let parsed: URL;
  try {
    parsed = new URL(urlStr);
  } catch {
    return null;
  }
  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
    return null;
  }
  // URL.origin already drops default ports (443 for https, 80 for http) and
  // lowercases the host, giving us a stable comparison key.
  return parsed.origin;
}

/**
 * Build the set of allowlisted tarball origins.
 *
 * Sourced from VIZ_BUNDLE_ALLOWED_ORIGINS (comma-separated `scheme://host[:port]`
 * entries). If unset/empty, defaults to the origin of PLATFORM_URL, where the
 * platform serves `/binaries/...viz.tar.gz`. Entries that don't parse to a valid
 * http(s) origin are ignored.
 */
function getAllowedOrigins(): Set<string> {
  const allowed = new Set<string>();

  const raw = process.env.VIZ_BUNDLE_ALLOWED_ORIGINS;
  if (raw && raw.trim()) {
    for (const entry of raw.split(",")) {
      const origin = normalizeOrigin(entry.trim());
      if (origin) allowed.add(origin);
    }
  }

  // Default to the PLATFORM_URL origin (where /binaries/ is served) when the
  // explicit allowlist is unset or yielded no valid entries.
  if (allowed.size === 0 && process.env.PLATFORM_URL) {
    const origin = normalizeOrigin(process.env.PLATFORM_URL);
    if (origin) allowed.add(origin);
  }

  return allowed;
}

/**
 * Determine whether an IP address (v4 or v6) is loopback, private, link-local,
 * or otherwise non-routable — i.e. a SSRF target we must never fetch from.
 */
function isPrivateAddress(addr: string): boolean {
  const family = net.isIP(addr);

  if (family === 4) {
    const octets = addr.split(".").map(Number);
    if (octets.length !== 4 || octets.some((o) => Number.isNaN(o))) return true;
    const [a, b] = octets;
    if (a === 0) return true; // 0.0.0.0/8 ("this network")
    if (a === 10) return true; // 10.0.0.0/8
    if (a === 127) return true; // 127.0.0.0/8 loopback
    if (a === 169 && b === 254) return true; // 169.254.0.0/16 link-local
    if (a === 172 && b >= 16 && b <= 31) return true; // 172.16.0.0/12
    if (a === 192 && b === 168) return true; // 192.168.0.0/16
    return false;
  }

  if (family === 6) {
    let ip = addr.toLowerCase();
    // Strip zone id (e.g. fe80::1%eth0)
    const pct = ip.indexOf("%");
    if (pct !== -1) ip = ip.substring(0, pct);

    // IPv4-mapped / IPv4-compatible IPv6 — re-check the embedded v4 address.
    const mapped = ip.match(/^::(?:ffff:)?(\d+\.\d+\.\d+\.\d+)$/);
    if (mapped) return isPrivateAddress(mapped[1]);

    if (ip === "::" || ip === "::1") return true; // unspecified / loopback
    // fc00::/7 unique-local (fc.. or fd..)
    if (/^f[cd][0-9a-f]{2}:/.test(ip)) return true;
    // fe80::/10 link-local (fe8.. fe9.. fea.. feb..)
    if (/^fe[89ab][0-9a-f]:/.test(ip)) return true;
    return false;
  }

  // Not a literal IP — treat as unsafe (callers resolve to IPs first).
  return true;
}

/**
 * Resolve a hostname via DNS and reject if ANY resolved address is private/
 * loopback/link-local. Defeats DNS-rebinding and alternative IP encodings,
 * because we validate the actual resolved IPs rather than the literal host
 * string. Literal IP hosts are checked directly without a DNS lookup.
 */
async function assertHostResolvesPublic(hostname: string): Promise<void> {
  // Literal IP? validate directly (covers decimal/hex/octal forms once the URL
  // parser normalizes them, plus bracketed IPv6).
  const stripped = hostname.replace(/^\[|\]$/g, "");
  if (net.isIP(stripped)) {
    if (isPrivateAddress(stripped)) {
      throw new SsrfError("Resolved address is private/loopback");
    }
    return;
  }

  let records: { address: string }[];
  try {
    records = await dns.promises.lookup(hostname, { all: true });
  } catch {
    throw new SsrfError("Host could not be resolved");
  }

  if (records.length === 0) {
    throw new SsrfError("Host did not resolve to any address");
  }

  for (const { address } of records) {
    if (isPrivateAddress(address)) {
      throw new SsrfError("Host resolves to a private/loopback address");
    }
  }
}

class SsrfError extends Error {}

/** Validate the file path is safe (relative, no traversal). */
function isValidFilePath(file: string): boolean {
  if (!file || file.startsWith("/") || file.startsWith("\\")) return false;
  if (file.includes("..")) return false;
  if (file.includes("\0")) return false;
  return true;
}

async function fetchAndCacheTarball(url: string): Promise<Map<string, { data: Buffer; contentType: string }>> {
  const existing = bundleCache.get(url);
  if (existing) return existing;

  // Re-validate the resolved IPs immediately before fetching. The allowlist
  // (checked by the caller) restricts WHICH origin we contact; this defeats a
  // DNS-rebinding attack where an allowlisted host resolves to an internal IP.
  const parsed = new URL(url);
  await assertHostResolvesPublic(parsed.hostname);

  const maxDownloadSize = 500 * 1024 * 1024;
  // redirect: "error" — an allowlisted host must not be able to 302 us to an
  // internal/unvalidated target. Any redirect rejects the fetch.
  const response = await fetch(url, { redirect: "error" });
  if (!response.ok) {
    throw new Error(`Failed to download viz bundle: ${response.status} ${response.statusText}`);
  }

  const contentLength = response.headers.get("content-length");
  if (contentLength && parseInt(contentLength, 10) > maxDownloadSize) {
    throw new Error(`Viz bundle too large: ${contentLength} bytes exceeds ${maxDownloadSize} byte limit`);
  }

  const arrayBuffer = await response.arrayBuffer();
  if (arrayBuffer.byteLength > maxDownloadSize) {
    throw new Error(`Viz bundle too large: ${arrayBuffer.byteLength} bytes exceeds ${maxDownloadSize} byte limit`);
  }
  const files = extractTarGz(Buffer.from(arrayBuffer));

  bundleCache.set(url, files);
  return files;
}

function extractTarGz(buf: Buffer): Map<string, { data: Buffer; contentType: string }> {
  // F2: cap the gunzip output itself so a tiny gzip that decompresses to many
  // GB is rejected by zlib before we ever materialize it. Node throws an
  // ERR_BUFFER_TOO_LARGE-style error if the output would exceed
  // maxOutputLength; surface it as our typed BundleTooLargeError so the route
  // returns a 400 instead of a generic 502.
  let tar: Buffer;
  try {
    tar = gunzipSync(buf, { maxOutputLength: MAX_DECOMPRESSED_SIZE });
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    // Node's error code is typically ERR_BUFFER_TOO_LARGE; match defensively on
    // either the code or the message text since the exact wording has shifted
    // across Node versions.
    const code = (err as { code?: string } | undefined)?.code;
    if (code === "ERR_BUFFER_TOO_LARGE" || /too large|maxOutputLength/i.test(message)) {
      throw new BundleTooLargeError(
        `Viz bundle decompressed output exceeds ${MAX_DECOMPRESSED_SIZE} byte limit`,
      );
    }
    throw err;
  }
  return extractTar(tar);
}

function extractTar(buf: Buffer): Map<string, { data: Buffer; contentType: string }> {
  const files = new Map<string, { data: Buffer; contentType: string }>();
  let offset = 0;
  // F2: defense-in-depth — also track the sum of entry payload bytes during
  // extraction. gunzip's maxOutputLength already caught the bulk case, but a
  // crafted tar could have legitimate gunzip size yet many small-but-bogus
  // entries; this catches that and gives a typed error.
  let totalEntryBytes = 0;

  while (offset + 512 <= buf.length) {
    const header = buf.subarray(offset, offset + 512);

    if (header.every((b) => b === 0)) break;

    let name = header.subarray(0, 100).toString("utf8").replace(/\0+$/, "");
    const prefix = header.subarray(345, 500).toString("utf8").replace(/\0+$/, "");
    if (prefix) {
      name = prefix + "/" + name;
    }

    const sizeStr = header.subarray(124, 136).toString("utf8").replace(/\0+$/, "").trim();
    const size = parseInt(sizeStr, 8) || 0;

    const typeFlag = header[156];

    offset += 512;

    // F2: per-entry cap — reject single huge entries early, before we copy
    // them into the cache map.
    if (size > MAX_ENTRY_SIZE) {
      throw new BundleTooLargeError(
        `Viz bundle entry '${name}' size ${size} exceeds per-entry ${MAX_ENTRY_SIZE} byte limit`,
      );
    }
    // F2: running total across all entries.
    totalEntryBytes += size;
    if (totalEntryBytes > MAX_DECOMPRESSED_SIZE) {
      throw new BundleTooLargeError(
        `Viz bundle entries total ${totalEntryBytes} bytes exceeds ${MAX_DECOMPRESSED_SIZE} byte limit`,
      );
    }

    if ((typeFlag === 0x30 || typeFlag === 0) && size > 0 && name) {
      let cleanName = name.replace(/^\.\//, "");
      const firstSlash = cleanName.indexOf("/");
      if (firstSlash > 0 && !cleanName.substring(0, firstSlash).includes(".")) {
        cleanName = cleanName.substring(firstSlash + 1);
      }
      if (cleanName && !cleanName.includes("..") && !cleanName.startsWith("/")) {
        const data = Buffer.from(buf.subarray(offset, offset + size));
        files.set(cleanName, { data, contentType: getContentType(cleanName) });
        if (cleanName !== name.replace(/^\.\//, "")) {
          files.set(name.replace(/^\.\//, ""), { data, contentType: getContentType(name) });
        }
      }
    }

    offset += Math.ceil(size / 512) * 512;
  }

  return files;
}

/**
 * Path-based viz bundle file server.
 *
 * URL pattern: /api/viz/bundle/<base64url-encoded-tarball-url>/<file-path>
 *
 * Example:
 *   /api/viz/bundle/aHR0cHM6Ly9sZXR0dWNlLnNjaWVuY2UvYmluYXJpZXMvbmJvZHktY2x1c3Rlci12aXoudGFyLmd6/index.html
 *   /api/viz/bundle/aHR0cHM6Ly9sZXR0dWNlLnNjaWVuY2UvYmluYXJpZXMvbmJvZHktY2x1c3Rlci12aXoudGFyLmd6/lib/three.module.min.js
 *
 * Relative paths in HTML (href="style.css", src="main.js") resolve naturally
 * because the browser treats the URL as a directory structure.
 *
 * SECURITY (C3):
 *   - Origin isolation: when VIZ_ORIGIN is set, this route only answers requests
 *     whose Host matches the VIZ_ORIGIN host, so author JS never executes on the
 *     main app origin. (VIZ_ORIGIN unset = local dev; check skipped.)
 *   - Origin allowlist: the decoded tarball URL's origin must be in
 *     VIZ_BUNDLE_ALLOWED_ORIGINS (default: PLATFORM_URL origin).
 *   - SSRF defense-in-depth: resolved IPs validated against private/loopback
 *     ranges; redirects rejected.
 */
export async function GET(
  request: NextRequest,
  { params }: { params: Promise<{ url: string; path: string[] }> },
) {
  // --- Origin isolation: bind to the VIZ_ORIGIN host in production ---
  const vizOrigin = process.env.VIZ_ORIGIN;
  if (vizOrigin && vizOrigin.trim()) {
    let vizHost: string | null = null;
    try {
      vizHost = new URL(vizOrigin).host;
    } catch {
      vizHost = null;
    }
    const requestHost = request.headers.get("host");
    // If VIZ_ORIGIN is misconfigured (unparseable) or the incoming Host doesn't
    // match the isolated viz host, do not serve the bundle from this origin.
    if (!vizHost || !requestHost || requestHost.toLowerCase() !== vizHost.toLowerCase()) {
      return NextResponse.json(
        { error: { code: "NOT_FOUND", message: "Not found" } },
        { status: 404 },
      );
    }
  }

  const { url: encodedUrl, path: pathSegments } = await params;

  // Decode the base64url-encoded tarball URL.
  let tarballUrl: string;
  try {
    tarballUrl = Buffer.from(encodedUrl, "base64url").toString("utf8");
  } catch {
    return NextResponse.json(
      { error: { code: "INVALID_URL", message: "Invalid base64url-encoded URL" } },
      { status: 400 },
    );
  }

  const file = pathSegments.join("/");

  if (!isValidFilePath(file)) {
    return NextResponse.json(
      { error: { code: "INVALID_PATH", message: "Invalid file path" } },
      { status: 400 },
    );
  }

  // --- Strict origin allowlist (core fix) ---
  const tarballOrigin = normalizeOrigin(tarballUrl);
  if (!tarballOrigin) {
    return NextResponse.json(
      { error: { code: "INVALID_URL", message: "Tarball URL is not a valid http(s) URL" } },
      { status: 400 },
    );
  }
  const allowedOrigins = getAllowedOrigins();
  if (!allowedOrigins.has(tarballOrigin)) {
    return NextResponse.json(
      { error: { code: "ORIGIN_NOT_ALLOWED", message: "Tarball origin is not allowlisted" } },
      { status: 400 },
    );
  }

  try {
    const files = await fetchAndCacheTarball(tarballUrl);
    const entry = files.get(file);

    if (!entry) {
      return NextResponse.json(
        { error: { code: "NOT_FOUND", message: `File '${file}' not found in bundle` } },
        { status: 404 },
      );
    }

    const frameAncestors = process.env.PLATFORM_URL || "'self'";
    return new NextResponse(new Uint8Array(entry.data), {
      status: 200,
      headers: {
        "Content-Type": entry.contentType,
        "Cache-Control": "public, max-age=3600",
        "X-Content-Type-Options": "nosniff",
        "Content-Security-Policy": `frame-ancestors ${frameAncestors}`,
      },
    });
  } catch (err) {
    if (err instanceof SsrfError) {
      return NextResponse.json(
        { error: { code: "SSRF_BLOCKED", message: err.message } },
        { status: 400 },
      );
    }
    // F2: distinguish "the upstream bundle is hostile/oversized" (400) from a
    // generic upstream failure (502). On a too-large breach we have NOT
    // cached anything (cache write only happens after a successful extract),
    // so a retry won't serve poisoned data from the in-memory cache.
    if (err instanceof BundleTooLargeError) {
      return NextResponse.json(
        { error: { code: "BUNDLE_TOO_LARGE", message: err.message } },
        { status: 400 },
      );
    }
    const message = err instanceof Error ? err.message : "Unknown error";
    return NextResponse.json(
      { error: { code: "BUNDLE_FETCH_ERROR", message } },
      { status: 502 },
    );
  }
}
