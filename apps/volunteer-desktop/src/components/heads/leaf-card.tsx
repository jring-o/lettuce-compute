import { useState } from "react";
import { emit } from "@tauri-apps/api/event";
import { openUrl } from "@tauri-apps/plugin-opener";
import { Slider } from "@/components/ui/slider";
import { cn, formatBytes } from "@/lib/utils";
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
  // stop is offered only when the Memory slider can reach it.
  const memoryRaiseTo = requirements.find((r) => r.key === "memory")?.raiseToMb ?? 0;
  const memoryRaiseFits = memoryCeilingMb == null || memoryRaiseTo <= memoryCeilingMb;

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
                    : `Raise memory allowance to ${formatBytes(memoryRaiseTo)}`}
                </button>
              )}
              {!memoryRaiseFits && memoryCeilingMb != null && (
                <p className="text-xs text-amber-700 dark:text-amber-400">
                  Needs more memory than this machine can allow ({formatBytes(memoryCeilingMb)} at
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
                  {raising ? "Raising..." : `Raise disk allowance to ${raiseTo} GB`}
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
        <div className="pl-7 space-y-1">
          <div className="flex justify-between text-xs text-muted-foreground">
            <span>Weight</span>
            <span>{leaf.effective_weight}</span>
          </div>
          <Slider
            min={1}
            max={100}
            value={leaf.effective_weight}
            onChange={onWeightChange}
          />
        </div>
      )}
    </div>
  );
}
