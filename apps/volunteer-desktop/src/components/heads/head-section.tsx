import { useState } from "react";
import { Slider } from "@/components/ui/slider";
import { Button } from "@/components/ui/button";
import { LeafCard } from "./leaf-card";
import { cn } from "@/lib/utils";
import type { HeadInfo, ContainerRuntimeStatus } from "@/api/client";

interface HeadSectionProps {
  head: HeadInfo;
  showHeadWeight: boolean;
  containerStatus: ContainerRuntimeStatus | null;
  onHeadWeightChange: (weight: number) => void;
  onLeafToggle: (leafSlug: string, enabled: boolean) => void;
  onLeafWeightChange: (leafSlug: string, weight: number) => void;
  onResetDefaults: () => void;
  onDetach: () => void;
}

export function HeadSection({
  head,
  showHeadWeight,
  containerStatus,
  onHeadWeightChange,
  onLeafToggle,
  onLeafWeightChange,
  onResetDefaults,
  onDetach,
}: HeadSectionProps) {
  const [expanded, setExpanded] = useState(true);
  const [confirmDetach, setConfirmDetach] = useState(false);

  const enabledLeafCount = head.leafs.filter((l) => l.enabled).length;
  const showLeafWeights = enabledLeafCount >= 2;

  return (
    <div className="rounded-lg border">
      {/* Header */}
      <button
        onClick={() => setExpanded(!expanded)}
        className="flex w-full items-center gap-3 p-4 text-left hover:bg-muted/50 transition-colors"
      >
        <span
          className={cn(
            "text-muted-foreground transition-transform text-sm",
            expanded && "rotate-90"
          )}
        >
          ▶
        </span>
        <span className="font-semibold text-sm flex-1">{head.name}</span>
        <span
          className={cn(
            "h-2 w-2 rounded-full shrink-0",
            head.status === "connected" ? "bg-green-500" : "bg-red-500"
          )}
        />
      </button>

      {expanded && (
        <div className="px-4 pb-4 space-y-3">
          {/* Head weight slider */}
          {showHeadWeight && (
            <div className="space-y-1">
              <div className="flex justify-between text-xs text-muted-foreground">
                <span>Head weight</span>
                <span>{head.weight}</span>
              </div>
              <Slider
                min={1}
                max={100}
                value={head.weight}
                onChange={onHeadWeightChange}
              />
            </div>
          )}

          {/* Server URL */}
          <p className="text-xs text-muted-foreground">{head.url}</p>

          {/* Leaf cards */}
          <div className="space-y-2">
            {head.leafs.map((leaf) => (
              <LeafCard
                key={leaf.id}
                leaf={leaf}
                showWeightSlider={showLeafWeights}
                containerStatus={containerStatus}
                onToggle={(enabled) => onLeafToggle(leaf.slug, enabled)}
                onWeightChange={(weight) => onLeafWeightChange(leaf.slug, weight)}
                dashboardUrl={head.url || undefined}
                volunteerId={head.volunteer_id}
              />
            ))}
          </div>

          {/* Actions */}
          <div className="flex items-center justify-between pt-1">
            <Button variant="outline" size="sm" onClick={onResetDefaults}>
              Use Defaults
            </Button>
            {confirmDetach ? (
              <div className="flex gap-1">
                <Button
                  variant="destructive"
                  size="sm"
                  onClick={() => {
                    onDetach();
                    setConfirmDetach(false);
                  }}
                >
                  Confirm
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setConfirmDetach(false)}
                >
                  Cancel
                </Button>
              </div>
            ) : (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setConfirmDetach(true)}
              >
                Detach
              </Button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
