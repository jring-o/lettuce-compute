import { vi } from "vitest";
import type { ActiveTaskInfo } from "@/api/client";
import type { TaskActions } from "./task-context-menu";

/** Build a valid ActiveTaskInfo with sensible defaults, overridable via partial. */
export function makeTask(
  overrides: Partial<ActiveTaskInfo> & { work_unit_id: string; leaf_name: string }
): ActiveTaskInfo {
  return {
    progress_pct: 50,
    elapsed_seconds: 500,
    cpu_seconds: 500,
    task_status: "running",
    status_reason: null,
    deadline_seconds: 86400,
    head_name: "lettuce.science",
    runtime_type: "native",
    process_id: 1234,
    work_dir: "/tmp/wu",
    ...overrides,
  };
}

/** Build a TaskActions object with all callbacks as vi.fn() mocks. */
export function makeActions(overrides?: Partial<TaskActions>): TaskActions {
  return {
    onSuspend: vi.fn(),
    onResume: vi.fn(),
    onAbort: vi.fn(),
    onShowDetails: vi.fn(),
    onCopyId: vi.fn(),
    ...overrides,
  };
}
