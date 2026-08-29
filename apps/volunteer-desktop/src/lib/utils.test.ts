import { describe, it, expect } from "vitest";
import { cn, formatBytes, formatDuration, formatPercent, formatCredit } from "./utils";

describe("cn", () => {
  it("merges class names", () => {
    expect(cn("foo", "bar")).toBe("foo bar");
  });

  it("handles conditional classes", () => {
    expect(cn("base", false && "hidden", "visible")).toBe("base visible");
  });

  it("deduplicates tailwind classes via twMerge", () => {
    // twMerge should resolve conflicting Tailwind utilities
    expect(cn("p-4", "p-2")).toBe("p-2");
  });

  it("handles undefined and null inputs", () => {
    expect(cn("base", undefined, null, "end")).toBe("base end");
  });

  it("returns empty string for no inputs", () => {
    expect(cn()).toBe("");
  });
});

describe("formatBytes", () => {
  it("returns MB for values under 1024", () => {
    expect(formatBytes(512)).toBe("512 MB");
  });

  it("returns MB for zero", () => {
    expect(formatBytes(0)).toBe("0 MB");
  });

  it("converts to GB at 1024 MB", () => {
    expect(formatBytes(1024)).toBe("1.0 GB");
  });

  it("converts to GB with one decimal for values above 1024", () => {
    expect(formatBytes(2048)).toBe("2.0 GB");
    expect(formatBytes(1536)).toBe("1.5 GB");
  });

  it("handles fractional GB correctly", () => {
    // 1500 MB = 1.46... GB -> "1.5 GB"
    expect(formatBytes(1500)).toBe("1.5 GB");
  });
});

describe("formatDuration", () => {
  it("returns seconds for values under 60", () => {
    expect(formatDuration(0)).toBe("0s");
    expect(formatDuration(30)).toBe("30s");
    expect(formatDuration(59)).toBe("59s");
  });

  it("returns minutes and seconds for values under 3600", () => {
    expect(formatDuration(60)).toBe("1m 0s");
    expect(formatDuration(90)).toBe("1m 30s");
    expect(formatDuration(3599)).toBe("59m 59s");
  });

  it("returns hours and minutes for values >= 3600", () => {
    expect(formatDuration(3600)).toBe("1h 0m");
    expect(formatDuration(3661)).toBe("1h 1m");
    expect(formatDuration(7200)).toBe("2h 0m");
  });

  it("drops remaining seconds in the hours range", () => {
    // 3661 = 1h 1m 1s -> should show "1h 1m" (seconds dropped)
    expect(formatDuration(3661)).toBe("1h 1m");
  });
});

describe("formatPercent", () => {
  it("rounds to nearest integer", () => {
    expect(formatPercent(50)).toBe("50%");
    expect(formatPercent(99.9)).toBe("100%");
    expect(formatPercent(0.4)).toBe("0%");
  });

  it("handles zero", () => {
    expect(formatPercent(0)).toBe("0%");
  });

  it("handles 100", () => {
    expect(formatPercent(100)).toBe("100%");
  });
});

describe("formatCredit", () => {
  it("formats zero as '0'", () => {
    expect(formatCredit(0)).toBe("0");
  });

  it("formats small numbers without separators", () => {
    expect(formatCredit(42)).toBe("42");
  });

  it("formats thousands with locale separators", () => {
    const result = formatCredit(1234567);
    // toLocaleString output varies by locale, but should contain the digits
    expect(result).toContain("1");
    expect(result).toContain("234");
    expect(result).toContain("567");
    // Should have some separator (comma in en-US)
    expect(result.length).toBeGreaterThan(7);
  });

  it("formats negative numbers", () => {
    const result = formatCredit(-500);
    expect(result).toContain("500");
  });
});
