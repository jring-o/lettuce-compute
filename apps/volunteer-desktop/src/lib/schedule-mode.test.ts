import { describe, it, expect } from "vitest";
import {
  buildInitSchedule,
  describeWindow,
  formatDays,
  formatHour,
  fromConfigScheduleMode,
  normaliseWindow,
  toConfigScheduleMode,
  toInitScheduleFlag,
  WEEKDAYS,
} from "./schedule-mode";

describe("schedule-mode mapping", () => {
  it("maps wizard modes to config.yaml values", () => {
    expect(toConfigScheduleMode("always")).toBe("ALWAYS");
    expect(toConfigScheduleMode("idle")).toBe("WHEN_IDLE");
    expect(toConfigScheduleMode("scheduled")).toBe("SCHEDULED");
  });

  it("reads config.yaml values back, defaulting unknowns to always", () => {
    expect(fromConfigScheduleMode("ALWAYS")).toBe("always");
    expect(fromConfigScheduleMode("WHEN_IDLE")).toBe("idle");
    expect(fromConfigScheduleMode("SCHEDULED")).toBe("scheduled");
    expect(fromConfigScheduleMode("")).toBe("always");
    expect(fromConfigScheduleMode(undefined)).toBe("always");
    expect(fromConfigScheduleMode("CRON")).toBe("always");
  });

  it("sends init --schedule-mode always for scheduled (the window comes from schedule set)", () => {
    expect(toInitScheduleFlag("always")).toBe("always");
    expect(toInitScheduleFlag("idle")).toBe("idle");
    expect(toInitScheduleFlag("scheduled")).toBe("always");
  });
});

describe("buildInitSchedule", () => {
  const window = { from_hour: 20, to_hour: 6, days: [...WEEKDAYS] };

  it("always: no idle threshold, no window", () => {
    expect(buildInitSchedule({ mode: "always", idleThresholdMins: 5, window })).toEqual({
      schedule_mode: "always",
      idle_threshold_mins: null,
      schedule_window: null,
    });
  });

  it("idle: carries the threshold, no window", () => {
    expect(buildInitSchedule({ mode: "idle", idleThresholdMins: 12, window })).toEqual({
      schedule_mode: "idle",
      idle_threshold_mins: 12,
      schedule_window: null,
    });
  });

  it("scheduled: init runs as always and the window is attached", () => {
    expect(
      buildInitSchedule({
        mode: "scheduled",
        idleThresholdMins: 5,
        window: { from_hour: 19, to_hour: 7, days: ["fri", "mon", "mon"] },
      })
    ).toEqual({
      schedule_mode: "always",
      idle_threshold_mins: null,
      schedule_window: { from_hour: 19, to_hour: 7, days: ["mon", "fri"] },
    });
  });
});

describe("window helpers", () => {
  it("formats whole hours the way schedule set accepts them", () => {
    expect(formatHour(6)).toBe("06:00");
    expect(formatHour(20)).toBe("20:00");
    expect(formatHour(0)).toBe("00:00");
    expect(formatHour(99)).toBe("23:00");
  });

  it("formats --days as comma-separated names in week order", () => {
    expect(formatDays(["sun", "sat"])).toBe("sat,sun");
    expect(formatDays([...WEEKDAYS])).toBe("mon,tue,wed,thu,fri,sat,sun");
  });

  it("normalises days into week order without duplicates and clamps hours", () => {
    expect(
      normaliseWindow({ from_hour: -3, to_hour: 30, days: ["wed", "mon", "wed"] })
    ).toEqual({ from_hour: 0, to_hour: 23, days: ["mon", "wed"] });
  });

  it("describes windows like the CLI does", () => {
    expect(describeWindow({ from_hour: 20, to_hour: 6, days: [...WEEKDAYS] })).toBe(
      "20:00–06:00 (overnight) on every day"
    );
    expect(describeWindow({ from_hour: 9, to_hour: 17, days: ["mon", "tue"] })).toBe(
      "09:00–17:00 on Mon, Tue"
    );
    expect(describeWindow({ from_hour: 0, to_hour: 0, days: ["sat", "sun"] })).toBe(
      "all day on Sat, Sun"
    );
    expect(describeWindow({ from_hour: 1, to_hour: 2, days: [] })).toBe(
      "01:00–02:00 on no days selected"
    );
  });
});
