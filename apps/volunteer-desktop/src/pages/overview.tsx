import { useState, useMemo, useEffect, useCallback } from "react";
import { emit } from "@tauri-apps/api/event";
import { useDaemonStatus } from "@/hooks/use-daemon-status";
import { useMetrics, useSystemMetrics } from "@/hooks/use-metrics";
import { useCredit } from "@/hooks/use-credit";
import { useClient, useApiQuery } from "@/hooks/use-api";
import { useContainerRuntime } from "@/hooks/use-container-runtime";
import { VizFrame, describeVizUnavailable, type VizUnavailableReason } from "@/components/viz/VizFrame";
import { ResourceGauge } from "@/components/resource-gauge";
import { CreditDisplay } from "@/components/credit-display";
import { NoticesPanel } from "@/components/notices-panel";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { TaskContextMenu, type TaskActions } from "@/components/tasks/task-context-menu";
import { ActiveTaskTable } from "@/components/tasks/active-task-table";
import { TaskDetailPanel } from "@/components/tasks/task-detail-panel";
import { TaskFilters, applyTaskFilters, isQueuedTask, type TaskFilterState } from "@/components/tasks/task-filters";
import { STATUS_DOT_COLOR, STATUS_TEXT, RUNTIME_BADGE } from "@/components/tasks/task-status";
import {
  cn,
  formatDuration,
  formatTimeAgo,
  formatBytes,
  formatCredit,
  formatGb,
  formatDateTime,
  pausedLabel,
  pauseIsResumable,
  pausedExplanation,
} from "@/lib/utils";
import {
  getClientVersion,
  type ActiveTaskInfo,
  type QueuedTaskInfo,
  type CreditSummary,
  type FailingLeaf,
  type HeadInfo,
  type MachineCapabilities,
  type ManagementClient,
} from "@/api/client";

function StatusBadge({
  state,
  pausedReason,
}: {
  state: string;
  pausedReason: string | null;
}) {
  const config = {
    active: { label: "Active", bg: "bg-green-500/10 text-green-600 border-green-500/20" },
    paused: {
      label: pausedLabel(pausedReason),
      bg: "bg-yellow-500/10 text-yellow-600 border-yellow-500/20",
    },
    stopped: { label: "Stopped", bg: "bg-gray-500/10 text-gray-500 border-gray-500/20" },
  };
  const c = config[state as keyof typeof config] ?? config.stopped;

  return (
    <span className={cn("inline-flex items-center rounded-full border px-3 py-1 text-sm font-medium", c.bg)}>
      {c.label}
    </span>
  );
}

