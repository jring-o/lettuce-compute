import { describe, it, expect, beforeEach } from "vitest";
import {
  cn,
  formatBytes,
  formatDuration,
  formatPercent,
  formatCredit,
  formatAge,
  formatSizeMb,
  formatSizePairMb,
  formatGb,
  pausedLabel,
  formatDateTime,
  readStoredTheme,
  storeTheme,
  applyTheme,
} from "./utils";

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

describe("formatAge", () => {
  const now = Date.parse("2026-08-29T12:00:00Z");

  it("renders under a minute as just now", () => {
    expect(formatAge("2026-08-29T11:59:30Z", now)).toBe("just now");
  });

  it("renders minutes, hours and days", () => {
    expect(formatAge("2026-08-29T11:45:00Z", now)).toBe("15m ago");
    expect(formatAge("2026-08-29T09:00:00Z", now)).toBe("3h ago");
    expect(formatAge("2026-08-27T11:00:00Z", now)).toBe("2d ago");
  });

  it("clamps a timestamp in the future to just now", () => {
    expect(formatAge("2026-08-29T12:05:00Z", now)).toBe("just now");
  });

  it("renders an unparseable timestamp as unknown", () => {
    expect(formatAge("not a date", now)).toBe("unknown");
  });
});

describe("formatSizeMb", () => {
  it("renders whole gigabytes without a decimal", () => {
    expect(formatSizeMb(15360)).toBe("15 GB");
    expect(formatSizeMb(1024)).toBe("1 GB");
  });

  it("renders fractional gigabytes with one decimal", () => {
    expect(formatSizeMb(1536)).toBe("1.5 GB");
    expect(formatSizeMb(7000)).toBe("6.8 GB");
  });

  it("renders under a gigabyte in MB", () => {
    expect(formatSizeMb(512)).toBe("512 MB");
  });
});

describe("formatCredit with decimals", () => {
  it("keeps whole numbers grouped and cuts fractions to two decimals", () => {
    expect(formatCredit(1234)).toBe("1,234");
    expect(formatCredit(1234.5678)).toBe("1,234.57");
    expect(formatCredit(0.5)).toBe("0.5");
    expect(formatCredit(NaN)).toBe("0");
  });
});

describe("formatGb", () => {
  it("renders megabytes as gigabytes with one decimal", () => {
    expect(formatGb(1024)).toBe("1.0 GB");
    expect(formatGb(5734)).toBe("5.6 GB");
    expect(formatGb(0)).toBe("0.0 GB");
  });
});

describe("pausedLabel", () => {
  it("words the scheduled reason and passes others through", () => {
    expect(pausedLabel(null)).toBe("Paused");
    expect(pausedLabel(undefined)).toBe("Paused");
    expect(pausedLabel("scheduled")).toBe("Paused — outside your schedule");
    expect(pausedLabel("thermal")).toBe("Paused — thermal");
    expect(pausedLabel("user")).toBe("Paused — user");
  });
});

describe("formatDateTime", () => {
  it("returns the input when it is not a date", () => {
    expect(formatDateTime("not a date")).toBe("not a date");
  });

  it("formats a valid timestamp as a short local date and time", () => {
    const out = formatDateTime("2030-01-02T03:04:00Z");
    expect(out).toMatch(/Jan/);
    expect(out).toMatch(/\d/);
  });
});

describe("theme helpers", () => {
  beforeEach(() => {
    localStorage.removeItem("lettuce-theme");
    document.documentElement.classList.remove("dark");
  });

  it("defaults to system and round-trips a stored choice", () => {
    expect(readStoredTheme()).toBe("system");
    storeTheme("dark");
    expect(readStoredTheme()).toBe("dark");
    localStorage.setItem("lettuce-theme", "purple");
    expect(readStoredTheme()).toBe("system");
  });

  it("applies dark and light directly", () => {
    applyTheme("dark");
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    applyTheme("light");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });

  it("follows the OS preference for system and stops on cleanup", () => {
    const listeners: Array<() => void> = [];
    const original = window.matchMedia;
    let matches = true;
    Object.defineProperty(window, "matchMedia", {
      writable: true,
      value: () => ({
        get matches() {
          return matches;
        },
        addEventListener: (_: string, fn: () => void) => listeners.push(fn),
        removeEventListener: (_: string, fn: () => void) => {
          const i = listeners.indexOf(fn);
          if (i >= 0) listeners.splice(i, 1);
        },
      }),
    });
    try {
      const stop = applyTheme("system");
      expect(document.documentElement.classList.contains("dark")).toBe(true);
      matches = false;
      listeners.forEach((fn) => fn());
      expect(document.documentElement.classList.contains("dark")).toBe(false);
      stop();
      expect(listeners).toHaveLength(0);
    } finally {
      Object.defineProperty(window, "matchMedia", { writable: true, value: original });
    }
  });
});

// TB-66: a 7000 MB requirement and a 6912 MB allowance both rounded to
// "6.8 GB", so the card read "6.8 GB RAM (you allow 6.8 GB)" while the head
// refused the machine by 88 MB.
describe("formatSizePairMb", () => {
  it("prints both figures in MB when either would be rounded", () => {
    expect(formatSizePairMb(7000, 6912)).toEqual(["7000 MB", "6912 MB"]);
    expect(formatSizePairMb(7000, 6656)).toEqual(["7000 MB", "6656 MB"]);
    expect(formatSizePairMb(14340, 14336)).toEqual(["14340 MB", "14336 MB"]);
    expect(formatSizePairMb(1536, 1024)).toEqual(["1536 MB", "1024 MB"]);
  });

  it("keeps whole gigabytes short", () => {
    expect(formatSizePairMb(16384, 8192)).toEqual(["16 GB", "8 GB"]);
    expect(formatSizePairMb(15360, 10240)).toEqual(["15 GB", "10 GB"]);
    expect(formatSizePairMb(3072, 2048)).toEqual(["3 GB", "2 GB"]);
  });

  it("leaves sizes under a gigabyte in MB as before", () => {
    expect(formatSizePairMb(512, 256)).toEqual(["512 MB", "256 MB"]);
  });

  it("never prints two different sizes as the same label", () => {
    for (const [need, have] of [
      [7000, 6912],
      [7000, 7168],
      [8000, 7936],
      [14340, 14336],
      [1100, 1050],
    ] as const) {
      const [a, b] = formatSizePairMb(need, have);
      expect(a, `${need} vs ${have}`).not.toBe(b);
    }
  });
});
