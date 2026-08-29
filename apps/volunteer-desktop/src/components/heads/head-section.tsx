import { useState } from "react";
import { AlertTriangle } from "lucide-react";
import { Slider } from "@/components/ui/slider";
import { Button } from "@/components/ui/button";
import { LeafCard } from "./leaf-card";
import {
  RuntimeTrustFields,
  choiceFromTrustedRuntimes,
  trustedRuntimesFromChoice,
  type RuntimeTrustChoice,
} from "./runtime-trust-fields";
import { cn, formatAge } from "@/lib/utils";
import type { HeadInfo, ContainerRuntimeStatus, MachineCapabilities } from "@/api/client";

interface HeadSectionProps {
  head: HeadInfo;
  showHeadWeight: boolean;
  containerStatus: ContainerRuntimeStatus | null;
  /** This machine's capabilities from the running daemon; null until loaded. */
  machine: MachineCapabilities | null;
  /** The head's `trusted_runtimes` from config (uppercase); null when not known. */
  trustedRuntimes: string[] | null;
  onHeadWeightChange: (weight: number) => void;
  onLeafToggle: (leafSlug: string, enabled: boolean) => void;
  onLeafWeightChange: (leafSlug: string, weight: number) => void;
  onResetDefaults: () => void;
  onDetach: () => void;
  /** Save a new `trusted_runtimes` list for this head. Rejects on failure. */
  onTrustChange: (trustedRuntimes: string[]) => Promise<void>;
  /** Raise `resource_limits.max_disk_gb` to the given whole-GB value. */
  onRaiseDisk?: (gb: number) => Promise<void>;
}

function TrustMark({ allowed }: { allowed: boolean }) {
  return (
    <span className={allowed ? "text-green-600" : "text-muted-foreground"}>
      {allowed ? "✓" : "✗"}
    </span>
  );
}

export function HeadSection({
  head,
  showHeadWeight,
  containerStatus,
  machine,
  trustedRuntimes,
  onHeadWeightChange,
  onLeafToggle,
  onLeafWeightChange,
  onResetDefaults,
  onDetach,
  onTrustChange,
  onRaiseDisk,
}: HeadSectionProps) {
  const [expanded, setExpanded] = useState(true);
  const [confirmDetach, setConfirmDetach] = useState(false);
  const [editingTrust, setEditingTrust] = useState(false);
  const [trustDraft, setTrustDraft] = useState<RuntimeTrustChoice>({ container: false, native: false });
  const [savingTrust, setSavingTrust] = useState(false);
  const [trustError, setTrustError] = useState<string | null>(null);

  const enabledLeafCount = head.leafs.filter((l) => l.enabled).length;
  const showLeafWeights = enabledLeafCount >= 2;
  const trust = choiceFromTrustedRuntimes(trustedRuntimes ?? []);
  const containerAvailable = machine === null ? true : machine.runtimes.includes("container");

  const openTrustEditor = () => {
    setTrustDraft(trust);
    setTrustError(null);
    setEditingTrust(true);
  };

  const saveTrust = async () => {
    setSavingTrust(true);
    setTrustError(null);
    try {
      await onTrustChange(trustedRuntimesFromChoice(trustDraft));
      setEditingTrust(false);
    } catch (err) {
      setTrustError(err instanceof Error ? err.message : "Could not save trust settings");
    } finally {
      setSavingTrust(false);
    }
  };

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
        {head.head_version && (
          <span className="text-xs text-muted-foreground">head {head.head_version}</span>
        )}
        <span
          data-testid="connection-dot"
          title={head.status === "connected" ? "Connected" : "Not connected"}
          className={cn(
            "h-2 w-2 rounded-full shrink-0",
            head.status === "connected" ? "bg-green-500" : "bg-red-500"
          )}
        />
      </button>

      {head.update_required && (
        <div
          role="alert"
          className="mx-4 mb-3 flex items-center gap-2 rounded-md border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-800 dark:border-red-800 dark:bg-red-950 dark:text-red-200"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" />
          <span>This app is too old for this head. Update Lettuce Compute to keep contributing here.</span>
        </div>
      )}

      {expanded && (
        <div className="px-4 pb-4 space-y-3">
          {/* Server URL */}
          {head.url && <p className="text-xs text-muted-foreground">{head.url}</p>}

          {/* Runtime trust */}
          <div className="space-y-2">
            <div className="flex items-center gap-2 flex-wrap text-xs">
              <span className="text-muted-foreground">May run:</span>
              <span className="inline-flex items-center gap-1 rounded-full border px-2 py-0.5">
                WASM <TrustMark allowed />
              </span>
              <span className="inline-flex items-center gap-1 rounded-full border px-2 py-0.5">
                Container <TrustMark allowed={trust.container} />
              </span>
              <span className="inline-flex items-center gap-1 rounded-full border px-2 py-0.5">
                Native <TrustMark allowed={trust.native} />
              </span>
              {trustedRuntimes === null && (
                <span className="text-muted-foreground">(trust settings not loaded)</span>
              )}
              {!editingTrust && (
                <button
                  onClick={openTrustEditor}
                  className="text-blue-600 hover:underline"
                >
                  Change...
                </button>
              )}
            </div>

            {editingTrust && (
              <div className="rounded-md border p-3 space-y-3">
                <RuntimeTrustFields
                  headName={head.name}
                  value={trustDraft}
                  onChange={setTrustDraft}
                  containerAvailable={containerAvailable}
                  disabled={savingTrust}
                />
                {trustError && <p className="text-xs text-destructive">{trustError}</p>}
                <div className="flex justify-end gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setEditingTrust(false)}
                    disabled={savingTrust}
                  >
                    Cancel
                  </Button>
                  <Button size="sm" onClick={saveTrust} disabled={savingTrust}>
                    {savingTrust ? "Saving..." : "Save trust settings"}
                  </Button>
                </div>
              </div>
            )}
          </div>

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

          {/* Freshness of the leaf figures */}
          <p className="text-xs text-muted-foreground">
            {head.leafs_refreshed_at
              ? `Leaf figures as of ${formatAge(head.leafs_refreshed_at)}`
              : "Leaf figures not fetched yet"}
            {" — they refresh only when Lettuce last tried to fetch work."}
          </p>

          {/* Leaf cards */}
          <div className="space-y-2">
            {head.leafs.map((leaf) => (
              <LeafCard
                key={leaf.id}
                leaf={leaf}
                showWeightSlider={showLeafWeights}
                containerStatus={containerStatus}
                machine={machine}
                trustedRuntimes={trustedRuntimes}
                onToggle={(enabled) => onLeafToggle(leaf.slug, enabled)}
                onWeightChange={(weight) => onLeafWeightChange(leaf.slug, weight)}
                onRaiseDisk={onRaiseDisk}
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
