import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { invoke } from "@tauri-apps/api/core";
import { useClient } from "@/hooks/use-api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ApiError, type HeadPreview } from "@/api/client";

interface AddServerDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onServerAdded: () => void;
}

export function AddServerDialog({
  open,
  onOpenChange,
  onServerAdded,
}: AddServerDialogProps) {
  const { client } = useClient();
  const [url, setUrl] = useState("");
  const [customName, setCustomName] = useState("");
  const [isTesting, setIsTesting] = useState(false);
  const [testError, setTestError] = useState<string | null>(null);
  const [headPreview, setHeadPreview] = useState<HeadPreview | null>(null);
  const [isAttaching, setIsAttaching] = useState(false);
  const [attachError, setAttachError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (open) {
      setUrl("");
      setCustomName("");
      setIsTesting(false);
      setTestError(null);
      setHeadPreview(null);
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

      try {
        const headData = await invoke<{ name: string; description: string; leafs: Array<{ slug: string; name: string; research_area: string; state: string }> }>("fetch_head_info", { url: url.trim() });
        setHeadPreview({
          name: headData.name ?? "",
          description: headData.description ?? "",
          leafs: (headData.leafs ?? []).filter(
            (l) => l.state === "ACTIVE"
          ),
        });
      } catch {
        // Head info not available, proceed without preview
      }
    } catch {
      setTestError("Connection failed. Check the URL and try again.");
    } finally {
      setIsTesting(false);
    }
  }, [url]);

  const handleAttach = useCallback(async () => {
    if (!client) return;
    setIsAttaching(true);
    setAttachError(null);
    try {
      await client.attachHead({
        server_address: url.trim(),
        name: customName.trim() || undefined,
      });
      onOpenChange(false);
      onServerAdded();
    } catch (err) {
      setAttachError(
        err instanceof ApiError ? err.message : "Failed to attach server"
      );
    } finally {
      setIsAttaching(false);
    }
  }, [client, url, customName, onOpenChange, onServerAdded]);

  if (!open) return null;

  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
      onClick={(e) => {
        if (e.target === e.currentTarget) onOpenChange(false);
      }}
    >
      <div className="w-full max-w-[480px] rounded-lg border bg-background p-6 shadow-lg space-y-4 mx-4">
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
                {headPreview.leafs.map((leaf) => (
                  <div key={leaf.slug} className="flex items-center gap-2 text-xs">
                    <span>{leaf.name}</span>
                    <span className="inline-flex items-center rounded-full bg-secondary px-1.5 py-0.5 text-[10px]">
                      {leaf.research_area}
                    </span>
                  </div>
                ))}
              </div>
            )}

            <Input
              placeholder="Display name (optional)"
              value={customName}
              onChange={(e) => setCustomName(e.target.value)}
              className="mt-2"
            />
          </div>
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
