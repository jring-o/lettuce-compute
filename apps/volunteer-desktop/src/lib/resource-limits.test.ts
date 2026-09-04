import { describe, it, expect } from "vitest";
import {
  MEMORY_SLIDER_STEP_MB,
  memoryAllowanceCeilingMb,
  memorySliderMaxMb,
  memoryStopAtOrAboveMb,
} from "./resource-limits";

// TB-66: the Memory slider moves in 256 MB steps, so the stop just under a
// 7000 MB requirement (6912) still fails the head's check; the card must
// name the next stop up.
describe("memoryStopAtOrAboveMb", () => {
  it("rounds a requirement up to the next slider stop", () => {
    expect(MEMORY_SLIDER_STEP_MB).toBe(256);
    expect(memoryStopAtOrAboveMb(7000)).toBe(7168);
    expect(memoryStopAtOrAboveMb(8000)).toBe(8192);
    expect(memoryStopAtOrAboveMb(6913)).toBe(7168);
  });

  it("keeps a requirement that is already a stop", () => {
    expect(memoryStopAtOrAboveMb(7168)).toBe(7168);
    expect(memoryStopAtOrAboveMb(16384)).toBe(16384);
  });

  it("never goes below the slider's first stop", () => {
    expect(memoryStopAtOrAboveMb(100)).toBe(256);
    expect(memoryStopAtOrAboveMb(0)).toBe(256);
  });
});

describe("memory slider bounds", () => {
  it("ceiling is 90 % of RAM", () => {
    expect(memoryAllowanceCeilingMb(8192)).toBe(7373);
    expect(memoryAllowanceCeilingMb(16384)).toBe(14746);
  });

  it("the maximum never clamps an allowance already saved above the ceiling", () => {
    expect(memorySliderMaxMb(16384, 2048)).toBe(14746);
    expect(memorySliderMaxMb(16384, 16000)).toBe(16000);
  });
});
