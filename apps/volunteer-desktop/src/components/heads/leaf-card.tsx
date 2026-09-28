import { useState } from "react";
import { emit } from "@tauri-apps/api/event";
import { openUrl } from "@tauri-apps/plugin-opener";
import { cn, formatExactMb } from "@/lib/utils";
import { WeightSlider } from "./weight-slider";
import type { LeafInfo, ContainerRuntimeStatus, MachineCapabilities } from "@/api/client";
import {
  leafRuntimes,
  leafRequirementItems,
  runtimeTrusted,
  type LeafRuntime,
} from "./leaf-requirements";

interface LeafCardProps {
  leaf: LeafInfo;
  showWeightSlider: boolean;
  containerStatus: ContainerRuntimeStatus | null;
  /** This machine's capabilities from the running daemon; null until loaded. */
  machine: MachineCapabilities | null;
  /** The head's `trusted_runtimes` (uppercase); null when not known. */
  trustedRuntimes: string[] | null;
  onToggle: (enabled: boolean) => void;
  onWeightChange: (weight: number) => void;
  /**
   * Save the cores each of the leaf's tasks is given, or null for the
   * leaf's own range. Rejects on failure. Absent: no override control.
   */
  onCoresChange?: (cores: number | null) => Promise<void>;
  /** Save the most of the leaf's tasks that run at once, or null for no cap. */
  onMaxRunningChange?: (count: number | null) => Promise<void>;
  /** Raise `resource_limits.max_disk_gb` to the given whole-GB value. */
  onRaiseDisk?: (gb: number) => Promise<void>;
  /** Raise `resource_limits.max_memory_mb` to the given Memory-slider stop, in MB. */
  onRaiseMemory?: (mb: number) => Promise<void>;
  /**
   * The most the Memory slider can be set to (90 % of this machine's RAM),
   * in MB; null while unknown. A stop above it is not offered.
   */
  memoryCeilingMb?: number | null;
  dashboardUrl?: string;
  volunteerId?: string;
}

const runtimeBadge: Record<LeafRuntime, { label: string; className: string }> = {
  container: {
    label: "Container",
    className: "border-blue-200 bg-blue-100 text-blue-700",
  },
  native: {
    label: "Native",
    className: "border-green-200 bg-green-100 text-green-700",
  },
  wasm: {
    label: "WASM",
    className: "border-purple-200 bg-purple-100 text-purple-700",
  },
};

