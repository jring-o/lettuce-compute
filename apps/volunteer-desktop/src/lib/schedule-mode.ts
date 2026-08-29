/**
 * The one place the app maps between its three ways of naming a schedule:
 *
 * - the wizard's choice (`WizardScheduleMode`, lowercase, what the user picked);
 * - the CLI's `init --schedule-mode` flag (`always|idle|scheduled`, see
 *   `services/volunteer-cli/internal/cli/init.go`);
 * - the value stored in `config.yaml` under `scheduling.mode`
 *   (`ALWAYS|WHEN_IDLE|SCHEDULED`), which the daemon's config API reports.
 *
 * Scheduled windows are the CLI's own model — `schedule set --from HH:00 --to
 * HH:00 --days ...` (see `schedule.go`): whole hours, a window that may wrap
 * past midnight, and a set of weekdays. The wizard collects exactly those
 * three values and hands them to `run_init`, which runs `schedule set` after
 * `init` and before the daemon starts.
 */

export type WizardScheduleMode = "always" | "idle" | "scheduled";

/** `scheduling.mode` as written to config.yaml. */
export type ConfigScheduleMode = "ALWAYS" | "WHEN_IDLE" | "SCHEDULED";

/** Value accepted by `lettuce-volunteer init --schedule-mode`. */
export type InitScheduleFlag = "always" | "idle" | "scheduled";

/** Day names exactly as `schedule set --days` accepts them. */
export type Weekday = "mon" | "tue" | "wed" | "thu" | "fri" | "sat" | "sun";

export const WEEKDAYS: readonly Weekday[] = [
  "mon",
  "tue",
  "wed",
  "thu",
  "fri",
  "sat",
  "sun",
];

export const WEEKDAY_LABELS: Record<Weekday, string> = {
  mon: "Mon",
  tue: "Tue",
  wed: "Wed",
  thu: "Thu",
  fri: "Fri",
  sat: "Sat",
  sun: "Sun",
};

/**
 * One daily window, in the shape `run_init` forwards to `schedule set`.
 * Hours are 0–23; `to_hour <= from_hour` means the window wraps past midnight
 * (and equal hours mean the whole day, exactly as the CLI treats them).
 */
export interface ScheduleWindow {
  from_hour: number;
  to_hour: number;
  days: Weekday[];
}

export interface ScheduleChoice {
  mode: WizardScheduleMode;
  idleThresholdMins: number;
  window: ScheduleWindow;
}

/** The schedule-related fields of the `run_init` payload. */
export interface InitSchedulePayload {
  schedule_mode: InitScheduleFlag;
  idle_threshold_mins: number | null;
  schedule_window: ScheduleWindow | null;
}

export function toConfigScheduleMode(mode: WizardScheduleMode): ConfigScheduleMode {
  switch (mode) {
    case "idle":
      return "WHEN_IDLE";
    case "scheduled":
      return "SCHEDULED";
    default:
      return "ALWAYS";
  }
}

/** Reverse of `toConfigScheduleMode`; unknown or empty values read as "always". */
export function fromConfigScheduleMode(mode: string | undefined | null): WizardScheduleMode {
  switch ((mode ?? "").toUpperCase()) {
    case "WHEN_IDLE":
      return "idle";
    case "SCHEDULED":
      return "scheduled";
    default:
      return "always";
  }
}

/**
 * The `init --schedule-mode` value for a wizard choice.
 *
 * "scheduled" deliberately maps to `always`: a non-interactive
 * `init --schedule-mode scheduled` writes `SCHEDULED` with no window and the
 * CLI refuses to save it ("schedule_ranges is required when mode is
 * SCHEDULED"). The window is applied afterwards by `schedule set`, which
 * switches the mode to `SCHEDULED` itself.
 */
export function toInitScheduleFlag(mode: WizardScheduleMode): InitScheduleFlag {
  return mode === "idle" ? "idle" : "always";
}

/** Build the schedule fields of the `run_init` payload from the wizard's choice. */
export function buildInitSchedule(choice: ScheduleChoice): InitSchedulePayload {
  return {
    schedule_mode: toInitScheduleFlag(choice.mode),
    idle_threshold_mins: choice.mode === "idle" ? choice.idleThresholdMins : null,
    schedule_window:
      choice.mode === "scheduled" ? normaliseWindow(choice.window) : null,
  };
}

/** Days in week order with duplicates removed; hours clamped to 0–23. */
export function normaliseWindow(window: ScheduleWindow): ScheduleWindow {
  const chosen = new Set(window.days);
  return {
    from_hour: clampHour(window.from_hour),
    to_hour: clampHour(window.to_hour),
    days: WEEKDAYS.filter((d) => chosen.has(d)),
  };
}

function clampHour(h: number): number {
  if (!Number.isFinite(h)) return 0;
  return Math.min(23, Math.max(0, Math.trunc(h)));
}

/** `20` -> `"20:00"`, the whole-hour form `schedule set` accepts. */
export function formatHour(hour: number): string {
  return `${String(clampHour(hour)).padStart(2, "0")}:00`;
}

/** The `--days` value: comma-separated names in week order, e.g. `mon,tue,sat`. */
export function formatDays(days: Weekday[]): string {
  return normaliseWindow({ from_hour: 0, to_hour: 0, days }).days.join(",");
}

/**
 * A plain-language description of a window, mirroring the CLI's own summary
 * (`describeRange` in schedule.go): "20:00–06:00 (overnight) on every day".
 */
export function describeWindow(window: ScheduleWindow): string {
  const w = normaliseWindow(window);
  let span: string;
  if (w.from_hour === w.to_hour) {
    span = "all day";
  } else if (w.from_hour > w.to_hour) {
    span = `${formatHour(w.from_hour)}–${formatHour(w.to_hour)} (overnight)`;
  } else {
    span = `${formatHour(w.from_hour)}–${formatHour(w.to_hour)}`;
  }
  let days: string;
  if (w.days.length === 0) {
    days = "no days selected";
  } else if (w.days.length === WEEKDAYS.length) {
    days = "every day";
  } else {
    days = w.days.map((d) => WEEKDAY_LABELS[d]).join(", ");
  }
  return `${span} on ${days}`;
}
