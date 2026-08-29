/** Shared task-status display constants used by both card and table views. */

/** Maps task_status values to Tailwind dot color classes. */
export const STATUS_DOT_COLOR: Record<string, string> = {
  running: "bg-green-500",
  suspended_user: "bg-yellow-500",
  suspended_thermal: "bg-yellow-500",
  suspended_scheduled: "bg-yellow-500",
  error: "bg-red-500",
  queued: "bg-yellow-400",
};

/** Full status labels for card view (more descriptive). */
export const STATUS_TEXT: Record<string, string> = {
  running: "Running",
  suspended_user: "Suspended",
  suspended_thermal: "Suspended \u2014 thermal",
  suspended_scheduled: "Suspended \u2014 scheduled",
  error: "Error",
  queued: "Queued",
};

/** Runtime type badge styling. */
export const RUNTIME_BADGE: Record<string, { label: string; className: string }> = {
  native: { label: "Native", className: "bg-blue-500/10 text-blue-600 border-blue-500/20" },
  container: { label: "Container", className: "bg-purple-500/10 text-purple-600 border-purple-500/20" },
  wasm: { label: "WASM", className: "bg-emerald-500/10 text-emerald-600 border-emerald-500/20" },
};

/** Compact status labels for table view (space-constrained). */
export const STATUS_TEXT_SHORT: Record<string, string> = {
  running: "Running",
  suspended_user: "Suspended",
  suspended_thermal: "Thermal",
  suspended_scheduled: "Scheduled",
  error: "Error",
  queued: "Queued",
};
