/**
 * @jest-environment node
 */

import {
  anonVizRateLimiter,
  clientIpFrom,
  publicLeafVerdictCache,
  publicResultCache,
  resetPublicVizGuard,
} from "@/lib/public-viz-guard";

describe("public-viz guard primitives", () => {
  beforeEach(() => {
    jest.useFakeTimers();
    resetPublicVizGuard();
  });

  afterEach(() => {
    jest.useRealTimers();
  });

  describe("TTL caches", () => {
    it("returns a stored value inside the TTL and forgets it after", () => {
      publicLeafVerdictCache.set("leaf-1", true);
      expect(publicLeafVerdictCache.get("leaf-1")).toBe(true);

      jest.advanceTimersByTime(30_000); // the verdict TTL
      expect(publicLeafVerdictCache.get("leaf-1")).toBeUndefined();
    });

    it("caches negative verdicts too (a denied leaf is not re-fetched per request)", () => {
      publicLeafVerdictCache.set("leaf-1", false);
      expect(publicLeafVerdictCache.get("leaf-1")).toBe(false);
    });

    it("bounds its size by evicting the oldest entry", () => {
      for (let i = 0; i < 501; i++) {
        publicResultCache.set(`key-${i}`, { result: i });
      }
      expect(publicResultCache.get("key-0")).toBeUndefined();
      expect(publicResultCache.get("key-500")).toEqual({ result: 500 });
    });
  });

  describe("anonymous rate limiter", () => {
    it("allows up to the window limit and refuses the next request", () => {
      for (let i = 0; i < 120; i++) {
        expect(anonVizRateLimiter.allow("198.51.100.1")).toBe(true);
      }
      expect(anonVizRateLimiter.allow("198.51.100.1")).toBe(false);
    });

    it("keeps per-IP buckets independent", () => {
      for (let i = 0; i < 121; i++) anonVizRateLimiter.allow("198.51.100.1");
      expect(anonVizRateLimiter.allow("198.51.100.2")).toBe(true);
    });

    it("opens a fresh window after the interval elapses", () => {
      for (let i = 0; i < 121; i++) anonVizRateLimiter.allow("198.51.100.1");
      expect(anonVizRateLimiter.allow("198.51.100.1")).toBe(false);

      jest.advanceTimersByTime(60_000);
      expect(anonVizRateLimiter.allow("198.51.100.1")).toBe(true);
    });
  });

  describe("clientIpFrom", () => {
    const headersOf = (map: Record<string, string>) => ({
      get: (name: string) => map[name.toLowerCase()] ?? null,
    });

    it("takes the first X-Forwarded-For hop", () => {
      expect(
        clientIpFrom(headersOf({ "x-forwarded-for": "203.0.113.7, 10.0.0.1" })),
      ).toBe("203.0.113.7");
    });

    it("falls back to a shared bucket without the header", () => {
      expect(clientIpFrom(headersOf({}))).toBe("unknown");
    });
  });
});
