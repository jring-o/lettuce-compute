import { useCallback, useEffect, useState } from "react";
import { useHeads, useWriteLeafPreferences, useDebouncedHeadWeight, useDebouncedLeafWeight } from "@/hooks/use-heads";
import { useClient } from "@/hooks/use-api";
import { useContainerRuntime } from "@/hooks/use-container-runtime";
import { HeadSection } from "@/components/heads/head-section";
import { AddServerDialog } from "@/components/heads/add-server-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { ApiError } from "@/api/client";

function SkeletonCard() {
  return (
    <Card>
      <CardContent className="p-4 space-y-3">
        <div className="h-4 w-2/3 animate-pulse rounded bg-muted" />
        <div className="h-3 w-1/2 animate-pulse rounded bg-muted" />
        <div className="h-8 w-full animate-pulse rounded bg-muted" />
      </CardContent>
    </Card>
  );
}

export function ProjectsPage() {
  const { heads, isLoading, error, refetch, setHeads } = useHeads();
  const { write: writeLeafPrefs } = useWriteLeafPreferences();
  const { write: writeHeadWeight } = useDebouncedHeadWeight();
  const { write: writeLeafWeight } = useDebouncedLeafWeight();
  const { client } = useClient();
  const { status: containerStatus } = useContainerRuntime();
  const [toast, setToast] = useState<string | null>(null);
  const [toastType, setToastType] = useState<"error" | "warning">("error");
  const [addDialogOpen, setAddDialogOpen] = useState(false);

  useEffect(() => {
    if (toast) {
      const id = setTimeout(() => setToast(null), 3000);
      return () => clearTimeout(id);
    }
  }, [toast]);

  const handleDetach = useCallback(
    async (headName: string) => {
      if (!client) return;
      try {
        await client.detachHead({ server_name: headName });
        refetch();
      } catch (err) {
        setToastType("error");
        setToast(err instanceof ApiError ? err.message : "Failed to detach server");
      }
    },
    [client, refetch]
  );

  const handleLeafToggle = useCallback(
    (headName: string, leafSlug: string, enabled: boolean) => {
      const head = heads.find((h) => h.name === headName);
      if (!head) return;

      // Warn if enabling a container leaf without a running container runtime
      if (enabled) {
        const leaf = head.leafs.find((l) => l.slug === leafSlug);
        if (leaf?.execution_spec?.image && containerStatus?.status !== "running") {
          setToastType("warning");
          setToast("This leaf requires a container runtime. Go to Settings to set up Podman.");
        }
      }

      // Update local state immediately — UI is source of truth
      setHeads((prev) =>
        prev.map((h) =>
          h.name === headName
            ? { ...h, leafs: h.leafs.map((l) => l.slug === leafSlug ? { ...l, enabled } : l) }
            : h
        )
      );

      // Write to config immediately (no debounce for discrete actions)
      const enabledSlugs = head.leafs
        .filter((l) => (l.slug === leafSlug ? enabled : l.enabled))
        .map((l) => l.slug);

      const prefs = enabledSlugs.length === head.leafs.length
        ? { mode: "ALL" as const }
        : { mode: "SPECIFIC" as const, enabled: enabledSlugs };

      writeLeafPrefs(headName, prefs).catch(() => {
        // Roll back on failure
        setHeads((prev) =>
          prev.map((h) =>
            h.name === headName
              ? { ...h, leafs: h.leafs.map((l) => l.slug === leafSlug ? { ...l, enabled: !enabled } : l) }
              : h
          )
        );
        setToastType("error");
        setToast("Error: Failed to save leaf preference");
      });
    },
    [heads, setHeads, writeLeafPrefs, containerStatus]
  );

  const handleLeafWeightChange = useCallback(
    (headName: string, leafSlug: string, weight: number) => {
      // Update local state immediately
      setHeads((prev) =>
        prev.map((h) =>
          h.name === headName
            ? { ...h, leafs: h.leafs.map((l) => l.slug === leafSlug ? { ...l, effective_weight: weight } : l) }
            : h
        )
      );

      // Debounced write for continuous slider input
      writeLeafWeight(headName, leafSlug, weight, heads);
    },
    [heads, setHeads, writeLeafWeight]
  );

  const handleResetDefaults = useCallback(
    (headName: string) => {
      // Update local state immediately
      setHeads((prev) =>
        prev.map((h) =>
          h.name === headName
            ? { ...h, leafs: h.leafs.map((l) => ({ ...l, enabled: true, effective_weight: 100 })) }
            : h
        )
      );
      writeLeafPrefs(headName, { mode: "ALL" });
    },
    [setHeads, writeLeafPrefs]
  );

  const showHeadWeight = heads.length >= 2;

  return (
    <div className="p-6 space-y-4 max-w-3xl mx-auto">
      {/* Toast */}
      {toast && (
        <div
          className={
            toastType === "warning"
              ? "fixed top-4 right-4 z-50 rounded-md px-4 py-2 text-sm font-medium shadow-lg bg-yellow-100 text-yellow-800 border border-yellow-300"
              : "fixed top-4 right-4 z-50 rounded-md px-4 py-2 text-sm font-medium shadow-lg bg-destructive text-destructive-foreground"
          }
        >
          {toast}
        </div>
      )}

      {/* Loading state */}
      {isLoading && heads.length === 0 && (
        <div className="space-y-4">
          <SkeletonCard />
          <SkeletonCard />
        </div>
      )}

      {/* Empty state */}
      {!isLoading && heads.length === 0 && (
        <p className="text-sm text-muted-foreground py-8 text-center">
          No servers connected. Add a server to start contributing.
        </p>
      )}

      {/* Error state */}
      {error && (
        <p className="text-sm text-destructive py-4 text-center">
          Failed to load servers: {error.message}
        </p>
      )}

      {/* Head sections */}
      {heads.map((head) => (
        <HeadSection
          key={head.grpc_address}
          head={head}
          showHeadWeight={showHeadWeight}
          containerStatus={containerStatus}
          onHeadWeightChange={(weight) => {
            setHeads((prev) =>
              prev.map((h) => h.name === head.name ? { ...h, weight } : h)
            );
            writeHeadWeight(head.name, weight);
          }}
          onLeafToggle={(slug, enabled) =>
            handleLeafToggle(head.name, slug, enabled)
          }
          onLeafWeightChange={(slug, weight) =>
            handleLeafWeightChange(head.name, slug, weight)
          }
          onResetDefaults={() => handleResetDefaults(head.name)}
          onDetach={() => handleDetach(head.name)}
        />
      ))}

      {/* Add Server button */}
      <Button
        variant="outline"
        className="w-full"
        onClick={() => setAddDialogOpen(true)}
      >
        + Add Server
      </Button>

      <AddServerDialog
        open={addDialogOpen}
        onOpenChange={setAddDialogOpen}
        onServerAdded={refetch}
      />
    </div>
  );
}
