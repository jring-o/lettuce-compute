import { emit } from "@tauri-apps/api/event";
import { openUrl } from "@tauri-apps/plugin-opener";
import { Slider } from "@/components/ui/slider";
import { cn } from "@/lib/utils";
import type { LeafInfo, ContainerRuntimeStatus } from "@/api/client";

interface LeafCardProps {
  leaf: LeafInfo;
  showWeightSlider: boolean;
  containerStatus: ContainerRuntimeStatus | null;
  onToggle: (enabled: boolean) => void;
  onWeightChange: (weight: number) => void;
  dashboardUrl?: string;
  volunteerId?: string;
}

export function LeafCard({
  leaf,
  showWeightSlider,
  containerStatus,
  onToggle,
  onWeightChange,
  dashboardUrl,
  volunteerId,
}: LeafCardProps) {
  const spec = leaf.execution_spec;
  const requiresContainer = !!spec?.image;
  const hasWasmBinary = !!spec?.binaries?.wasm;
  const hasNativeBinaries = !!spec?.binaries && Object.keys(spec.binaries).some(
    (k) => k !== "wasm" && k !== "wgsl"
  );
  const requiresGpu = !!spec?.gpu_required;
  const containerUnavailable = requiresContainer && containerStatus?.status !== "running";

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
            <span className="inline-flex items-center rounded-full bg-secondary px-2 py-0.5 text-[10px] font-medium">
              {leaf.research_area}
            </span>
            <span className="inline-flex items-center rounded-full border px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wider text-muted-foreground">
              {leaf.task_pattern}
            </span>
            {requiresContainer && (
              <span className="inline-flex items-center rounded-full border border-blue-200 bg-blue-100 px-2 py-0.5 text-[10px] font-medium text-blue-700">
                Container
              </span>
            )}
            {hasNativeBinaries && (
              <span className="inline-flex items-center rounded-full border border-green-200 bg-green-100 px-2 py-0.5 text-[10px] font-medium text-green-700">
                Native
              </span>
            )}
            {hasWasmBinary && (
              <span className="inline-flex items-center rounded-full border border-purple-200 bg-purple-100 px-2 py-0.5 text-[10px] font-medium text-purple-700">
                WASM
              </span>
            )}
            {requiresGpu && (
              <span className="inline-flex items-center rounded-full border border-orange-200 bg-orange-100 px-2 py-0.5 text-[10px] font-medium text-orange-700">
                {spec?.gpu_type || "GPU"}
              </span>
            )}
            {!leaf.enabled && (
              <span className="text-xs text-muted-foreground">(disabled)</span>
            )}
          </div>
          <p className="text-xs text-muted-foreground mt-0.5">
            {leaf.queued_work_units} queued &middot; {leaf.active_volunteers} volunteers
          </p>
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
              onClick={() => openUrl(`${dashboardUrl}/leafs/${leaf.slug}/visualize?volunteer=${volunteerId}`)}
              className="text-xs text-blue-500 hover:underline mt-0.5"
            >
              View My Results
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
