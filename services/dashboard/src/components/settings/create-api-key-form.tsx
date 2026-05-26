"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertTitle, AlertDescription } from "@/components/ui/alert";
import { createApiKey } from "@/lib/actions/api-keys";

export function CreateApiKeyForm() {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [plaintextKey, setPlaintextKey] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [isPending, startTransition] = useTransition();

  function handleCreate() {
    setError(null);
    startTransition(async () => {
      const result = await createApiKey(name);
      if ("error" in result) {
        setError(result.error.message);
        return;
      }
      setPlaintextKey(result.data.plaintextKey);
      setName("");
    });
  }

  function handleDismiss() {
    setPlaintextKey(null);
    setOpen(false);
    setCopied(false);
    router.refresh();
  }

  async function handleCopy() {
    if (plaintextKey) {
      await navigator.clipboard.writeText(plaintextKey);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  }

  if (plaintextKey) {
    return (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
        <div className="mx-4 w-full max-w-lg rounded-lg border bg-background p-6">
          <h3 className="text-lg font-semibold">API Key Created</h3>
          <Alert variant="destructive" className="mt-3">
            <AlertTitle>Copy this key now</AlertTitle>
            <AlertDescription>
              This key will not be shown again. Store it securely.
            </AlertDescription>
          </Alert>
          <div className="mt-4 flex gap-2">
            <code className="flex-1 overflow-x-auto rounded border bg-muted px-3 py-2 text-sm font-mono">
              {plaintextKey}
            </code>
            <Button variant="outline" onClick={handleCopy}>
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <div className="mt-4 flex justify-end">
            <Button onClick={handleDismiss}>Done</Button>
          </div>
        </div>
      </div>
    );
  }

  if (!open) {
    return (
      <Button onClick={() => setOpen(true)} size="sm">
        Create API Key
      </Button>
    );
  }

  return (
    <div className="flex items-end gap-2">
      <div>
        <Label htmlFor="key-name">Key name</Label>
        <Input
          id="key-name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. My Dev Key"
          className="mt-1 w-48"
          maxLength={100}
        />
      </div>
      {error && <p className="text-sm text-destructive">{error}</p>}
      <Button onClick={handleCreate} disabled={isPending || !name.trim()} size="sm">
        {isPending ? "Creating..." : "Create"}
      </Button>
      <Button
        variant="ghost"
        size="sm"
        onClick={() => {
          setOpen(false);
          setName("");
          setError(null);
        }}
      >
        Cancel
      </Button>
    </div>
  );
}