function ActiveTaskCard({ task, actions, isVizActive }: { task: ActiveTaskInfo; actions: TaskActions; isVizActive?: boolean }) {
  const [contextOpen, setContextOpen] = useState(false);
  const progressKnown = task.progress_pct > 0;
  const remaining = task.estimated_remaining_seconds ?? null;
  const isSuspended = task.task_status.startsWith("suspended");
  const pausedSeconds = task.elapsed_seconds - task.cpu_seconds;
  const runtime = RUNTIME_BADGE[task.runtime_type];

  const handleContextMenu = (e: React.MouseEvent) => {
    e.preventDefault();
    setContextOpen(true);
  };

  return (
    <Card className={cn("group", isVizActive && "ring-2 ring-primary/30")} onContextMenu={handleContextMenu}>
      <CardContent className="p-4 space-y-2">
        {/* Row 1: Leaf name + status dot + status text + overflow menu */}
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <span className={cn("h-2.5 w-2.5 rounded-full", STATUS_DOT_COLOR[task.task_status] ?? "bg-gray-500")} />
            <span className="font-medium text-sm">{task.leaf_name}</span>
            <span className="text-xs text-muted-foreground">
              {STATUS_TEXT[task.task_status] ?? task.task_status}
            </span>
          </div>
          <div onClick={(e) => e.stopPropagation()}>
            <TaskContextMenu
              task={task}
              actions={actions}
              open={contextOpen}
              onOpenChange={setContextOpen}
              trigger={
                <button className="opacity-0 group-hover:opacity-100 transition-opacity p-1 rounded hover:bg-accent text-muted-foreground text-sm">
                  ···
                </button>
              }
            />
          </div>
        </div>

        {/* Row 2: WU ID + runtime badge + resumed badge */}
        <div className="flex items-center gap-2">
          <code className="text-xs text-muted-foreground font-mono">
            {task.work_unit_id.slice(0, 8)}
          </code>
          {runtime && (
            <span className={cn("inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-medium", runtime.className)}>
              {runtime.label}
            </span>
          )}
          {task.viz_bundle_path && (
            <span className="inline-flex items-center rounded border border-indigo-500/20 bg-indigo-500/10 px-1.5 py-0.5 text-[10px] font-medium text-indigo-600">
              Viz
            </span>
          )}
          {task.resumed_from_checkpoint && (
            <span className="inline-flex items-center rounded-full border border-blue-500/20 bg-blue-500/10 px-2 py-0.5 text-[10px] font-medium text-blue-600">
              Resumed from checkpoint
            </span>
          )}
        </div>

        {/* Row 3: Progress bar with percentage */}
        {progressKnown ? (
          <div className="space-y-1">
            <div className="h-2 rounded-full bg-secondary overflow-hidden">
              <div
                className="h-full rounded-full bg-primary transition-all duration-300"
                style={{ width: `${Math.min(100, task.progress_pct)}%` }}
              />
            </div>
            <div className="text-xs text-muted-foreground text-right">
              {Math.round(task.progress_pct)}%
            </div>
          </div>
        ) : (
          <div className="text-xs text-muted-foreground">In progress...</div>
        )}

        {/* Row 4: CPU time + pause breakdown + estimated remaining */}
        <div className="flex items-center justify-between text-xs text-muted-foreground">
          <span>
            Crunching: {formatDuration(task.cpu_seconds)}
            {isSuspended && pausedSeconds > 0 && ` · Paused ${formatDuration(pausedSeconds)}`}
          </span>
          {remaining != null && remaining > 0 && (
            <span>~{formatDuration(remaining)} remaining</span>
          )}
        </div>

        {/* Row 5: Deadline countdown */}
        <div className={cn(
          "text-xs",
          task.deadline_seconds < 0
            ? "text-red-500"
            : task.deadline_seconds < 7200
              ? "text-yellow-500"
              : "text-muted-foreground"
        )}>
          {task.deadline_seconds < 0
            ? `Overdue by ${formatDuration(Math.abs(task.deadline_seconds))}`
            : `${formatDuration(task.deadline_seconds)} deadline`}
        </div>

        {/* Row 6: Checkpoint info */}
        {task.checkpoint_sequence != null && task.checkpoint_sequence > 0 && (
          <div className="text-xs text-muted-foreground">
            Checkpoint: seq {task.checkpoint_sequence}
            {task.last_checkpoint_at && ` saved ${formatTimeAgo(task.last_checkpoint_at)}`}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function QueuedTaskCard({ task }: { task: QueuedTaskInfo }) {
  const fetchedAt = new Date(task.fetched_at);
  const elapsed = Math.floor((Date.now() - fetchedAt.getTime()) / 1000);
  const remaining = Math.max(0, task.deadline_seconds - elapsed);

  return (
    <div className="flex items-center justify-between px-4 py-2 rounded-lg border bg-muted/30">
      <div className="flex items-center gap-2">
        <span className="h-2 w-2 rounded-full bg-yellow-400" />
        <span className="text-sm">{task.leaf_name}</span>
        <code className="text-xs text-muted-foreground font-mono">
          {task.work_unit_id.slice(0, 8)}
        </code>
      </div>
      <span className="text-xs text-muted-foreground">
        {formatDuration(remaining)} deadline
      </span>
    </div>
  );
}

function PulsingDot() {
  return (
    <span className="relative flex h-2.5 w-2.5">
      <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-primary opacity-75" />
      <span className="relative inline-flex h-2.5 w-2.5 rounded-full bg-primary" />
    </span>
  );
}

function CreditBreakdownSection({ credit }: { credit: CreditSummary }) {
  const [expanded, setExpanded] = useState(false);

  if (credit.by_head.length === 0 && credit.by_leaf.length === 0) return null;

  return (
    <div className="space-y-2">
      <button
        onClick={() => setExpanded(!expanded)}
        className="flex items-center gap-2 text-xs font-medium text-muted-foreground hover:text-foreground transition-colors"
      >
        <span className={cn("transition-transform text-[10px]", expanded && "rotate-90")}>
          ▶
        </span>
        Credit Breakdown
      </button>
      {expanded && (
        <div className="space-y-3 pl-4">
          {credit.by_head.length > 0 && (
            <div className="space-y-1">
              <p className="text-[11px] uppercase tracking-wide text-muted-foreground">By head</p>
              {credit.by_head.map((head) => (
                <div key={head.head_name} className="flex justify-between text-xs">
                  <span className="font-medium">
                    {head.head_name}
                    {head.volunteer_id && (
                      <span className="ml-2 font-normal text-muted-foreground font-mono">
                        {head.volunteer_id.slice(0, 8)}
                      </span>
                    )}
                  </span>
                  <span className={cn("text-muted-foreground", !head.available && "italic")}>
                    {head.available ? formatCredit(head.total_credit) : "unreachable"}
                  </span>
                </div>
              ))}
            </div>
          )}
          {credit.by_leaf.length > 0 && (
            <div className="space-y-1">
              <p className="text-[11px] uppercase tracking-wide text-muted-foreground">By leaf</p>
              {credit.by_leaf.map((leaf) => (
                <div
                  key={leaf.leaf_id}
                  className="flex justify-between text-xs text-muted-foreground"
                >
                  <span>{leaf.leaf_name}</span>
                  <span>{formatCredit(leaf.credit)}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/** Leaves whose work units keep failing here, from `status.failing_leafs`. */
function FailingLeafsSection({ failing }: { failing: FailingLeaf[] }) {
  if (failing.length === 0) return null;
  return (
    <section className="space-y-2" aria-label="Failing on this machine">
      <h2 className="text-sm font-medium text-muted-foreground">Failing on this machine</h2>
      <ul className="space-y-2">
        {failing.map((f) => (
          <li
            key={f.leaf_id}
            className="rounded-md border border-red-500/30 bg-red-500/5 px-3 py-2 text-sm space-y-1"
          >
            <div className="flex items-center justify-between gap-3">
              <span className="font-medium">{f.leaf_name}</span>
              <span className="text-xs text-muted-foreground">
                {f.consecutive_failures} in a row
                {f.total_failures > f.consecutive_failures && ` · ${f.total_failures} total`}
              </span>
            </div>
            {f.last_reason && (
              <p className="text-xs text-muted-foreground break-words">{f.last_reason}</p>
            )}
            {f.paused && (
              <p className="text-xs text-red-600">
                Not requesting more of this leaf
                {f.paused_until ? ` until ${formatDateTime(f.paused_until)}` : " for now"}.
              </p>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}

/** Heads that refuse this client version until the app is updated. */
function UpdateRequiredBanner({ heads }: { heads: HeadInfo[] }) {
  const stale = heads.filter((h) => h.update_required);
  if (stale.length === 0) return null;
  return (
    <div
      role="alert"
      className="rounded-md border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-700 dark:text-red-300 space-y-1"
    >
      {stale.map((h) => (
        <p key={h.name} className="font-medium">
          This app is too old for {h.name} — update Lettuce Compute
        </p>
      ))}
      <p className="text-xs opacity-80">
        The head will not send work to this version. Install the update from the banner at
        the top of the window, or from the download page, then restart Lettuce.
      </p>
    </div>
  );
}

function gpuSummary(machine: MachineCapabilities): string {
  const vendor = machine.gpu_vendors[0] ?? "GPU";
  const card = machine.gpu_card_vram_mb > 0 ? `${formatGb(machine.gpu_card_vram_mb)} card` : "card size unknown";
  const allowed =
    machine.max_gpu_vram_mb > 0
      ? `${formatGb(machine.max_gpu_vram_mb)} allowed for Lettuce`
      : "GPU work disabled (allowance 0%)";
  return `GPU: ${vendor}, ${card}, ${allowed}`;
}

type ViewMode = "cards" | "table";
const VIEW_MODE_KEY = "lettuce-task-view-mode";

export function OverviewPage() {
  const { status } = useDaemonStatus(3000);
  const { metrics } = useMetrics(3000);
  const { system } = useSystemMetrics(3000);
  const { credit } = useCredit(10000);
  const { client } = useClient();
  const { status: containerStatus } = useContainerRuntime();
  const { data: headsResp } = useApiQuery(
    (c: ManagementClient) => c.headsAndMachine(),
    30000
  );
  const heads = headsResp?.heads ?? [];
  const machine = headsResp?.machine ?? null;

  // The daemon reports its version in /status on current builds; older builds
  // do not, so fall back to asking the bundled CLI once.
  const [cliVersion, setCliVersion] = useState<string | null>(null);
  useEffect(() => {
    if (status?.client_version || cliVersion) return;
    let cancelled = false;
    getClientVersion()
      .then((v) => {
        if (!cancelled && typeof v === "string" && v) setCliVersion(v);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [status?.client_version, cliVersion]);
  const clientVersion = status?.client_version ?? cliVersion;

  const [detailTaskId, setDetailTaskId] = useState<string | null>(null);
  const [filters, setFilters] = useState<TaskFilterState>({
    leafName: null,
    status: "all",
    showQueued: false,
  });

  const [viewMode, setViewMode] = useState<ViewMode>(() => {
    const stored = localStorage.getItem(VIEW_MODE_KEY);
    return stored === "table" ? "table" : "cards";
  });

  const handleViewModeChange = (mode: ViewMode) => {
    setViewMode(mode);
    localStorage.setItem(VIEW_MODE_KEY, mode);
  };

  const state = status?.state ?? "stopped";
  const tasks = status?.active_tasks ?? [];
  const queuedTasks = status?.queued_tasks ?? [];
  const isPaused = state === "paused";
  const pausedReason = status?.paused_reason ?? null;
  // The daemon's resume undoes a user pause only; offered for a schedule
  // pause it was refused with 409 "not paused" and the refusal was swallowed,
  // so the button did nothing (TB-72). Any other pause names its remedy.
  const canResume = isPaused && pauseIsResumable(pausedReason);

  // A refused pause or resume, with the daemon's own reason, for a few seconds.
  const [toast, setToast] = useState<string | null>(null);
  useEffect(() => {
    if (!toast) return;
    const id = setTimeout(() => setToast(null), 4000);
    return () => clearTimeout(id);
  }, [toast]);

  const taskActions: TaskActions = useMemo(() => ({
    onSuspend: (id) => client?.suspendTask(id),
    onResume: (id) => client?.resumeTask(id),
    onAbort: (id) => client?.abortTask(id),
    onShowDetails: (task) => setDetailTaskId(task.work_unit_id),
    onCopyId: (id) => navigator.clipboard.writeText(id),
  }), [client]);

  const leafCount = useMemo(() => {
    if (!credit) return 0;
    return credit.by_leaf.length;
  }, [credit]);

  const filteredTasks = useMemo(
    () => applyTaskFilters(tasks, queuedTasks, filters),
    [tasks, queuedTasks, filters],
  );

  // Viz task selection — volunteers can click task cards to switch which viz is displayed
  const [selectedVizTaskId, setSelectedVizTaskId] = useState<string | null>(null);

  const vizTask = useMemo(() => {
    if (selectedVizTaskId) {
      const selected = tasks.find(t => t.work_unit_id === selectedVizTaskId && t.viz_bundle_path && t.work_dir);
      if (selected) return selected;
    }
    return tasks.find(t => t.viz_bundle_path && t.work_dir) ?? null;
  }, [tasks, selectedVizTaskId]);

  // Auto-clear selection when the selected task is no longer active
  useEffect(() => {
    if (selectedVizTaskId && !tasks.some(t => t.work_unit_id === selectedVizTaskId)) {
      setSelectedVizTaskId(null);
    }
  }, [tasks, selectedVizTaskId]);

  const handleTaskClick = useCallback((task: ActiveTaskInfo) => {
    setDetailTaskId(task.work_unit_id);
    if (!isQueuedTask(task) && task.viz_bundle_path) {
      setSelectedVizTaskId(task.work_unit_id);
    }
  }, []);

  const vizWorkDir = vizTask?.work_dir ?? null;
  const vizBundlePath = vizTask?.viz_bundle_path ?? null;
  const vizLeafSlug = vizTask?.leaf_name ?? "";
  const vizTaskId = vizTask?.work_unit_id ?? null;

  // The frame's verdict on the shown unit's bundle (TB-69): once it reports
  // the page has no live view, or never started, the 320 px panel gives way
  // to a one-line note. Keyed by unit so switching units starts afresh.
  const [vizUnavailable, setVizUnavailable] = useState<{ workUnitId: string; reason: VizUnavailableReason } | null>(null);
  const vizUnavailableReason =
    vizTaskId && vizUnavailable?.workUnitId === vizTaskId ? vizUnavailable.reason : null;
  const handleVizUnavailable = useCallback((reason: VizUnavailableReason) => {
    if (vizTaskId) setVizUnavailable({ workUnitId: vizTaskId, reason });
  }, [vizTaskId]);

  const handlePauseResume = async () => {
    if (!client) return;
    try {
      if (isPaused) {
        await client.resume();
      } else {
        await client.pause();
      }
    } catch (err) {
      const reason = err instanceof Error ? err.message : String(err);
      setToast(`Could not ${isPaused ? "resume" : "pause"}: ${reason}`);
    }
  };

  return (
    <div className="p-6 space-y-6 max-w-3xl mx-auto">
      <UpdateRequiredBanner heads={heads} />

      {toast && (
        <div
          role="alert"
          className="fixed top-4 right-4 z-50 rounded-md px-4 py-2 text-sm font-medium shadow-lg bg-destructive text-destructive-foreground"
        >
          {toast}
        </div>
      )}

      {/* Status section */}
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <StatusBadge state={state} pausedReason={status?.paused_reason ?? null} />
          {status && (
            <span className="text-sm text-muted-foreground">
              Running for {formatDuration(status.uptime_seconds)}
            </span>
          )}
        </div>
        {state === "active" && (
          <Button variant="outline" size="sm" onClick={handlePauseResume}>
            Pause
          </Button>
        )}
        {canResume && (
          <Button variant="default" size="sm" onClick={handlePauseResume}>
            Resume
          </Button>
        )}
        {isPaused && !canResume && pausedReason === "scheduled" && (
          <Button variant="outline" size="sm" onClick={() => emit("navigate:settings")}>
            Change schedule
          </Button>
        )}
      </div>

      {/* Visualization panel */}
      <div className="space-y-2">
        {vizBundlePath && vizWorkDir && vizUnavailableReason ? (
          <div
            data-testid="viz-unavailable-note"
            className="flex items-center justify-center rounded-lg border bg-muted/20 px-4 text-center text-sm text-muted-foreground"
            style={{ height: 80 }}
          >
            {describeVizUnavailable(vizUnavailableReason, "live", vizLeafSlug)}
          </div>
        ) : vizBundlePath && vizWorkDir ? (
          <div className="rounded-lg overflow-hidden border" style={{ height: 320 }}>
            <VizFrame
              key={vizTaskId ?? undefined}
              vizBundlePath={vizBundlePath}
              workDir={vizWorkDir}
              leafSlug={vizLeafSlug}
              paused={isPaused}
              onUnavailable={handleVizUnavailable}
            />
          </div>
        ) : tasks.length > 0 ? (
          <div className="flex items-center justify-center rounded-lg border bg-muted/20 text-sm text-muted-foreground" style={{ height: 80 }}>
            Computing in progress...
          </div>
        ) : (
          <div className="flex items-center justify-center rounded-lg border bg-[#0a0a0f] text-sm text-muted-foreground" style={{ height: 80 }}>
            Start computing to see a simulation
          </div>
        )}
      </div>

      <NoticesPanel />

      <FailingLeafsSection failing={status?.failing_leafs ?? []} />

      {/* Active tasks */}
      <div className="space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-medium text-muted-foreground">Active Tasks</h2>
          {tasks.length > 0 && (
            <div className="flex items-center gap-1">
              <button
                onClick={() => handleViewModeChange("cards")}
                className={cn("p-1.5 rounded", viewMode === "cards" ? "bg-muted" : "hover:bg-muted/50")}
                title="Card view"
              >
                <svg className="h-4 w-4 text-muted-foreground" viewBox="0 0 16 16" fill="currentColor">
                  <rect x="1" y="1" width="6" height="6" rx="1" />
                  <rect x="9" y="1" width="6" height="6" rx="1" />
                  <rect x="1" y="9" width="6" height="6" rx="1" />
                  <rect x="9" y="9" width="6" height="6" rx="1" />
                </svg>
              </button>
              <button
                onClick={() => handleViewModeChange("table")}
                className={cn("p-1.5 rounded", viewMode === "table" ? "bg-muted" : "hover:bg-muted/50")}
                title="Table view"
              >
                <svg className="h-4 w-4 text-muted-foreground" viewBox="0 0 16 16" fill="currentColor">
                  <rect x="1" y="2" width="14" height="2" rx="0.5" />
                  <rect x="1" y="7" width="14" height="2" rx="0.5" />
                  <rect x="1" y="12" width="14" height="2" rx="0.5" />
                </svg>
              </button>
            </div>
          )}
        </div>
        <TaskFilters
          tasks={tasks}
          filters={filters}
          onFiltersChange={setFilters}
        />
        {tasks.length > 0 ? (
          viewMode === "table" ? (
            <ActiveTaskTable
              tasks={filteredTasks}
              actions={taskActions}
              onRowClick={handleTaskClick}
            />
          ) : (
            filteredTasks.map((task) => (
              <div
                key={task.work_unit_id}
                className="cursor-pointer"
                onClick={() => handleTaskClick(task)}
              >
                {isQueuedTask(task) ? (
                  <QueuedTaskCard task={{ work_unit_id: task.work_unit_id, leaf_name: task.leaf_name, deadline_seconds: task.deadline_seconds, fetched_at: new Date().toISOString(), server_name: task.head_name }} />
                ) : (
                  <ActiveTaskCard task={task} actions={taskActions} isVizActive={vizTask?.work_unit_id === task.work_unit_id} />
                )}
              </div>
            ))
          )
        ) : isPaused ? (
          <p className="text-sm text-muted-foreground">{pausedExplanation(pausedReason)}</p>
        ) : (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <PulsingDot />
            <span>No active tasks. Waiting for work...</span>
          </div>
        )}
      </div>

      {/* Queued tasks */}
      {queuedTasks.length > 0 && (
        <div className="space-y-2">
          <h2 className="text-sm font-medium text-muted-foreground">
            Queued ({queuedTasks.length})
          </h2>
          {queuedTasks.map((qt) => (
            <QueuedTaskCard key={qt.work_unit_id} task={qt} />
          ))}
        </div>
      )}

      {/* Machine gauges: CPU and memory are measured by the app itself (the
          daemon reports zeros for them); disk is the daemon's own footprint
          against its allowance. */}
      {(system || metrics) && (
        <div className="space-y-3">
          <h2 className="text-sm font-medium text-muted-foreground">Resources</h2>
          <div className="grid grid-cols-2 gap-4 justify-items-center sm:grid-cols-3">
            {system && (
              <ResourceGauge
                label="CPU"
                value={system.cpu_usage_pct}
                displayValue={`${Math.round(system.cpu_usage_pct)}%`}
                temperature={metrics?.cpu_temp_c}
              />
            )}
            {system && (
              <ResourceGauge
                label="Memory"
                value={
                  system.memory_total_mb > 0
                    ? (system.memory_used_mb / system.memory_total_mb) * 100
                    : 0
                }
                displayValue={`${formatBytes(system.memory_used_mb)} / ${formatBytes(system.memory_total_mb)}`}
              />
            )}
            {metrics && metrics.disk_usage_known && (
              <ResourceGauge
                label="Disk"
                value={
                  metrics.disk_allowance_mb > 0
                    ? (metrics.disk_used_mb / metrics.disk_allowance_mb) * 100
                    : 0
                }
                displayValue={`${(metrics.disk_used_mb / 1024).toFixed(1)} / ${(metrics.disk_allowance_mb / 1024).toFixed(1)} GB`}
              />
            )}
          </div>
          {metrics && metrics.disk_usage_known && (
            <p className="text-xs text-muted-foreground text-center">
              Lettuce is using {formatGb(metrics.disk_used_mb)} of your{" "}
              {formatGb(metrics.disk_allowance_mb)} allowance
            </p>
          )}
          {machine?.has_gpu && (
            <p className="text-xs text-muted-foreground text-center">{gpuSummary(machine)}</p>
          )}
        </div>
      )}

      {/* Container runtime status */}
      {containerStatus && (
        <div className="flex items-center gap-2 text-sm">
          <span
            className={cn(
              "inline-block h-2 w-2 rounded-full",
              containerStatus.status === "running"
                ? "bg-green-500"
                : containerStatus.status === "not_installed"
                  ? "bg-gray-400"
                  : "bg-yellow-500"
            )}
          />
          <span className="text-muted-foreground">
            {containerStatus.status === "running"
              ? "Containers: Ready"
              : containerStatus.status === "not_installed"
                ? "Containers: Not installed"
                : "Containers: Unavailable"}
          </span>
          {containerStatus.status !== "running" && (
            <button
              onClick={() => emit("navigate:settings")}
              className="text-xs text-primary hover:underline"
            >
              {containerStatus.status === "not_installed" ? "Setup" : "Start"}
            </button>
          )}
        </div>
      )}

      {/* Credit summary */}
      {credit && (
        <div className="space-y-3">
          <h2 className="text-sm font-medium text-muted-foreground">Credit</h2>
          <CreditDisplay
            today={credit.today}
            thisWeek={credit.this_week}
            thisMonth={credit.this_month}
            total={credit.total_credit}
            leafCount={leafCount}
            dayBoundary={credit.day_boundary}
          />
          {credit.source === "local" && (
            <p className="text-xs text-yellow-600">
              Figures from this machine's local history — no head could be reached.
            </p>
          )}
          <p className="text-xs text-muted-foreground">
            Credit moves only when a head validates your results, so these counters can
            sit still for a while on leaves that check each result against other
            volunteers' copies.
          </p>
          <CreditBreakdownSection credit={credit} />
        </div>
      )}

      {/* Quick stats footer */}
      {status && credit && (
        <p className="text-xs text-muted-foreground text-center border-t pt-4">
          Active tasks: {tasks.length} · Queued: {status.queued_tasks?.length ?? 0} ·
          Connected servers: {status.connected_servers} ·
          Leafs: {leafCount}
          {clientVersion && ` · Client v${clientVersion.replace(/^v/, "")}`}
        </p>
      )}

      {/* Task detail panel */}
      <TaskDetailPanel
        workUnitId={detailTaskId}
        open={!!detailTaskId}
        onOpenChange={(open) => { if (!open) setDetailTaskId(null); }}
        onSuspend={(id) => client?.suspendTask(id)}
        onResume={(id) => client?.resumeTask(id)}
        onAbort={(id) => client?.abortTask(id)}
      />
    </div>
  );
}
