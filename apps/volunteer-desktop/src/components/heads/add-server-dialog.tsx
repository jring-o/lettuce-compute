import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { invoke } from "@tauri-apps/api/core";
import { useClient } from "@/hooks/use-api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ApiError, type HeadPreview, type MachineCapabilities } from "@/api/client";
import {
  RuntimeTrustFields,
  trustedRuntimesFromChoice,
  type RuntimeTrustChoice,
} from "./runtime-trust-fields";

interface AddServerDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Called after a successful attach, with the name the head will show under. */
  onServerAdded: (headName: string) => void;
  /** This machine's capabilities; decides whether container trust can be offered. */
  machine: MachineCapabilities | null;
}

/** Shape of the `fetch_head_info` command's result (the head's public `GET /api/v1/head`). */
interface RawHeadInfo {
  name?: string;
  description?: string;
  leafs?: Array<{
    slug: string;
    name: string;
    research_area?: string | string[] | null;
    state?: string;
  }>;
}

function researchAreas(value: string | string[] | null | undefined): string[] {
  if (Array.isArray(value)) return value.filter((v) => typeof v === "string" && v !== "");
  return typeof value === "string" && value !== "" ? [value] : [];
}

export function AddServerDialog({
  open,
  onOpenChange,
  onServerAdded,
  machine,
}: AddServerDialogProps) {
  const { client } = useClient();
  const [url, setUrl] = useState("");
  const [customName, setCustomName] = useState("");
  const [isTesting, setIsTesting] = useState(false);
  const [testError, setTestError] = useState<string | null>(null);
  const [headPreview, setHeadPreview] = useState<HeadPreview | null>(null);
  const [trust, setTrust] = useState<RuntimeTrustChoice>({ container: false, native: false });
  const [isAttaching, setIsAttaching] = useState(false);
  const [attachError, setAttachError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const containerAvailable = machine === null ? true : machine.runtimes.includes("container");

  useEffect(() => {
    if (open) {
      setUrl("");
      setCustomName("");
      setIsTesting(false);
      setTestError(null);
      setHeadPreview(null);
      setTrust({ container: false, native: false });
      setIsAttaching(false);
      setAttachError(null);
      setTimeout(() => inputRef.current?.focus(), 100);
    }
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onOpenChange(false);
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [open, onOpenChange]);

  const testConnection = useCallback(async () => {
    if (!url.trim()) return;
    setIsTesting(true);
    setTestError(null);
    setHeadPreview(null);
    try {
      await invoke<{ status: string }>("test_server_connection", { url: url.trim() });

      let preview: HeadPreview = { name: url.trim(), description: "", leafs: [] };
      try {
        const headData = await invoke<RawHeadInfo>("fetch_head_info", { url: url.trim() });
        preview = {
          name: headData.name || url.trim(),
          description: headData.description ?? "",
          leafs: (headData.leafs ?? [])
            .filter((l) => !l.state || l.state === "ACTIVE")
            .map((l) => ({ slug: l.slug, name: l.name, research_area: researchAreas(l.research_area) })),
        };
      } catch {
        // The head answered the health check but not the public head info;
        // attaching still works, so proceed with a bare preview.
      }
      setHeadPreview(preview);
      // Container is the safe default when a backend exists; native never is.
      setTrust({ container: containerAvailable, native: false });
    } catch {
      setTestError("Connection failed. Check the URL and try again.");
    } finally {
      setIsTesting(false);
    }
  }, [url, containerAvailable]);

  const handleAttach = useCallback(async () => {
    if (!client) return;
    setIsAttaching(true);
    setAttachError(null);
    const name = customName.trim() || undefined;
    try {
      await client.attachHead({
        server_address: url.trim(),
        name,
        trusted_runtimes: trustedRuntimesFromChoice({
          container: trust.container && containerAvailable,
          native: trust.native,
        }),
      });
      onOpenChange(false);
      onServerAdded(name || headPreview?.name || url.trim());
    } catch (err) {
      setAttachError(
        err instanceof ApiError ? err.message : "Failed to attach server"
      );
    } finally {
      setIsAttaching(false);
    }
  }, [client, url, customName, trust, containerAvailable, headPreview, onOpenChange, onServerAdded]);

  if (!open) return null;

  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
      onClick={(e) => {
        if (e.target === e.currentTarget) onOpenChange(false);
      }}
    >
      <div className="w-full max-w-[480px] max-h-[90vh] overflow-y-auto rounded-lg border bg-background p-6 shadow-lg space-y-4 mx-4">
        <h2 className="text-lg font-semibold">Add Server</h2>

        <div className="space-y-3">
          <Input
            ref={inputRef}
            placeholder="https://compute.example.org"
            value={url}
            onChange={(e) => {
              setUrl(e.target.value);
              setTestError(null);
              setHeadPreview(null);
            }}
          />
          <Button
            variant="outline"
            size="sm"
            onClick={testConnection}
            disabled={!url.trim() || isTesting}
          >
            {isTesting ? "Testing..." : "Test Connection"}
          </Button>
        </div>

        {testError && (
          <p className="text-sm text-destructive">{testError}</p>
        )}

        {headPreview && (
          <>
            <div className="space-y-2 rounded-md border p-3">
              <p className="font-medium text-sm">{headPreview.name}</p>
              {headPreview.description && (
                <p className="text-xs text-muted-foreground line-clamp-2">
                  {headPreview.description}
                </p>
              )}
              {headPreview.leafs.length > 0 && (
                <div className="space-y-1">
                  <p className="text-xs font-medium text-muted-foreground">
                    Active leafs:
                  </p>
                  {headPreview.leafs.map((leaf) => {
                    const areas = researchAreas(leaf.research_area);
                    return (
                      <div key={leaf.slug} className="flex items-center gap-2 text-xs">
                        <span>{leaf.name}</span>
                        {areas.length > 0 && (
                          <span className="inline-flex items-center rounded-full bg-secondary px-1.5 py-0.5 text-[10px]">
                            {areas.join(", ")}
                          </span>
                        )}
                      </div>
                    );
                  })}
                </div>
              )}

              <Input
                placeholder="Display name (optional)"
                value={customName}
                onChange={(e) => setCustomName(e.target.value)}
                className="mt-2"
              />
            </div>

            <div className="rounded-md border p-3">
              <RuntimeTrustFields
                headName={customName.trim() || headPreview.name}
                value={trust}
                onChange={setTrust}
                containerAvailable={containerAvailable}
                disabled={isAttaching}
              />
            </div>
          </>
        )}

        {attachError && (
          <p className="text-sm text-destructive">{attachError}</p>
        )}

        <div className="flex justify-end gap-2">
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          {headPreview && (
            <Button onClick={handleAttach} disabled={isAttaching}>
              {isAttaching ? "Attaching..." : "Attach"}
            </Button>
          )}
        </div>
      </div>
    </div>,
    document.body
  );
}
