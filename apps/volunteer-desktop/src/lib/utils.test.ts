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
  formatExactMb,
  pausedLabel,
  pauseIsResumable,
  pausedExplanation,
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
  it("returns MiB for values under 1024", () => {
    expect(formatBytes(512)).toBe("512 MiB");
  });

  it("returns MiB for zero", () => {
    expect(formatBytes(0)).toBe("0 MiB");
  });

  it("converts to GiB at 1024 MiB", () => {
    expect(formatBytes(1024)).toBe("1.0 GiB");
  });

  it("converts to GiB with one decimal for values above 1024", () => {
    expect(formatBytes(2048)).toBe("2.0 GiB");
    expect(formatBytes(1536)).toBe("1.5 GiB");
  });

  it("handles fractional GiB correctly", () => {
    // 1500 MiB = 1.46... GiB -> "1.5 GiB"
    expect(formatBytes(1500)).toBe("1.5 GiB");
  });
});

// TB-78: every size the app prints is a MiB value — the daemon, `doctor` and
// the heads work in MiB — but the labels said "MB" and "GB", so the Memory
// slider's 6912 stop read "6.8 GB" (it is 6.75 GiB; neither 6800 decimal MB
// nor the 6912 the head compares), consecutive 256-MiB stops read 6.5 / 6.8 /
// 7.0, and a tester reconciling Podman Desktop (decimal GB), the kernel (MiB)
// and Lettuce could not tell which unit he was reading.
describe("binary sizes carry binary prefixes (TB-78)", () => {
  it("never labels a MiB value GB or MB", () => {
    for (const mb of [0, 512, 1024, 1500, 6912, 7000, 7168, 16384]) {
      for (const label of [formatBytes(mb), formatSizeMb(mb), formatGb(mb), formatExactMb(mb)]) {
        expect(label, `${mb}`).not.toMatch(/\b[GM]B\b/);
        expect(label, `${mb}`).toMatch(/ [GM]iB$/);
      }
    }
    for (const label of formatSizePairMb(7000, 6912)) {
      expect(label).not.toMatch(/\b[GM]B\b/);
    }
  });

  it("rounds 6912 MiB to 6.8 GiB, not 6.8 GB", () => {
    expect(formatBytes(6912)).toBe("6.8 GiB");
    expect(formatSizeMb(6912)).toBe("6.8 GiB");
  });

  it("prints a slider stop exactly, so neighbouring stops never read alike", () => {
    const stops = [6656, 6912, 7168].map(formatExactMb);
    expect(stops).toEqual(["6656 MiB", "6912 MiB", "7168 MiB"]);
    expect(new Set(stops).size).toBe(3);
    expect(formatExactMb(16384)).toBe("16384 MiB");
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
    expect(formatSizeMb(15360)).toBe("15 GiB");
    expect(formatSizeMb(1024)).toBe("1 GiB");
  });

  it("renders fractional gigabytes with one decimal", () => {
    expect(formatSizeMb(1536)).toBe("1.5 GiB");
    expect(formatSizeMb(7000)).toBe("6.8 GiB");
  });

  it("renders under a gigabyte in MiB", () => {
    expect(formatSizeMb(512)).toBe("512 MiB");
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
  it("renders MiB as GiB with one decimal", () => {
    expect(formatGb(1024)).toBe("1.0 GiB");
    expect(formatGb(5734)).toBe("5.6 GiB");
    expect(formatGb(0)).toBe("0.0 GiB");
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

  it("words a yield pause as the computer being busy (TB-83)", () => {
    expect(pausedLabel("busy")).toBe("Paused — your computer is busy");
  });

  it("does not call an unreadable idle time an ordinary schedule pause", () => {
    expect(pausedLabel("idle_unknown")).toBe("Paused — can't tell when this computer is idle");
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
  it("prints both figures in MiB when either would be rounded", () => {
    expect(formatSizePairMb(7000, 6912)).toEqual(["7000 MiB", "6912 MiB"]);
    expect(formatSizePairMb(7000, 6656)).toEqual(["7000 MiB", "6656 MiB"]);
    expect(formatSizePairMb(14340, 14336)).toEqual(["14340 MiB", "14336 MiB"]);
    expect(formatSizePairMb(1536, 1024)).toEqual(["1536 MiB", "1024 MiB"]);
  });

  it("keeps whole gigabytes short", () => {
    expect(formatSizePairMb(16384, 8192)).toEqual(["16 GiB", "8 GiB"]);
    expect(formatSizePairMb(15360, 10240)).toEqual(["15 GiB", "10 GiB"]);
    expect(formatSizePairMb(3072, 2048)).toEqual(["3 GiB", "2 GiB"]);
  });

  it("leaves sizes under a gigabyte in MiB as before", () => {
    expect(formatSizePairMb(512, 256)).toEqual(["512 MiB", "256 MiB"]);
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

describe("pauseIsResumable / pausedExplanation (TB-72)", () => {
  it("only a user pause is the daemon's resume to undo", () => {
    expect(pauseIsResumable("user")).toBe(true);
    expect(pauseIsResumable("scheduled")).toBe(false);
    expect(pauseIsResumable("thermal")).toBe(false);
    expect(pauseIsResumable(null)).toBe(false);
    expect(pauseIsResumable(undefined)).toBe(false);
  });

  it("explains each pause with what ends it", () => {
    expect(pausedExplanation("user")).toBe("Computing is paused. Resume to start contributing.");
    expect(pausedExplanation("scheduled")).toMatch(/schedule.*Settings/);
    expect(pausedExplanation("thermal")).toMatch(/cools down/);
    expect(pausedExplanation("busy")).toMatch(/other programs are using the CPU/);
    expect(pauseIsResumable("busy")).toBe(false);
    expect(pausedExplanation(null)).toBe("Computing is paused.");
  });

  it("says an unreadable idle time will not end on its own", () => {
    expect(pausedExplanation("idle_unknown")).toMatch(/cannot tell when it is idle.*will not start on its own.*Settings/);
    expect(pauseIsResumable("idle_unknown")).toBe(false);
  });
});