function formatClock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export function LeafCard({
  leaf,
  showWeightSlider,
  containerStatus,
  machine,
  trustedRuntimes,
  onToggle,
  onWeightChange,
  onCoresChange,
  onMaxRunningChange,
  onRaiseDisk,
  onRaiseMemory,
  memoryCeilingMb = null,
  dashboardUrl,
  volunteerId,
}: LeafCardProps) {
  const [raising, setRaising] = useState(false);
  const [raiseError, setRaiseError] = useState<string | null>(null);
  const [raisingMemory, setRaisingMemory] = useState(false);
  const [raiseMemoryError, setRaiseMemoryError] = useState<string | null>(null);

  const spec = leaf.execution_spec;
  const runtimes = leafRuntimes(leaf);
  const untrusted = runtimes.filter((r) => !runtimeTrusted(r, trustedRuntimes));
  const requiresContainer = runtimes.includes("container");
  const requiresGpu = !!spec?.gpu_required || !!leaf.resource_requirements?.gpu_required;
  const gpuLabel = leaf.resource_requirements?.gpu_type || spec?.gpu_type || "GPU";
  const containerUnavailable = requiresContainer && containerStatus?.status !== "running";
  const requirements = leafRequirementItems(leaf, machine);
  const failures = leaf.failures;
  const diskGate = leaf.disk_gate;
  const raiseTo = diskGate?.raise_to_gb ?? 0;
  const researchArea = leaf.research_area.join(", ");
  // The memory shortfall names the slider stop that clears it (TB-66); the
  // stop is offered only when the Memory slider can reach it. When the
  // container engine's virtual machine is the bound (TB-63) there is no stop
  // to offer — the machine has to be enlarged — and the card says so.
  const memoryItem = requirements.find((r) => r.key === "memory");
  const memoryRaiseTo = memoryItem?.raiseToMb ?? 0;
  const memoryRaiseFits = memoryCeilingMb == null || memoryRaiseTo <= memoryCeilingMb;
  const memoryVMLimited = !!memoryItem?.vmLimited;

  const handleRaiseDisk = async (gb: number) => {
    if (!onRaiseDisk) return;
    setRaising(true);
    setRaiseError(null);
    try {
      await onRaiseDisk(gb);
    } catch (err) {
      setRaiseError(err instanceof Error ? err.message : "Could not raise the disk allowance");
    } finally {
      setRaising(false);
    }
  };

  const handleRaiseMemory = async (mb: number) => {
    if (!onRaiseMemory) return;
    setRaisingMemory(true);
    setRaiseMemoryError(null);
    try {
      await onRaiseMemory(mb);
    } catch (err) {
      setRaiseMemoryError(err instanceof Error ? err.message : "Could not raise the memory allowance");
    } finally {
      setRaisingMemory(false);
    }
  };

  return (
    <div
      className={cn(
        "rounded-md border p-3 space-y-2 transition-opacity",
        !leaf.enabled && "opacity-50",
        containerUnavailable && leaf.enabled && "opacity-60"
      )}
    >
      <div className="flex items-start gap-3">
        <input
          type="checkbox"
          checked={leaf.enabled}
          onChange={() => onToggle(!leaf.enabled)}
          className="mt-1 h-4 w-4 rounded border-input accent-primary cursor-pointer"
        />
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="font-medium text-sm">{leaf.name}</span>
            {researchArea && (
              <span className="inline-flex items-center rounded-full bg-secondary px-2 py-0.5 text-[10px] font-medium">
                {researchArea}
              </span>
            )}
            <span className="inline-flex items-center rounded-full border px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wider text-muted-foreground">
              {leaf.task_pattern}
            </span>
            {runtimes.map((rt) => {
              const trusted = runtimeTrusted(rt, trustedRuntimes);
              const badge = runtimeBadge[rt];
              return (
                <span
                  key={rt}
                  data-testid={`runtime-badge-${rt}`}
                  data-trusted={trusted ? "true" : "false"}
                  title={
                    trusted ? undefined : `${badge.label} is not allowed by your trust settings for this head`
                  }
                  className={cn(
                    "inline-flex items-center rounded-full border px-2 py-0.5 text-[10px] font-medium",
                    trusted
                      ? badge.className
                      : "border-muted bg-muted text-muted-foreground line-through"
                  )}
                >
                  {badge.label}
                </span>
              );
            })}
            {requiresGpu && (
              <span className="inline-flex items-center rounded-full border border-orange-200 bg-orange-100 px-2 py-0.5 text-[10px] font-medium text-orange-700">
                {gpuLabel}
              </span>
            )}
            {!leaf.enabled && (
              <span className="text-xs text-muted-foreground">(disabled)</span>
            )}
          </div>

          <p className="text-xs text-muted-foreground mt-0.5">
            {leaf.queued_work_units} queued &middot; {leaf.active_volunteers} volunteers &middot;{" "}
            {leaf.active_hosts} hosts
          </p>

          {untrusted.length > 0 && (
            <p className="text-xs text-muted-foreground mt-0.5">
              {untrusted.map((r) => runtimeBadge[r].label).join(" and ")}{" "}
              {untrusted.length === 1 ? "is" : "are"} not allowed by your trust settings for this
              head.
            </p>
          )}

          {requirements.length > 0 && (
            <p className="text-xs text-muted-foreground mt-0.5">
              Needs:{" "}
              {requirements.map((item, i) => (
                <span key={item.key}>
                  {i > 0 && " · "}
                  <span
                    data-testid={`requirement-${item.key}`}
                    data-short={item.shortfall ? "true" : "false"}
                    className={cn(
                      item.shortfall && "font-medium text-amber-700 dark:text-amber-400"
                    )}
                  >
                    {item.label}
                    {item.shortfall && ` (${item.shortfall})`}
                  </span>
                </span>
              ))}
            </p>
          )}

          {memoryVMLimited && (
            <p className="text-xs text-amber-700 dark:text-amber-400 mt-1">
              Needs more memory than the container engine's virtual machine has. Enlarge the
              machine's memory (Podman Desktop or Docker Desktop: Settings → Resources; or{" "}
              <code>podman machine set --memory</code>), then restart Lettuce — raising the memory
              allowance alone changes nothing.
            </p>
          )}

          {memoryRaiseTo > 0 && (
            <div className="mt-1 space-y-1">
              {memoryRaiseFits && onRaiseMemory && (
                <button
                  onClick={() => handleRaiseMemory(memoryRaiseTo)}
                  disabled={raisingMemory}
                  className="text-xs text-blue-600 hover:underline disabled:opacity-50"
                >
                  {raisingMemory
                    ? "Raising..."
                    : `Raise memory allowance to ${formatExactMb(memoryRaiseTo)}`}
                </button>
              )}
              {!memoryRaiseFits && memoryCeilingMb != null && (
                <p className="text-xs text-amber-700 dark:text-amber-400">
                  Needs more memory than this machine can allow ({formatExactMb(memoryCeilingMb)} at
                  most).
                </p>
              )}
              {raiseMemoryError && <p className="text-xs text-destructive">{raiseMemoryError}</p>}
            </div>
          )}

          {diskGate?.blocked && (
            <div className="mt-1 space-y-1">
              <p className="text-xs text-amber-700 dark:text-amber-400">
                Will not fetch: {diskGate.reason || "blocked by the disk allowance"}
              </p>
              {raiseTo > 0 && onRaiseDisk && (
                <button
                  onClick={() => handleRaiseDisk(raiseTo)}
                  disabled={raising}
                  className="text-xs text-blue-600 hover:underline disabled:opacity-50"
                >
                  {raising ? "Raising..." : `Raise disk allowance to ${raiseTo} GiB`}
                </button>
              )}
              {raiseError && <p className="text-xs text-destructive">{raiseError}</p>}
            </div>
          )}

          {failures && failures.total_failures > 0 && (
            <p className="text-xs text-red-600 dark:text-red-400 mt-0.5">
              Failing on this machine: {failures.consecutive_failures} in a row,{" "}
              {failures.total_failures} total
              {failures.last_reason && ` (last: ${failures.last_reason})`}
              {failures.paused &&
                failures.paused_until &&
                `. Paused here until ${formatClock(failures.paused_until)}`}
              {failures.paused && !failures.paused_until && ". Paused here for now"}
            </p>
          )}

          {containerUnavailable && (
            <button
              onClick={() => emit("navigate:settings")}
              className="text-xs text-yellow-600 hover:underline mt-0.5"
            >
              Container runtime required
            </button>
          )}

          {dashboardUrl && volunteerId && (
            <button
              onClick={() =>
                openUrl(`${dashboardUrl}/leafs/${leaf.slug}/visualize?volunteer=${volunteerId}`)
              }
              title="Opens in your browser. Only works when the head has made this leaf's visualization public."
              className="text-xs text-muted-foreground hover:underline mt-0.5"
            >
              View results on the head's website
            </button>
          )}
        </div>
      </div>
      {showWeightSlider && leaf.enabled && (
        <div className="pl-7">
          <WeightSlider
            label="Weight"
            value={leaf.effective_weight}
            caption="This leaf's share of the head's compute time on this machine, against the other enabled leafs' weights."
            onChange={onWeightChange}
          />
        </div>
      )}
      {leaf.enabled && leaf.cpu && onCoresChange && onMaxRunningChange && (
        <div className="pl-7">
          <LeafCPUControl
            leaf={leaf}
            cpuLimit={machine?.host_max_cpu_cores ?? 0}
            onCoresChange={onCoresChange}
            onMaxRunningChange={onMaxRunningChange}
          />
        </div>
      )}
    </div>
  );
}

