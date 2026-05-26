"use client";

import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Switch } from "@/components/ui/switch";

interface LeafInfo {
  id: string;
  name: string;
  description: string;
  state: string;
  queued_work_units: number;
  execution_spec?: {
    binaries?: Record<string, string>;
    gpu_required?: boolean;
    gpu_type?: string;
  };
}

interface HeadInfoResponse {
  name: string;
  leafs: LeafInfo[];
}

interface LeafSelectorProps {
  serverUrl: string;
  selectedIds: Set<string>;
  onSelectionChange: (ids: Set<string>) => void;
  disabled?: boolean;
}

export function LeafSelector({
  serverUrl,
  selectedIds,
  onSelectionChange,
  disabled,
}: LeafSelectorProps) {
  const [leafs, setLeafs] = useState<LeafInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    async function fetchLeafs() {
      try {
        setLoading(true);
        setError(null);
        const resp = await fetch(`${serverUrl}/api/v1/head`);
        if (!resp.ok) {
          throw new Error(`Failed to fetch head info: ${resp.status}`);
        }
        const data: HeadInfoResponse = await resp.json();
        const wasmLeafs = (data.leafs || []).filter(
          (l) =>
            l.state === "ACTIVE" &&
            l.execution_spec?.binaries?.wasm &&
            l.queued_work_units > 0
        );
        setLeafs(wasmLeafs);
      } catch (err) {
        setError(
          err instanceof Error ? err.message : "Failed to load leafs"
        );
      } finally {
        setLoading(false);
      }
    }
    fetchLeafs();
  }, [serverUrl]);

  function toggleLeaf(id: string) {
    const next = new Set(selectedIds);
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    onSelectionChange(next);
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Available Leafs</CardTitle>
      </CardHeader>
      <CardContent>
        {loading && (
          <p className="text-sm text-muted-foreground">Loading leafs...</p>
        )}
        {error && <p className="text-sm text-destructive">{error}</p>}
        {!loading && !error && leafs.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No WASM leafs are currently active on this server.
          </p>
        )}
        {!loading && !error && leafs.length > 0 && (
          <div className="space-y-3">
            {leafs.map((leaf) => (
              <div
                key={leaf.id}
                className="flex items-start justify-between gap-3 rounded-md border p-3"
              >
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="font-medium text-sm">{leaf.name}</span>
                    {leaf.execution_spec?.gpu_required && (
                      <Badge variant="secondary">GPU</Badge>
                    )}
                  </div>
                  {leaf.description && (
                    <p className="mt-1 text-xs text-muted-foreground line-clamp-2">
                      {leaf.description}
                    </p>
                  )}
                  <p className="mt-1 text-xs text-muted-foreground">
                    {leaf.queued_work_units} queued work units
                  </p>
                </div>
                <Switch
                  checked={selectedIds.has(leaf.id)}
                  onCheckedChange={() => toggleLeaf(leaf.id)}
                  disabled={disabled}
                  aria-label={`Enable ${leaf.name}`}
                />
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