/** "3 cores", "2–4 cores". */
export function coreRangeText(min: number, max: number): string {
  if (max > min) return `${min}–${max} cores`;
  return `${min} core${min === 1 ? "" : "s"}`;
}

/**
 * The sentence under a leaf's CPU control: what each of its tasks is given
 * here, how many run at once, and when a change applies.
 */
export function leafCPUCaption(cpu: NonNullable<LeafInfo["cpu"]>): string {
  const given =
    cpu.task_cores_max > cpu.task_cores_min
      ? `Each task is given ${coreRangeText(cpu.task_cores_min, cpu.task_cores_max)}, as many as are free when it starts`
      : `Each task is given ${coreRangeText(cpu.task_cores_min, cpu.task_cores_max)}`;
  return `${given}; up to ${cpu.runs_at_once} run at once here, and Lettuce buffers no more of this leaf than that many can finish. Applies to tasks started afterwards — restart a running task from Overview to apply it now.`;
}

/**
 * The volunteer's CPU override for one leaf, beside its weight: the cores
 * each task is given (within the range the leaf declares) and the most of
 * its tasks that run at once. Saved at once; a failed save is shown.
 */
function LeafCPUControl({
  leaf,
  cpuLimit,
  onCoresChange,
  onMaxRunningChange,
}: {
  leaf: LeafInfo;
  cpuLimit: number;
  onCoresChange: (cores: number | null) => Promise<void>;
  onMaxRunningChange: (count: number | null) => Promise<void>;
}) {
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const cpu = leaf.cpu!;
  const declaredMin = Math.max(1, leaf.resource_requirements?.min_cpu_cores ?? 1);
  const declaredMax = Math.max(declaredMin, leaf.resource_requirements?.max_cpu_cores ?? declaredMin);
  const coreChoices: number[] = [];
  for (let n = declaredMin; n <= declaredMax; n++) coreChoices.push(n);
  if (cpu.cores_override > 0 && !coreChoices.includes(cpu.cores_override)) coreChoices.push(cpu.cores_override);
  const perTask = Math.max(1, cpu.task_cores_min);
  const runLimit = Math.max(1, cpuLimit > 0 ? Math.floor(cpuLimit / perTask) : cpu.runs_at_once, cpu.max_running_override);
  const runChoices: number[] = [];
  for (let n = 1; n <= runLimit; n++) runChoices.push(n);

  const save = async (fn: () => Promise<void>) => {
    setSaving(true);
    setError(null);
    try {
      await fn();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save the setting");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="space-y-1" data-testid="leaf-cpu-control">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {coreChoices.length > 1 && (
          <label className="flex items-center gap-1">
            Cores per task
            <select
              aria-label="Cores per task"
              className="rounded border bg-background px-1 py-0.5 text-foreground"
              value={cpu.cores_override > 0 ? String(cpu.cores_override) : ""}
              disabled={saving}
              onChange={(e) => save(() => onCoresChange(e.target.value ? Number(e.target.value) : null))}
            >
              <option value="">Leaf's range ({coreRangeText(declaredMin, declaredMax)})</option>
              {coreChoices.map((n) => (
                <option key={n} value={String(n)}>
                  {n}
                </option>
              ))}
            </select>
          </label>
        )}
        <label className="flex items-center gap-1">
          Run at most
          <select
            aria-label="Run at most"
            className="rounded border bg-background px-1 py-0.5 text-foreground"
            value={cpu.max_running_override > 0 ? String(cpu.max_running_override) : ""}
            disabled={saving}
            onChange={(e) => save(() => onMaxRunningChange(e.target.value ? Number(e.target.value) : null))}
          >
            <option value="">No limit</option>
            {runChoices.map((n) => (
              <option key={n} value={String(n)}>
                {n} at once
              </option>
            ))}
          </select>
        </label>
      </div>
      <p className="text-xs text-muted-foreground" data-testid="leaf-cpu-caption">
        {leafCPUCaption(cpu)}
      </p>
      {error && <p className="text-xs text-destructive">{error}</p>}
    </div>
  );
}
